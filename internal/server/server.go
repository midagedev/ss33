// Package server serves the S3 subset documented in docs/compatibility.md over a store.Store.
package server

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

// Headers stored with an object and returned on GET/HEAD.
var storedHeaders = []string{"Content-Disposition", "Cache-Control", "Content-Language", "Expires", "Content-Encoding"}

type Server struct {
	Store  *store.Store
	Creds  sigv4.Credentials
	Region string
	Log    *slog.Logger
	Now    func() time.Time
	// Domains turn on virtual-hosted addressing: a request to <bucket>.<domain> addresses <bucket>.
	// "localhost" is always one, so SDKs pointed at http://localhost:9000 work in either style.
	Domains []string
	// Webhooks are the notification targets by id; a bucket routes events to arn:minio:sqs::<id>:webhook.
	Webhooks map[string]Webhook

	queuesMu sync.Mutex
	queues   map[string]chan []byte // webhook delivery queues, started on first event
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	s.serve(rec, r)
	if s.Log != nil {
		s.Log.Info("request", "method", r.Method, "uri", r.RequestURI, "status", rec.status, "ms", time.Since(start).Milliseconds())
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if bucket := s.virtualHostBucket(r.Host); bucket != "" {
		// Route by the path-style path. Signature checks still see the request line as the client sent it,
		// and SigV2's canonical resource is this /bucket/key form anyway.
		r.URL.Path, r.URL.RawPath = "/"+bucket+r.URL.Path, ""
	}
	switch r.URL.Path {
	// The cluster probes are what the official `mc ready` and Kubernetes charts poll.
	case "/minio/health/live", "/minio/health/ready", "/minio/health/cluster", "/minio/health/cluster/read", "/healthz":
		w.WriteHeader(http.StatusOK)
		return
	}

	bucket, key := splitPath(r.URL.Path)
	if isPostObject(r, bucket, key) {
		s.postObject(w, r, bucket)
		return
	}
	if !s.authorized(w, r, bucket, key) {
		return
	}
	q := r.URL.Query()
	switch {
	case bucket == "":
		if r.Method != http.MethodGet {
			s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
			return
		}
		s.listBuckets(w, r)
	case key == "":
		s.bucketOp(w, r, bucket, q)
	default:
		s.objectOp(w, r, bucket, key, q)
	}
}

// virtualHostBucket returns the bucket a virtual-hosted request names in its Host, or "" for path style.
func (s *Server) virtualHostBucket(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	for _, d := range append(s.Domains, "localhost") {
		if bucket, ok := strings.CutSuffix(host, "."+strings.ToLower(d)); ok && bucket != "" {
			return bucket
		}
	}
	return ""
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

// authorized lets signed requests through when the signature verifies, and anonymous reads through on
// buckets whose policy grants public download.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request, bucket, key string) bool {
	if sigv4.HasAuth(r) {
		switch err := sigv4.Verify(r, s.Creds, s.now()); {
		case err == nil:
			return true
		case errors.Is(err, sigv4.ErrUnknownAccessKey):
			s.fail(w, r, http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
		case errors.Is(err, sigv4.ErrExpired):
			s.fail(w, r, http.StatusForbidden, "AccessDenied", "Request has expired")
		case errors.Is(err, sigv4.ErrTimeSkewed):
			s.fail(w, r, http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the current time is too large.")
		case errors.Is(err, sigv4.ErrUnsignedHeaders):
			s.fail(w, r, http.StatusForbidden, "AccessDenied", "There were headers present in the request which were not signed")
		case errors.Is(err, sigv4.ErrSignatureMismatch):
			s.fail(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		default:
			s.fail(w, r, http.StatusBadRequest, "AuthorizationHeaderMalformed", err.Error())
		}
		return false
	}
	if bucket != "" {
		sub := subresource(r.URL.Query())
		if action, resource := anonymousAction(r, bucket, key, sub); action != "" {
			if meta, err := s.Store.Bucket(bucket); err == nil && meta.AllowsAnonymous(action, resource) {
				return true
			}
		}
		if key != "" && sub == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if m, err := s.Store.HeadObject(bucket, key); err == nil && m.Public {
				return true
			}
		}
	}
	s.fail(w, r, http.StatusForbidden, "AccessDenied", "Access Denied.")
	return false
}

// anonymousAction is the policy action an unauthenticated request needs, or "" when only a signed request
// may make it.
func anonymousAction(r *http.Request, bucket, key, sub string) (action, resource string) {
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	object := "arn:aws:s3:::" + bucket + "/" + key
	switch {
	case key != "" && sub == "" && read && r.URL.Query().Has("versionId"):
		return "s3:GetObjectVersion", object
	case key != "" && sub == "" && read:
		return "s3:GetObject", object
	case key != "" && sub == "" && r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") == "":
		return "s3:PutObject", object
	case key != "" && sub == "" && r.Method == http.MethodDelete && r.URL.Query().Has("versionId"):
		return "s3:DeleteObjectVersion", object
	case key != "" && sub == "" && r.Method == http.MethodDelete:
		return "s3:DeleteObject", object
	case key == "" && sub == "" && read:
		return "s3:ListBucket", "arn:aws:s3:::" + bucket // ListObjects and HeadBucket
	case key == "" && sub == "location" && read:
		return "s3:GetBucketLocation", "arn:aws:s3:::" + bucket
	}
	return "", ""
}

// cors answers every origin. Browsers reach ss33 through presigned URLs from a page on another origin;
// a local store has no reason to be stricter than that.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "ETag, Content-Length, Content-Range, Content-Type, Last-Modified, x-amz-request-id, x-amz-version-id")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, PUT, POST, DELETE, HEAD")
		if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
			h.Set("Access-Control-Allow-Headers", req)
		}
		h.Set("Access-Control-Max-Age", "3600")
	}
}

// --- service ---

// listBuckets answers ListBuckets, with the 2024 paging parameters: prefix, max-buckets and a continuation
// token (the last bucket name of the previous page).
func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	max := 10000
	if v := q.Get("max-buckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "max-buckets must be an integer between 1 and 10000")
			return
		}
		max = n
	}
	after, _ := base64.RawURLEncoding.DecodeString(q.Get("continuation-token"))
	type bucketXML struct {
		Name         string
		CreationDate string
		BucketRegion string
	}
	out := struct {
		XMLName           xml.Name `xml:"ListAllMyBucketsResult"`
		NS                string   `xml:"xmlns,attr"`
		Owner             owner
		Buckets           []bucketXML `xml:"Buckets>Bucket"`
		ContinuationToken string      `xml:",omitempty"`
		Prefix            string      `xml:",omitempty"`
	}{NS: s3NS, Owner: defaultOwner, Prefix: q.Get("prefix")}
	for _, b := range s.Store.ListBuckets() {
		if !strings.HasPrefix(b.Name, out.Prefix) || b.Name <= string(after) {
			continue
		}
		if len(out.Buckets) == max {
			out.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(out.Buckets[max-1].Name))
			break
		}
		out.Buckets = append(out.Buckets, bucketXML{b.Name, isoTime(b.Created), s.Region})
	}
	writeXML(w, http.StatusOK, out)
}

