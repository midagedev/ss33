// Package sigv4 signs and verifies AWS Signature Version 4 requests for the S3 service.
//
// Verification covers the two forms S3 clients send: the Authorization header and the presigned
// query string. Payload hashes are taken from the client as declared (x-amz-content-sha256); the
// body itself is not re-hashed — ss33 is a local store, and buffering every upload to check it
// would cost more than the check is worth.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Algorithm       = "AWS4-HMAC-SHA256"
	UnsignedPayload = "UNSIGNED-PAYLOAD"
	TimeFormat      = "20060102T150405Z"
	dateFormat      = "20060102"
)

type Credentials struct {
	AccessKey string
	SecretKey string
}

var (
	ErrMissingAuth       = errors.New("missing authentication")
	ErrMalformed         = errors.New("malformed authorization")
	ErrUnknownAccessKey  = errors.New("unknown access key")
	ErrSignatureMismatch = errors.New("signature does not match")
	ErrExpired           = errors.New("request has expired")
	ErrUnsignedHeaders   = errors.New("there were headers present in the request which were not signed")
)

// IsPresigned reports whether the request carries query-string authentication.
func IsPresigned(r *http.Request) bool {
	return r.URL.Query().Get("X-Amz-Algorithm") != ""
}

// HasAuth reports whether the request carries any SigV4 authentication.
func HasAuth(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || IsPresigned(r)
}

// Verify checks the request signature against creds. now is injectable for tests.
func Verify(r *http.Request, creds Credentials, now time.Time) error {
	if IsPresigned(r) {
		return verifyPresigned(r, creds, now)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ErrMissingAuth
	}
	return verifyHeader(r, auth, creds)
}

type parsedAuth struct {
	accessKey     string
	scope         string // date/region/service/aws4_request
	date          string
	signedHeaders []string
	signature     string
}

func parseCredential(cred string) (accessKey, scope, date string, err error) {
	parts := strings.SplitN(cred, "/", 2)
	if len(parts) != 2 {
		return "", "", "", ErrMalformed
	}
	scopeParts := strings.Split(parts[1], "/")
	if len(scopeParts) != 4 || scopeParts[3] != "aws4_request" {
		return "", "", "", ErrMalformed
	}
	return parts[0], parts[1], scopeParts[0], nil
}

func verifyHeader(r *http.Request, auth string, creds Credentials) error {
	if !strings.HasPrefix(auth, Algorithm+" ") {
		return ErrMalformed
	}
	var p parsedAuth
	for _, field := range strings.Split(strings.TrimPrefix(auth, Algorithm+" "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(field), "=")
		if !ok {
			return ErrMalformed
		}
		switch k {
		case "Credential":
			var err error
			if p.accessKey, p.scope, p.date, err = parseCredential(v); err != nil {
				return err
			}
		case "SignedHeaders":
			p.signedHeaders = strings.Split(v, ";")
		case "Signature":
			p.signature = v
		}
	}
	if p.accessKey == "" || len(p.signedHeaders) == 0 || p.signature == "" {
		return ErrMalformed
	}
	if p.accessKey != creds.AccessKey {
		return ErrUnknownAccessKey
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		amzDate = r.Header.Get("Date")
	}
	// S3 rejects x-amz-* headers left out of the signature; accepting them would hide client signing bugs.
	signed := map[string]bool{}
	for _, h := range p.signedHeaders {
		signed[h] = true
	}
	for name := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") && !signed[lower] {
			return ErrUnsignedHeaders
		}
	}
	payload := r.Header.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = UnsignedPayload
	}
	query := canonicalQuery(rawQuery(r), "")
	return matchSignature(r, p, amzDate, query, payload, creds)
}

func verifyPresigned(r *http.Request, creds Credentials, now time.Time) error {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != Algorithm {
		return ErrMalformed
	}
	var p parsedAuth
	var err error
	if p.accessKey, p.scope, p.date, err = parseCredential(q.Get("X-Amz-Credential")); err != nil {
		return err
	}
	if p.accessKey != creds.AccessKey {
		return ErrUnknownAccessKey
	}
	p.signedHeaders = strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	p.signature = q.Get("X-Amz-Signature")
	amzDate := q.Get("X-Amz-Date")
	signedAt, err := time.Parse(TimeFormat, amzDate)
	if err != nil {
		return ErrMalformed
	}
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires < 0 {
		return ErrMalformed
	}
	if now.After(signedAt.Add(time.Duration(expires) * time.Second)) {
		return ErrExpired
	}
	payload := q.Get("X-Amz-Content-Sha256")
	if payload == "" {
		payload = UnsignedPayload
	}
	query := canonicalQuery(rawQuery(r), "X-Amz-Signature")
	return matchSignature(r, p, amzDate, query, payload, creds)
}

