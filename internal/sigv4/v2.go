package sigv4

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SigV2 query authentication (AWSAccessKeyId, Expires, Signature) is what boto3's generate_presigned_url
// produces unless the client is configured for s3v4. Only the presigned form is accepted; SigV2 headers
// are not.

// v2Subresources are the query parameters that are part of the SigV2 canonical resource.
var v2Subresources = map[string]bool{
	"acl": true, "cors": true, "delete": true, "lifecycle": true, "location": true, "logging": true,
	"notification": true, "partNumber": true, "policy": true, "requestPayment": true, "restore": true,
	"tagging": true, "torrent": true, "uploadId": true, "uploads": true, "versionId": true, "versioning": true,
	"versions": true, "website": true, "response-content-type": true, "response-content-language": true,
	"response-expires": true, "response-cache-control": true, "response-content-disposition": true,
	"response-content-encoding": true,
}

// IsPresignedV2 reports whether the request carries SigV2 query-string authentication.
func IsPresignedV2(r *http.Request) bool {
	q := r.URL.Query()
	return q.Has("AWSAccessKeyId") && q.Has("Signature")
}

func verifyPresignedV2(r *http.Request, creds Credentials, now time.Time) error {
	q := r.URL.Query()
	if q.Get("AWSAccessKeyId") != creds.AccessKey {
		return ErrUnknownAccessKey
	}
	expires, err := strconv.ParseInt(q.Get("Expires"), 10, 64)
	if err != nil {
		return ErrMalformed
	}
	if now.Unix() > expires {
		return ErrExpired
	}
	// botocore hoists Content-Type, Content-MD5 and x-amz-* headers into the query of a presigned URL.
	field := func(name string) string {
		if v := q.Get(strings.ToLower(name)); v != "" {
			return v
		}
		return r.Header.Get(name)
	}
	amz := map[string][]string{}
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") {
			amz[lower] = append(amz[lower], values...)
		}
	}
	for name, values := range q {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") {
			amz[lower] = append(amz[lower], values...)
		}
	}
	names := make([]string, 0, len(amz))
	for name := range amz {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(r.Method + "\n" + field("Content-MD5") + "\n" + field("Content-Type") + "\n" + q.Get("Expires") + "\n")
	for _, name := range names {
		b.WriteString(name + ":" + strings.TrimSpace(strings.Join(amz[name], ",")) + "\n")
	}
	var sub []string
	for name := range q {
		if v2Subresources[name] {
			if v := q.Get(name); v != "" {
				name += "=" + v
			}
			sub = append(sub, name)
		}
	}
	sort.Strings(sub)
	query := ""
	if len(sub) > 0 {
		query = "?" + strings.Join(sub, "&")
	}
	for _, uri := range candidateURIs(r) {
		mac := hmac.New(sha1.New, []byte(creds.SecretKey))
		mac.Write([]byte(b.String() + uri + query))
		if hmac.Equal([]byte(base64.StdEncoding.EncodeToString(mac.Sum(nil))), []byte(q.Get("Signature"))) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

// VerifyPolicy checks the signature on a browser-form (POST) upload policy. The form carries either SigV4
// fields (credential, signature) or SigV2 ones (AWSAccessKeyId as credential, a base64 signature): sigV4
// selects which.
func VerifyPolicy(creds Credentials, policy, credential, sig string, sigV4 bool) error {
	if !sigV4 {
		if credential != creds.AccessKey {
			return ErrUnknownAccessKey
		}
		mac := hmac.New(sha1.New, []byte(creds.SecretKey))
		mac.Write([]byte(policy))
		if !hmac.Equal([]byte(base64.StdEncoding.EncodeToString(mac.Sum(nil))), []byte(sig)) {
			return ErrSignatureMismatch
		}
		return nil
	}
	accessKey, scope, date, err := parseCredential(credential)
	if err != nil {
		return err
	}
	if accessKey != creds.AccessKey {
		return ErrUnknownAccessKey
	}
	if !hmac.Equal([]byte(hex.EncodeToString(hmacSHA256(signingKey(creds.SecretKey, scope, date), policy))), []byte(sig)) {
		return ErrSignatureMismatch
	}
	return nil
}