// --- buckets ---

func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	sub := subresource(q)
	switch {
	case r.Method == http.MethodPut && sub == "":
		// CreateBucketConfiguration may carry tags (tag on create, which the Terraform AWS provider uses).
		var cfg struct {
			Tags []struct{ Key, Value string } `xml:"Tags>Tag"`
		}
		if body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10)); len(bytes.TrimSpace(body)) > 0 && xml.Unmarshal(body, &cfg) != nil {
			s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
			return
		}
		err := s.Store.CreateBucket(bucket)
		if err == nil && len(cfg.Tags) > 0 {
			tags, _ := xml.Marshal(tagging{NS: s3NS, Tags: cfg.Tags})
			err = s.Store.UpdateBucket(bucket, func(m *store.BucketMeta) { m.Configs = map[string]string{"tagging": string(tags)} })
		}
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut && sub == "policy":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.storeResult(w, r, s.Store.UpdateBucket(bucket, func(m *store.BucketMeta) { m.Policy = string(body) }), http.StatusNoContent)
	case r.Method == http.MethodPut && bucketConfigs[sub].root != "":
		s.putBucketConfig(w, r, bucket, sub)
	case r.Method == http.MethodPut && sub == "acl":
		// Bucket ACLs beyond private are not modelled; public read is a bucket policy (mc anonymous set download).
		if acl := r.Header.Get("X-Amz-Acl"); acl != "" && acl != "private" {
			s.notImplemented(w, r)
			return
		}
		_, err := s.Store.Bucket(bucket)
		s.storeResult(w, r, err, http.StatusOK)
	case r.Method == http.MethodHead && sub == "":
		if _, err := s.Store.Bucket(bucket); err != nil {
			s.storeErr(w, r, err)
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", s.Region)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete && sub == "":
		s.storeResult(w, r, s.Store.DeleteBucket(bucket), http.StatusNoContent)
	case r.Method == http.MethodDelete && sub == "policy":
		s.storeResult(w, r, s.Store.UpdateBucket(bucket, func(m *store.BucketMeta) { m.Policy = "" }), http.StatusNoContent)
	case r.Method == http.MethodDelete && bucketConfigs[sub].root != "" && bucketConfigs[sub].empty == "":
		s.storeResult(w, r, s.Store.UpdateBucket(bucket, func(m *store.BucketMeta) {
			delete(m.Configs, sub)
			if sub == "lifecycle" {
				delete(m.Configs, cfgTransitionMinSize)
			}
		}), http.StatusNoContent)
	case r.Method == http.MethodPost && sub == "delete":
		s.deleteObjects(w, r, bucket)
	case r.Method == http.MethodGet:
		meta, err := s.Store.Bucket(bucket)
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		switch sub {
		case "", "versions":
			s.listObjects(w, r, bucket, q)
		case "location":
			writeXML(w, http.StatusOK, struct {
				XMLName xml.Name `xml:"LocationConstraint"`
				NS      string   `xml:"xmlns,attr"`
				Region  string   `xml:",chardata"`
			}{NS: s3NS, Region: s.Region})
		case "policy":
			if meta.Policy == "" {
				s.fail(w, r, http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, meta.Policy)
		case "cors", "lifecycle", "encryption", "tagging", "versioning", "notification", "website", "replication",
			"publicAccessBlock", "ownershipControls", "logging", "accelerate", "requestPayment":
			def := bucketConfigs[sub]
			cfg := cmp.Or(meta.Configs[sub], def.empty)
			if cfg == "" {
				s.fail(w, r, http.StatusNotFound, def.missing, def.message)
				return
			}
			if sub == "lifecycle" {
				w.Header().Set(hdrTransitionMinSize, cmp.Or(meta.Configs[cfgTransitionMinSize], "all_storage_classes_128K"))
			}
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, cfg)
		case "acl":
			writeACL(w, meta.AllowsAnonymous("s3:GetObject", "arn:aws:s3:::"+bucket+"/*"))
		case "uploads":
			s.listUploads(w, r, bucket, q)
		case "object-lock":
			// Object lock is never enabled. S3 answers this for such buckets, and mc mirror and the Terraform
			// AWS provider read it before doing anything else.
			s.fail(w, r, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
		default:
			s.notImplemented(w, r)
		}
	case r.Method != http.MethodPut && r.Method != http.MethodHead && r.Method != http.MethodDelete && r.Method != http.MethodPost:
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	default:
		s.notImplemented(w, r)
	}
}

// subresource names the query parameter that selects an operation (uploadId, tagging, acl, cors, ...), or ""
// for the bucket or object itself. Authentication, response-* overrides and the parameters of list
// operations do not count; an unknown parameter does, so the request is refused instead of being served
// as a plain GET, PUT or DELETE.
func subresource(q url.Values) string {
	var subs []string
	for k := range q {
		switch {
		case isAuthParam(k), strings.HasPrefix(k, "response-"), listParams[k], k == "x-id", // aws-sdk-go-v2 tags requests with ?x-id=<Operation>
			k == "partNumber" && q.Has("uploadId"),
			k == "versionId": // selects a version of whatever the request addresses
		default:
			subs = append(subs, k)
		}
	}
	sort.Strings(subs)
	return strings.Join(subs, "&")
}

var listParams = map[string]bool{
	"list-type": true, "prefix": true, "delimiter": true, "marker": true, "max-keys": true, "continuation-token": true,
	"start-after": true, "fetch-owner": true, "encoding-type": true, "key-marker": true, "version-id-marker": true,
	"upload-id-marker": true, "max-uploads": true, "max-parts": true, "part-number-marker": true,
	"metadata": true, // MinIO's extension: user metadata in each listed object (mc diff, mc find --metadata)
}

// isAuthParam is true for SigV4 (X-Amz-*) and SigV2 presigned query parameters, including the headers
// botocore moves into a SigV2 presigned URL.
func isAuthParam(k string) bool {
	switch k {
	case "AWSAccessKeyId", "Signature", "Expires", "content-type", "content-md5":
		return true
	}
	return strings.HasPrefix(k, "X-Amz-") || strings.HasPrefix(k, "x-amz-")
}

// listMetadata is the <UserMetadata> element MinIO adds to listed objects for ?metadata=true: one child
// element per header, named after it.
type listMetadata [][2]string