// matchSignature tries the path exactly as sent first, then the canonical re-encoding of the decoded
// path — clients that send a path in a different (still valid) percent-encoding than the one they signed
// would otherwise fail with a confusing mismatch.
func matchSignature(r *http.Request, p parsedAuth, amzDate, query, payload string, creds Credentials) error {
	headers := canonicalHeaders(r, p.signedHeaders)
	if headers == "" {
		return ErrMalformed
	}
	for _, uri := range candidateURIs(r) {
		creq := strings.Join([]string{r.Method, uri, query, headers, strings.Join(p.signedHeaders, ";"), payload}, "\n")
		if hmac.Equal([]byte(signature(creds.SecretKey, p.scope, p.date, amzDate, creq)), []byte(p.signature)) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

func candidateURIs(r *http.Request) []string {
	raw := r.RequestURI
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if u, err := url.Parse(raw); err == nil && u.IsAbs() {
		raw = u.EscapedPath()
	}
	if raw == "" {
		raw = "/"
	}
	encoded := URIEncode(r.URL.Path, false)
	if encoded == "" {
		encoded = "/"
	}
	if encoded == raw {
		return []string{raw}
	}
	return []string{raw, encoded}
}

func signature(secret, scope, date, amzDate, canonicalRequest string) string {
	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{Algorithm, amzDate, scope, hex.EncodeToString(hash[:])}, "\n")
	return hex.EncodeToString(hmacSHA256(signingKey(secret, scope, date), stringToSign))
}

// signingKeys caches derived keys: they change once a day per secret and region, but deriving one costs
// four HMACs on every request.
var signingKeys sync.Map // secret + "\x00" + scope -> []byte

func signingKey(secret, scope, date string) []byte {
	id := secret + "\x00" + scope
	if k, ok := signingKeys.Load(id); ok {
		return k.([]byte)
	}
	scopeParts := strings.Split(scope, "/")
	key := hmacSHA256([]byte("AWS4"+secret), date)
	key = hmacSHA256(key, scopeParts[1])
	key = hmacSHA256(key, scopeParts[2])
	key = hmacSHA256(key, "aws4_request")
	signingKeys.Store(id, key)
	return key
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func rawQuery(r *http.Request) string {
	if _, q, ok := strings.Cut(r.RequestURI, "?"); ok {
		return q
	}
	return r.URL.RawQuery
}

// canonicalQuery decodes every pair and re-encodes it the way SigV4 requires (RFC 3986, %20 for space),
// sorted by key then value. Pairs are decoded with PathUnescape so a literal '+' stays '+'.
func canonicalQuery(raw, skip string) string {
	if raw == "" {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		dk, err := url.PathUnescape(k)
		if err != nil {
			dk = k
		}
		if dk == skip {
			continue
		}
		dv, err := url.PathUnescape(v)
		if err != nil {
			dv = v
		}
		pairs = append(pairs, pair{URIEncode(dk, true), URIEncode(dv, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

func canonicalHeaders(r *http.Request, signed []string) string {
	var b strings.Builder
	for _, name := range signed {
		var values []string
		if name == "host" {
			values = []string{r.Host}
		} else {
			values = r.Header.Values(name)
		}
		if len(values) == 0 && name != "content-length" {
			return ""
		}
		if name == "content-length" && len(values) == 0 {
			values = []string{strconv.FormatInt(r.ContentLength, 10)}
		}
		for i, v := range values {
			values[i] = strings.Join(strings.Fields(v), " ")
		}
		b.WriteString(name + ":" + strings.Join(values, ",") + "\n")
	}
	return b.String()
}

// URIEncode encodes per SigV4: unreserved characters stay, everything else becomes %XX (uppercase).
func URIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Sign adds header authentication to req with an unsigned payload. req.URL must already carry the
// encoded path the server will see (build it with URIEncode).
func Sign(req *http.Request, creds Credentials, region string, now time.Time) {
	amzDate := now.UTC().Format(TimeFormat)
	date := now.UTC().Format(dateFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", UnsignedPayload)
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	signed := []string{"host"} // S3 requires every x-amz-* header to be signed (x-amz-copy-source, ...)
	for name := range req.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") {
			signed = append(signed, lower)
		}
	}
	sort.Strings(signed)
	uri := req.URL.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	scope := date + "/" + region + "/s3/aws4_request"
	creq := strings.Join([]string{req.Method, uri, canonicalQuery(req.URL.RawQuery, ""), canonicalHeaders(req, signed), strings.Join(signed, ";"), UnsignedPayload}, "\n")
	sig := signature(creds.SecretKey, scope, date, amzDate, creq)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, creds.AccessKey, scope, strings.Join(signed, ";"), sig))
}