func newListMetadata(o store.ObjectMeta) *listMetadata {
	m := listMetadata{{"content-type", o.ContentType}}
	for _, k := range slices.Sorted(maps.Keys(o.UserMeta)) {
		m = append(m, [2]string{http.CanonicalHeaderKey("x-amz-meta-" + k), o.UserMeta[k]})
	}
	return &m
}

func (m listMetadata) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, kv := range m {
		if err := e.EncodeElement(kv[1], xml.StartElement{Name: xml.Name{Local: kv[0]}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	v2 := q.Get("list-type") == "2"
	max := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < max {
			max = n
		}
	}
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	after := q.Get("marker")
	if q.Has("versions") {
		after = q.Get("key-marker")
	}
	if v2 {
		after = q.Get("start-after")
		if tok := q.Get("continuation-token"); tok != "" {
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			if err != nil {
				s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "The continuation token provided is incorrect")
				return
			}
			after = string(raw)
		}
	}
	res, err := s.Store.List(bucket, prefix, delimiter, after, max)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	enc := func(v string) string { return v }
	encodingType := ""
	if q.Get("encoding-type") == "url" {
		encodingType = "url"
		enc = func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "%2F", "/") }
	}
	type content struct {
		Key          string
		LastModified string
		ETag         string
		Size         int64
		StorageClass string
		Owner        *owner        `xml:",omitempty"`
		UserMetadata *listMetadata `xml:",omitempty"`
		UserTags     string        `xml:",omitempty"`
	}
	type commonPrefix struct{ Prefix string }
	withMeta := q.Get("metadata") == "true"
	contents := make([]content, 0, len(res.Objects))
	for _, o := range res.Objects {
		c := content{Key: enc(o.Key), LastModified: isoTime(o.LastModified), ETag: o.ETag, Size: o.Size, StorageClass: storageClass(o)}
		if withMeta {
			c.UserMetadata = newListMetadata(o)
			tags := url.Values{}
			for k, v := range o.Tags {
				tags.Set(k, v)
			}
			c.UserTags = tags.Encode()
		}
		contents = append(contents, c)
	}
	prefixes := make([]commonPrefix, 0, len(res.CommonPrefixes))
	for _, p := range res.CommonPrefixes {
		prefixes = append(prefixes, commonPrefix{enc(p)})
	}
	if q.Has("versions") {
		s.listVersions(w, r, bucket, q, max, enc, encodingType)
		return
	}
	if v2 {
		out := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			NS                    string   `xml:"xmlns,attr"`
			Name                  string
			Prefix                string
			Delimiter             string `xml:",omitempty"`
			StartAfter            string `xml:",omitempty"`
			ContinuationToken     string `xml:",omitempty"`
			NextContinuationToken string `xml:",omitempty"`
			KeyCount              int
			MaxKeys               int
			EncodingType          string `xml:",omitempty"`
			IsTruncated           bool
			Contents              []content
			CommonPrefixes        []commonPrefix
		}{NS: s3NS, Name: bucket, Prefix: enc(prefix), Delimiter: enc(delimiter), StartAfter: enc(q.Get("start-after")),
			ContinuationToken: q.Get("continuation-token"), KeyCount: len(contents) + len(prefixes), MaxKeys: max,
			EncodingType: encodingType, IsTruncated: res.Truncated, Contents: contents, CommonPrefixes: prefixes}
		if res.Truncated {
			out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(res.NextMarker))
		}
		writeXML(w, http.StatusOK, out)
		return
	}
	out := struct {
		XMLName        xml.Name `xml:"ListBucketResult"`
		NS             string   `xml:"xmlns,attr"`
		Name           string
		Prefix         string
		Marker         string
		NextMarker     string `xml:",omitempty"`
		Delimiter      string `xml:",omitempty"`
		MaxKeys        int
		EncodingType   string `xml:",omitempty"`
		IsTruncated    bool
		Contents       []content
		CommonPrefixes []commonPrefix
	}{NS: s3NS, Name: bucket, Prefix: enc(prefix), Marker: enc(q.Get("marker")), Delimiter: enc(delimiter), MaxKeys: max,
		EncodingType: encodingType, IsTruncated: res.Truncated, Contents: contents, CommonPrefixes: prefixes}
	if res.Truncated && delimiter != "" {
		out.NextMarker = enc(strings.TrimSuffix(res.NextMarker, "\xff"))
	}
	writeXML(w, http.StatusOK, out)
}

// listVersions answers ListObjectVersions: versions and delete markers in key order, newest first within
// a key, as interleaved <Version> and <DeleteMarker> elements.
func (s *Server) listVersions(w http.ResponseWriter, r *http.Request, bucket string, q url.Values, max int, enc func(string) string, encodingType string) {
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	res, err := s.Store.ListVersions(bucket, prefix, delimiter, q.Get("key-marker"), q.Get("version-id-marker"), max)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	type version struct {
		XMLName      xml.Name
		Key          string
		VersionId    string
		IsLatest     bool
		LastModified string
		ETag         string `xml:",omitempty"`
		Size         *int64 `xml:",omitempty"`
		StorageClass string `xml:",omitempty"`
		Owner        owner
	}
	entries := make([]version, 0, len(res.Versions))
	for _, v := range res.Versions {
		e := version{XMLName: xml.Name{Local: "Version"}, Key: enc(v.Key), VersionId: cmp.Or(v.VersionID, "null"), IsLatest: v.IsLatest,
			LastModified: isoTime(v.LastModified), Owner: defaultOwner}
		if v.DeleteMarker {
			e.XMLName.Local = "DeleteMarker"
		} else {
			size := v.Size
			e.ETag, e.Size, e.StorageClass = v.ETag, &size, storageClass(v.ObjectMeta)
		}
		entries = append(entries, e)
	}
	type commonPrefix struct{ Prefix string }
	prefixes := make([]commonPrefix, 0, len(res.CommonPrefixes))
	for _, p := range res.CommonPrefixes {
		prefixes = append(prefixes, commonPrefix{enc(p)})
	}
	out := struct {
		XMLName             xml.Name `xml:"ListVersionsResult"`
		NS                  string   `xml:"xmlns,attr"`
		Name                string
		Prefix              string
		KeyMarker           string
		VersionIdMarker     string
		NextKeyMarker       string `xml:",omitempty"`
		NextVersionIdMarker string `xml:",omitempty"`
		MaxKeys             int
		Delimiter           string `xml:",omitempty"`
		EncodingType        string `xml:",omitempty"`
		IsTruncated         bool
		Entries             []version
		CommonPrefixes      []commonPrefix
	}{NS: s3NS, Name: bucket, Prefix: enc(prefix), KeyMarker: enc(q.Get("key-marker")), VersionIdMarker: q.Get("version-id-marker"),
		MaxKeys: max, Delimiter: enc(delimiter), EncodingType: encodingType, IsTruncated: res.Truncated, Entries: entries, CommonPrefixes: prefixes}
	if res.Truncated {
		out.NextKeyMarker, out.NextVersionIdMarker = enc(res.NextKeyMarker), res.NextVersionIDMarker
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Quiet   bool
		Objects []struct{ Key, VersionId string } `xml:"Object"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed")
		return
	}
	type deleted struct {
		Key                   string
		VersionId             string `xml:",omitempty"`
		DeleteMarker          bool   `xml:",omitempty"`
		DeleteMarkerVersionId string `xml:",omitempty"`
	}
	type delErr struct{ Key, VersionId, Code, Message string }
	out := struct {
		XMLName xml.Name `xml:"DeleteResult"`
		NS      string   `xml:"xmlns,attr"`
		Deleted []deleted
		Error   []delErr
	}{NS: s3NS}
	for _, o := range req.Objects {
		var d store.Deleted
		var err error
		if o.VersionId != "" {
			d, err = s.Store.DeleteVersion(bucket, o.Key, o.VersionId)
		} else {
			d, err = s.Store.DeleteObject(bucket, o.Key)
		}
		if err != nil {
			out.Error = append(out.Error, delErr{o.Key, o.VersionId, err.Error(), err.Error()})
			continue
		}
		s.notifyDelete(r, bucket, o.Key, d, o.VersionId != "")
		if !req.Quiet {
			e := deleted{Key: o.Key, VersionId: o.VersionId, DeleteMarker: d.DeleteMarker}
			if d.DeleteMarker && o.VersionId == "" {
				e.DeleteMarkerVersionId = d.VersionID
			}
			out.Deleted = append(out.Deleted, e)
		}
	}
	writeXML(w, http.StatusOK, out)
}

// --- objects ---

func (s *Server) objectOp(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	sub := subresource(q)
	copySource := r.Header.Get("X-Amz-Copy-Source") != ""
	switch {
	case r.Method == http.MethodPut && sub == "" && copySource:
		s.copyObject(w, r, bucket, key)
	case r.Method == http.MethodPut && sub == "":
		s.putObject(w, r, bucket, key)
	case r.Method == http.MethodPut && sub == "uploadId" && copySource:
		s.uploadPartCopy(w, r, bucket, key, q)
	case r.Method == http.MethodPut && sub == "uploadId":
		s.uploadPart(w, r, bucket, key, q)
	case r.Method == http.MethodPut && sub == "tagging":
		s.putTagging(w, r, bucket, key, q.Get("versionId"))
	case r.Method == http.MethodPut && sub == "acl":
		s.putObjectACL(w, r, bucket, key, q.Get("versionId"))
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (sub == "" || sub == "partNumber"):
		s.getObject(w, r, bucket, key, q)
	case r.Method == http.MethodGet && sub == "uploadId":
		s.listParts(w, r, bucket, key, q)
	case r.Method == http.MethodGet && sub == "tagging":
		s.getTagging(w, r, bucket, key, q.Get("versionId"))
	case r.Method == http.MethodGet && sub == "attributes":
		s.getAttributes(w, r, bucket, key, q.Get("versionId"))
	case r.Method == http.MethodGet && sub == "acl":
		m, err := s.Store.Version(bucket, key, q.Get("versionId"))
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		writeACL(w, m.Public)
	case r.Method == http.MethodDelete && sub == "":
		s.deleteObject(w, r, bucket, key, q)
	case r.Method == http.MethodDelete && sub == "uploadId":
		s.storeResult(w, r, s.Store.AbortUpload(bucket, key, q.Get("uploadId")), http.StatusNoContent)
	case r.Method == http.MethodDelete && sub == "tagging":
		s.storeResult(w, r, s.Store.UpdateObject(bucket, key, q.Get("versionId"), func(m *store.ObjectMeta) { m.Tags = nil }), http.StatusNoContent)
	case r.Method == http.MethodPost && sub == "uploads":
		s.createUpload(w, r, bucket, key)
	case r.Method == http.MethodPost && sub == "uploadId":
		s.completeUpload(w, r, bucket, key, q.Get("uploadId"))
	case r.Method != http.MethodPut && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete && r.Method != http.MethodPost:
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	default:
		s.notImplemented(w, r)
	}
}

// deleteObject is DeleteObject: with ?versionId= it removes that version for good, without it the object
// goes away or, with versioning, gets a delete marker.
func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	var d store.Deleted
	var err error
	if q.Has("versionId") {
		d, err = s.Store.DeleteVersion(bucket, key, q.Get("versionId"))
	} else {
		d, err = s.Store.DeleteObject(bucket, key)
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	setVersionHeaders(w, d.VersionID, d.DeleteMarker)
	s.notifyDelete(r, bucket, key, d, q.Has("versionId"))
	w.WriteHeader(http.StatusNoContent)
}

// setVersionHeaders sets x-amz-version-id ("" means the bucket has never had versioning, so S3 sends
// none) and x-amz-delete-marker.
func setVersionHeaders(w http.ResponseWriter, versionID string, deleteMarker bool) {
	if versionID != "" {
		w.Header().Set("X-Amz-Version-Id", versionID)
	}
	if deleteMarker {
		w.Header().Set("X-Amz-Delete-Marker", "true")
	}
}

// responseVersionID is the version ID a response reports for m: none in a bucket that never had
// versioning, "null" for the null version once it has.
func (s *Server) responseVersionID(bucket string, m store.ObjectMeta) string {
	if m.VersionID != "" {
		return m.VersionID
	}
	if meta, err := s.Store.Bucket(bucket); err == nil && meta.Configs["versioning"] != "" {
		return "null"
	}
	return ""
}

func objectMetaFromRequest(r *http.Request, key string) store.ObjectMeta {
	meta := store.ObjectMeta{Key: key, ContentType: r.Header.Get("Content-Type"), Headers: map[string]string{}, UserMeta: map[string]string{}}
	if meta.ContentType == "" {
		meta.ContentType = "binary/octet-stream"
	}
	for _, h := range storedHeaders {
		v := r.Header.Get(h)
		if h == "Content-Encoding" {
			v = storedContentEncoding(v)
		}
		if v != "" {
			meta.Headers[h] = v
		}
	}
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") {
			meta.UserMeta[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
		}
	}
	meta.Tags = requestTags(r)
	meta.Public = publicACL(r.Header.Get("X-Amz-Acl"))
	return meta
}

// requestTags parses the x-amz-tagging header, which carries tags as a URL query string.
func requestTags(r *http.Request) map[string]string {
	q, _ := url.ParseQuery(r.Header.Get("X-Amz-Tagging"))
	if len(q) == 0 {
		return nil
	}
	tags := make(map[string]string, len(q))
	for k := range q {
		tags[k] = q.Get(k)
	}
	return tags
}

func publicACL(canned string) bool { return canned == "public-read" || canned == "public-read-write" }

func requestBody(r *http.Request) io.Reader {
	if isAWSChunked(r) {
		return newChunkedReader(r.Body)
	}
	return r.Body
}

// checksumHashes are the x-amz-checksum-* algorithms PutObject answers for. The value is computed over the
// stored bytes, not copied from the request; a client-sent value is not compared against it.
var checksumHashes = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"sha1":      sha1.New,
	"sha256":    sha256.New,
	"crc64nvme": func() hash.Hash { return crc64.New(crc64NVME) },
}

// crc64NVME is the CRC-64/NVME table; the AWS CLI v2 sends x-amz-checksum-crc64nvme by default.
var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// requestedChecksum names the algorithm a request asked for: the SDK header, an aws-chunked trailer, or a
// checksum header sent up front.
func requestedChecksum(r *http.Request) string {
	if alg := r.Header.Get("X-Amz-Sdk-Checksum-Algorithm"); alg != "" {
		return strings.ToLower(alg)
	}
	if t := r.Header.Get("X-Amz-Trailer"); t != "" {
		return strings.TrimPrefix(strings.ToLower(t), "x-amz-checksum-")
	}
	for alg := range checksumHashes {
		if r.Header.Get("X-Amz-Checksum-"+alg) != "" {
			return alg
		}
	}
	return ""
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	body, err := newDigestReader(r)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm != "*" {
		s.fail(w, r, http.StatusNotImplemented, "NotImplemented", "If-None-Match on PUT only supports *")
		return
	}
	cond := store.Precondition{IfMatch: r.Header.Get("If-Match"), IfNoneMatch: r.Header.Get("If-None-Match")}
	meta, err := s.requestMeta(r, bucket, key)
	if err == nil {
		meta, err = s.Store.PutObject(bucket, meta, body, cond)
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	w.Header().Set("ETag", meta.ETag)
	setEncryptionHeaders(w, meta)
	body.setChecksumHeader(w)
	s.notify(r, bucket, "s3:ObjectCreated:Put", meta)
	setVersionHeaders(w, s.responseVersionID(bucket, meta), false)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	srcBucket, srcKey, srcVersion := copySource(r)
	src, err := s.Store.Version(srcBucket, srcKey, srcVersion)
	if errors.Is(err, store.ErrDeleteMarker) {
		err = store.ErrNoSuchKey // a delete marker cannot be a copy source
	}
	if err == nil {
		err = checkCustomerKey(r.Header, copySourcePrefix, src)
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	// Encryption and storage class come from the copy request (or the bucket default), never the source.
	dest, err := s.requestMeta(r, bucket, key)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	meta := dest
	meta.Tags = src.Tags
	if !strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		meta = src
		meta.Headers = maps.Clone(src.Headers)
		for _, h := range []string{hdrSSE, hdrSSEKMSKey, hdrSSEBucketKey, hdrSSECAlgorithm, hdrSSECKeyMD5, hdrStorageClass} {
			delete(meta.Headers, h)
			if v := dest.Headers[h]; v != "" {
				if meta.Headers == nil {
					meta.Headers = map[string]string{}
				}
				meta.Headers[h] = v
			}
		}
	}
	if strings.EqualFold(r.Header.Get("X-Amz-Tagging-Directive"), "REPLACE") {
		meta.Tags = requestTags(r)
	}
	meta.Public = publicACL(r.Header.Get("X-Amz-Acl")) // S3 does not copy the ACL
	meta, err = s.Store.CopyObject(srcBucket, srcKey, srcVersion, bucket, key, &meta)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	if v := s.responseVersionID(srcBucket, src); v != "" {
		w.Header().Set("X-Amz-Copy-Source-Version-Id", v)
	}
	setEncryptionHeaders(w, meta)
	s.notify(r, bucket, "s3:ObjectCreated:Copy", meta)
	setVersionHeaders(w, s.responseVersionID(bucket, meta), false)
	writeXML(w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		NS           string   `xml:"xmlns,attr"`
		LastModified string
		ETag         string
	}{NS: s3NS, LastModified: isoTime(meta.LastModified), ETag: meta.ETag})
}

// copySource splits x-amz-copy-source ("bucket/key", URL-encoded, optionally "?versionId=...").
func copySource(r *http.Request) (bucket, key, versionID string) {
	src, query, _ := strings.Cut(r.Header.Get("X-Amz-Copy-Source"), "?")
	if decoded, err := url.PathUnescape(src); err == nil {
		src = decoded
	}
	q, _ := url.ParseQuery(query)
	bucket, key = splitPath("/" + strings.TrimPrefix(src, "/"))
	return bucket, key, q.Get("versionId")
}

// responseOverrides are the presigned-GET query parameters that replace response headers.
var responseOverrides = map[string]string{
	"response-content-type":        "Content-Type",
	"response-content-disposition": "Content-Disposition",
	"response-cache-control":       "Cache-Control",
	"response-content-language":    "Content-Language",
	"response-content-encoding":    "Content-Encoding",
	"response-expires":             "Expires",
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	var meta store.ObjectMeta
	var body io.ReadSeeker
	var err error
	if r.Method == http.MethodHead {
		// HEAD never reads the body: answer from the in-memory index without touching the disk.
		meta, err = s.Store.Version(bucket, key, q.Get("versionId"))
		body = io.NewSectionReader(zeros{}, 0, meta.Size)
	} else {
		var f store.Object
		meta, f, err = s.Store.OpenVersion(bucket, key, q.Get("versionId"))
		if f != nil {
			defer f.Close()
			body = f
		}
	}
	if err == nil {
		err = checkCustomerKey(r.Header, "", meta)
	}
	if err != nil {
		if meta.DeleteMarker { // the current version, or the one asked for, is a delete marker
			setVersionHeaders(w, s.responseVersionID(bucket, meta), true)
		}
		s.storeErr(w, r, err)
		return
	}
	h := w.Header()
	setVersionHeaders(w, s.responseVersionID(bucket, meta), false)
	if r.Method == http.MethodHead {
		s.notify(r, bucket, "s3:ObjectAccessed:Head", meta)
	} else {
		s.notify(r, bucket, "s3:ObjectAccessed:Get", meta)
	}
	h.Set("Content-Type", meta.ContentType)
	h.Set("ETag", meta.ETag)
	h.Set("Accept-Ranges", "bytes")
	for name, v := range meta.Headers {
		h.Set(name, v)
	}
	for name, v := range meta.UserMeta {
		h["x-amz-meta-"+name] = []string{v} // lowercase as S3 sends it: boto3 keys Metadata by the raw header name
	}
	if len(meta.Tags) > 0 {
		h.Set("X-Amz-Tagging-Count", strconv.Itoa(len(meta.Tags)))
	}
	for param, header := range responseOverrides {
		if v := q.Get(param); v != "" {
			h.Set(header, v)
		}
	}
	if q.Has("partNumber") && !s.partRange(w, r, meta, q.Get("partNumber")) {
		return
	}
	// ServeContent handles Range, If-Range, If-None-Match and HEAD.
	http.ServeContent(w, r, "", meta.LastModified, body)
}

// partRange turns GET/HEAD ?partNumber=N into the byte range of that part, which ServeContent then serves
// as a 206. A single-part object has one part, the whole object. It reports false after answering an error.
func (s *Server) partRange(w http.ResponseWriter, r *http.Request, meta store.ObjectMeta, v string) bool {
	sizes := meta.Parts
	if sizes == nil {
		sizes = []int64{meta.Size}
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 10000 {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		return false
	}
	if n > len(sizes) {
		s.fail(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber", "The requested partnumber is not satisfiable")
		return false
	}
	var start int64
	for _, size := range sizes[:n-1] {
		start += size
	}
	if meta.Parts != nil {
		w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(meta.Parts)))
	}
	if sizes[n-1] > 0 {
		r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+sizes[n-1]-1))
	}
	return true
}

// getAttributes answers GetObjectAttributes for the attributes ss33 has: ETag, size, storage class and parts.
func (s *Server) getAttributes(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) {
	m, err := s.Store.Version(bucket, key, versionID)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	want := map[string]bool{}
	for _, v := range r.Header.Values("X-Amz-Object-Attributes") {
		for _, a := range strings.Split(v, ",") {
			want[strings.TrimSpace(a)] = true
		}
	}
	type partXML struct {
		PartNumber int
		Size       int64
	}
	type partsXML struct {
		TotalPartsCount int
		PartsCount      int
		IsTruncated     bool
		Parts           []partXML `xml:"Part"`
	}
	out := struct {
		XMLName      xml.Name  `xml:"GetObjectAttributesResponse"`
		NS           string    `xml:"xmlns,attr"`
		ETag         string    `xml:",omitempty"`
		ObjectParts  *partsXML `xml:",omitempty"`
		StorageClass string    `xml:",omitempty"`
		ObjectSize   *int64    `xml:",omitempty"`
	}{NS: s3NS}
	if want["ETag"] {
		out.ETag = strings.Trim(m.ETag, `"`)
	}
	if want["StorageClass"] {
		out.StorageClass = storageClass(m)
	}
	if want["ObjectSize"] {
		out.ObjectSize = &m.Size
	}
	if want["ObjectParts"] && m.Parts != nil {
		p := &partsXML{TotalPartsCount: len(m.Parts), PartsCount: len(m.Parts)}
		for i, size := range m.Parts {
			p.Parts = append(p.Parts, partXML{i + 1, size})
		}
		out.ObjectParts = p
	}
	w.Header().Set("Last-Modified", m.LastModified.UTC().Format(http.TimeFormat))
	setVersionHeaders(w, s.responseVersionID(bucket, m), false)
	writeXML(w, http.StatusOK, out)
}

// zeros backs the HEAD body; ServeContent only seeks it (a multi-range HEAD may read and discard).
type zeros struct{}

func (zeros) ReadAt(p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}

// --- multipart ---

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	meta, err := s.requestMeta(r, bucket, key)
	var id string
	if err == nil {
		id, err = s.Store.CreateUpload(bucket, meta)
	}
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	setEncryptionHeaders(w, meta)
	writeXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Bucket   string
		Key      string
		UploadId string
	}{NS: s3NS, Bucket: bucket, Key: key, UploadId: id})
}

func (s *Server) partNumber(w http.ResponseWriter, r *http.Request, q url.Values) (int, bool) {
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		return 0, false
	}
	return n, true
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	n, ok := s.partNumber(w, r, q)
	if !ok {
		return
	}
	body, err := newDigestReader(r)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	etag, err := s.Store.PutPart(bucket, key, q.Get("uploadId"), n, body)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	body.setChecksumHeader(w)
	w.WriteHeader(http.StatusOK)
}

// uploadPartCopy fills a part from (a byte range of) an existing object: boto3's managed copy and the Java
// transfer manager use it for large objects.
func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	n, ok := s.partNumber(w, r, q)
	if !ok {
		return
	}
	srcBucket, srcKey, srcVersion := copySource(r)
	src, f, err := s.Store.OpenVersion(srcBucket, srcKey, srcVersion)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	defer f.Close()
	var body io.Reader = f
	if rng := r.Header.Get("X-Amz-Copy-Source-Range"); rng != "" {
		first, last, ok := parseByteRange(rng)
		if !ok || last >= src.Size {
			s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "Range specified is not valid for source object of size: "+strconv.FormatInt(src.Size, 10))
			return
		}
		if _, err := f.Seek(first, io.SeekStart); err != nil {
			s.storeErr(w, r, err)
			return
		}
		body = io.LimitReader(f, last-first+1)
	}
	etag, err := s.Store.PutPart(bucket, key, q.Get("uploadId"), n, body)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyPartResult"`
		NS           string   `xml:"xmlns,attr"`
		LastModified string
		ETag         string
	}{NS: s3NS, LastModified: isoTime(s.now()), ETag: etag})
}

// parseByteRange parses "bytes=first-last", the only form x-amz-copy-source-range takes.
func parseByteRange(v string) (first, last int64, ok bool) {
	a, b, found := strings.Cut(strings.TrimPrefix(v, "bytes="), "-")
	first, err1 := strconv.ParseInt(a, 10, 64)
	last, err2 := strconv.ParseInt(b, 10, 64)
	return first, last, found && strings.HasPrefix(v, "bytes=") && err1 == nil && err2 == nil && first >= 0 && first <= last
}

func (s *Server) listUploads(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	uploads, err := s.Store.ListUploads(bucket, q.Get("prefix"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	type uploadXML struct {
		Key          string
		UploadId     string
		Initiator    owner
		Owner        owner
		StorageClass string
		Initiated    string
	}
	out := struct {
		XMLName            xml.Name `xml:"ListMultipartUploadsResult"`
		NS                 string   `xml:"xmlns,attr"`
		Bucket             string
		KeyMarker          string
		UploadIdMarker     string
		NextKeyMarker      string
		NextUploadIdMarker string
		Prefix             string
		MaxUploads         int
		IsTruncated        bool
		Uploads            []uploadXML `xml:"Upload"`
	}{NS: s3NS, Bucket: bucket, Prefix: q.Get("prefix"), MaxUploads: 1000}
	// ponytail: no pagination; every open upload is returned in one page.
	for _, u := range uploads {
		out.Uploads = append(out.Uploads, uploadXML{u.Key, u.ID, defaultOwner, defaultOwner, "STANDARD", isoTime(u.Created)})
	}
	writeXML(w, http.StatusOK, out)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	// S3 refuses a completion without parts as malformed; storing one would leave an object with no bytes.
	if err := xml.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil || len(req.Parts) == 0 {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
		return
	}
	parts := make([]store.Part, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = store.Part{Number: p.PartNumber, ETag: p.ETag}
	}
	cond := store.Precondition{IfMatch: r.Header.Get("If-Match"), IfNoneMatch: r.Header.Get("If-None-Match")}
	meta, err := s.Store.CompleteUpload(bucket, key, id, parts, cond)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.notify(r, bucket, "s3:ObjectCreated:CompleteMultipartUpload", meta)
	setVersionHeaders(w, s.responseVersionID(bucket, meta), false)
	writeXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{NS: s3NS, Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: meta.ETag})
}

// listParts answers ListParts with every field S3 sends: the Docker registry's S3 driver dereferences
// IsTruncated and the markers without checking them.
func (s *Server) listParts(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	parts, err := s.Store.ListParts(bucket, key, q.Get("uploadId"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	max := 1000
	if n, err := strconv.Atoi(q.Get("max-parts")); err == nil && n >= 0 && n < max {
		max = n
	}
	marker, _ := strconv.Atoi(q.Get("part-number-marker"))
	type partXML struct {
		PartNumber   int
		LastModified string
		ETag         string
		Size         int64
	}
	out := struct {
		XMLName              xml.Name `xml:"ListPartsResult"`
		NS                   string   `xml:"xmlns,attr"`
		Bucket               string
		Key                  string
		UploadId             string
		Initiator            owner
		Owner                owner
		StorageClass         string
		PartNumberMarker     int
		NextPartNumberMarker int
		MaxParts             int
		IsTruncated          bool
		Parts                []partXML `xml:"Part"`
	}{NS: s3NS, Bucket: bucket, Key: key, UploadId: q.Get("uploadId"), Initiator: defaultOwner, Owner: defaultOwner,
		StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: max}
	for _, p := range parts {
		if p.Number <= marker {
			continue
		}
		if len(out.Parts) == max {
			out.IsTruncated = true
			break
		}
		out.Parts = append(out.Parts, partXML{p.Number, isoTime(p.LastModified), p.ETag, p.Size})
		out.NextPartNumberMarker = p.Number
	}
	writeXML(w, http.StatusOK, out)
}

// --- tagging, ACLs, CORS ---

type tagging struct {
	XMLName xml.Name                      `xml:"Tagging"`
	NS      string                        `xml:"xmlns,attr,omitempty"`
	Tags    []struct{ Key, Value string } `xml:"TagSet>Tag"`
}

func (s *Server) getTagging(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) {
	m, err := s.Store.Version(bucket, key, versionID)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	out := tagging{NS: s3NS, Tags: []struct{ Key, Value string }{}}
	for k, v := range m.Tags {
		out.Tags = append(out.Tags, struct{ Key, Value string }{k, v})
	}
	sort.Slice(out.Tags, func(i, j int) bool { return out.Tags[i].Key < out.Tags[j].Key })
	writeXML(w, http.StatusOK, out)
}

func (s *Server) putTagging(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) {
	var in tagging
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed")
		return
	}
	tags := make(map[string]string, len(in.Tags))
	for _, t := range in.Tags {
		tags[t.Key] = t.Value
	}
	s.storeResult(w, r, s.Store.UpdateObject(bucket, key, versionID, func(m *store.ObjectMeta) { m.Tags = tags }), http.StatusOK)
}

// putObjectACL takes a canned ACL header or an AccessControlPolicy body; the only distinction kept is
// whether everyone may read.
func (s *Server) putObjectACL(w http.ResponseWriter, r *http.Request, bucket, key, versionID string) {
	public := publicACL(r.Header.Get("X-Amz-Acl"))
	if r.Header.Get("X-Amz-Acl") == "" {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		public = strings.Contains(string(body), "/global/AllUsers")
	}
	s.storeResult(w, r, s.Store.UpdateObject(bucket, key, versionID, func(m *store.ObjectMeta) { m.Public = public }), http.StatusOK)
}

const aclXML = `<AccessControlPolicy xmlns="` + s3NS + `"><Owner><ID>ss33</ID><DisplayName>ss33</DisplayName></Owner><AccessControlList>` +
	`<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="CanonicalUser"><ID>ss33</ID><DisplayName>ss33</DisplayName></Grantee><Permission>FULL_CONTROL</Permission></Grant>`

const publicGrantXML = `<Grant><Grantee xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:type="Group"><URI>http://acs.amazonaws.com/groups/global/AllUsers</URI></Grantee><Permission>READ</Permission></Grant>`

func writeACL(w http.ResponseWriter, public bool) {
	w.Header().Set("Content-Type", "application/xml")
	io.WriteString(w, xml.Header+aclXML)
	if public {
		io.WriteString(w, publicGrantXML)
	}
	io.WriteString(w, `</AccessControlList></AccessControlPolicy>`)
}

// bucketConfigs are bucket configurations that are stored and returned but not acted on: CORS stays open to
// every origin (see cors), lifecycle rules never expire anything, encryption is not applied, and versioning
// keeps only the latest version. Bootstrap scripts set them; failing those calls would stop the script.
// bucketConfigs are the bucket configurations stored and returned as sent (see docs/compatibility.md for
// which of them ss33 acts on). Reading one that was never set answers missing/message, or empty when S3
// answers with a default document instead.
var bucketConfigs = map[string]struct{ root, missing, message, empty string }{
	"cors":              {root: "CORSConfiguration", missing: "NoSuchCORSConfiguration", message: "The CORS configuration does not exist"},
	"lifecycle":         {root: "LifecycleConfiguration", missing: "NoSuchLifecycleConfiguration", message: "The lifecycle configuration does not exist"},
	"encryption":        {root: "ServerSideEncryptionConfiguration", missing: "ServerSideEncryptionConfigurationNotFoundError", message: "The server side encryption configuration was not found"},
	"tagging":           {root: "Tagging", missing: "NoSuchTagSet", message: "The TagSet does not exist"},
	"website":           {root: "WebsiteConfiguration", missing: "NoSuchWebsiteConfiguration", message: "The specified bucket does not have a website configuration"},
	"replication":       {root: "ReplicationConfiguration", missing: "ReplicationConfigurationNotFoundError", message: "The replication configuration was not found"},
	"publicAccessBlock": {root: "PublicAccessBlockConfiguration", missing: "NoSuchPublicAccessBlockConfiguration", message: "The public access block configuration was not found"},
	"ownershipControls": {root: "OwnershipControls", missing: "OwnershipControlsNotFoundError", message: "The bucket ownership controls were not found"},
	"versioning":        {root: "VersioningConfiguration", empty: `<VersioningConfiguration xmlns="` + s3NS + `"/>`},
	"notification":      {root: "NotificationConfiguration", empty: `<NotificationConfiguration xmlns="` + s3NS + `"/>`},
	"logging":           {root: "BucketLoggingStatus", empty: `<BucketLoggingStatus xmlns="` + s3NS + `"/>`},
	"accelerate":        {root: "AccelerateConfiguration", empty: `<AccelerateConfiguration xmlns="` + s3NS + `"/>`},
	"requestPayment":    {root: "RequestPaymentConfiguration", empty: `<RequestPaymentConfiguration xmlns="` + s3NS + `"><Payer>BucketOwner</Payer></RequestPaymentConfiguration>`},
}

// transitionMinSize is the lifecycle setting S3 takes and reports in a header next to the XML; the
// Terraform AWS provider waits until it reads back what it sent.
const (
	hdrTransitionMinSize = "X-Amz-Transition-Default-Minimum-Object-Size"
	cfgTransitionMinSize = "lifecycle:transition-default-minimum-object-size"
)

func (s *Server) putBucketConfig(w http.ResponseWriter, r *http.Request, bucket, sub string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	var root struct{ XMLName xml.Name }
	var versioning struct{ Status string }
	xml.Unmarshal(body, &versioning)
	if xml.Unmarshal(body, &root) != nil || root.XMLName.Local != bucketConfigs[sub].root ||
		sub == "versioning" && versioning.Status != "Enabled" && versioning.Status != "Suspended" {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema")
		return
	}
	if sub == "notification" {
		if err := s.checkNotificationConfig(body); err != nil {
			s.fail(w, r, http.StatusBadRequest, "InvalidArgument", err.Error())
			return
		}
	}
	minSize := r.Header.Get(hdrTransitionMinSize)
	err := s.Store.UpdateBucket(bucket, func(m *store.BucketMeta) {
		if m.Configs == nil {
			m.Configs = map[string]string{}
		}
		m.Configs[sub] = string(body)
		if sub == "lifecycle" {
			m.Configs[cfgTransitionMinSize] = minSize
		}
	})
	if err == nil && sub == "lifecycle" {
		w.Header().Set(hdrTransitionMinSize, cmp.Or(minSize, "all_storage_classes_128K"))
	}
	s.storeResult(w, r, err, http.StatusOK)
}

func storageClass(m store.ObjectMeta) string { return cmp.Or(m.Headers[hdrStorageClass], "STANDARD") }

// --- responses ---

type owner struct {
	ID          string
	DisplayName string
}

var defaultOwner = owner{ID: "ss33", DisplayName: "ss33"}

var storeErrors = map[error]struct {
	status  int
	message string
}{
	store.ErrNoSuchBucket:     {http.StatusNotFound, "The specified bucket does not exist"},
	store.ErrNoSuchKey:        {http.StatusNotFound, "The specified key does not exist."},
	store.ErrBucketExists:     {http.StatusConflict, "Your previous request to create the named bucket succeeded and you already own it."},
	store.ErrBucketNotEmpty:   {http.StatusConflict, "The bucket you tried to delete is not empty"},
	store.ErrInvalidBucket:    {http.StatusBadRequest, "The specified bucket is not valid."},
	store.ErrNoSuchUpload:     {http.StatusNotFound, "The specified upload does not exist."},
	store.ErrInvalidPart:      {http.StatusBadRequest, "One or more of the specified parts could not be found."},
	store.ErrInvalidPartOrder: {http.StatusBadRequest, "The list of parts was not in ascending order."},
	store.ErrPrecondition:     {http.StatusPreconditionFailed, "At least one of the pre-conditions you specified did not hold"},
	store.ErrNoSuchVersion:    {http.StatusNotFound, "The specified version does not exist."},
	store.ErrDeleteMarker:     {http.StatusMethodNotAllowed, "The specified method is not allowed against this resource."},
	errIncompleteBody:         {http.StatusBadRequest, "The request body is not valid aws-chunked encoding."},
	errBadDigest:              {http.StatusBadRequest, "The Content-MD5 or checksum you specified did not match what we received."},
	errInvalidDigest:          {http.StatusBadRequest, "The Content-MD5 you specified was invalid."},
	errSSECKeyInvalid:         {http.StatusBadRequest, "The secret key was invalid for the specified algorithm, or does not match its MD5."},
	errSSECRequired:           {http.StatusBadRequest, "The object was stored using a form of Server Side Encryption. The correct parameters must be provided to retrieve the object."},
	errSSECKeyMismatch:        {http.StatusForbidden, "Access Denied"},
	errStorageClass:           {http.StatusBadRequest, "The storage class you specified is not valid"},
	errSSEAlgorithm:           {http.StatusBadRequest, "The encryption request you specified is not valid."},
}

// errIncompleteBody is a malformed aws-chunked body; its text is the S3 error code.
var errIncompleteBody = errors.New("IncompleteBody")

func (s *Server) storeErr(w http.ResponseWriter, r *http.Request, err error) {
	for known, e := range storeErrors {
		if errors.Is(err, known) {
			s.fail(w, r, e.status, known.Error(), e.message)
			return
		}
	}
	if s.Log != nil {
		s.Log.Error("internal error", "uri", r.RequestURI, "err", err)
	}
	s.fail(w, r, http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.")
}

func (s *Server) storeResult(w http.ResponseWriter, r *http.Request, err error, okStatus int) {
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	w.WriteHeader(okStatus)
}

func (s *Server) notImplemented(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotImplemented, "NotImplemented", "ss33 does not implement this operation (see docs/compatibility.md)")
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	reqID := requestID()
	w.Header().Set("X-Amz-Request-Id", reqID)
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string
		Message   string
		Resource  string
		RequestId string
	}{Code: code, Message: message, Resource: r.URL.Path, RequestId: reqID})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	io.WriteString(w, xml.Header)
	xml.NewEncoder(w).Encode(v)
}

func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func requestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// ReadFrom keeps net/http's sendfile path: without it, embedding hides http.response's ReadFrom and every
// GET body is copied through a 32 KiB user-space buffer.
func (s *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(s.ResponseWriter, r)
}
