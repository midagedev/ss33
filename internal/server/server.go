// Package server serves the S3 subset documented in docs/compatibility.md over a store.Store.
package server

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	switch r.URL.Path {
	case "/minio/health/live", "/minio/health/ready", "/healthz":
		w.WriteHeader(http.StatusOK)
		return
	}

	bucket, key := splitPath(r.URL.Path)
	if !s.authorized(w, r, bucket, key) {
		return
	}
	q := r.URL.Query()
	q.Del("x-id") // aws-sdk-go-v2 tags requests with ?x-id=<Operation>; it is not a subresource
	switch {
	case bucket == "":
		if r.Method != http.MethodGet {
			s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
			return
		}
		s.listBuckets(w)
	case key == "":
		s.bucketOp(w, r, bucket, q)
	default:
		s.objectOp(w, r, bucket, key, q)
	}
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
		case errors.Is(err, sigv4.ErrUnsignedHeaders):
			s.fail(w, r, http.StatusForbidden, "AccessDenied", "There were headers present in the request which were not signed")
		case errors.Is(err, sigv4.ErrSignatureMismatch):
			s.fail(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		default:
			s.fail(w, r, http.StatusBadRequest, "AuthorizationHeaderMalformed", err.Error())
		}
		return false
	}
	if bucket != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if meta, err := s.Store.Bucket(bucket); err == nil && meta.PublicRead() {
			return true
		}
	}
	s.fail(w, r, http.StatusForbidden, "AccessDenied", "Access Denied.")
	return false
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

func (s *Server) listBuckets(w http.ResponseWriter) {
	type bucketXML struct {
		Name         string
		CreationDate string
	}
	out := struct {
		XMLName xml.Name `xml:"ListAllMyBucketsResult"`
		NS      string   `xml:"xmlns,attr"`
		Owner   owner
		Buckets []bucketXML `xml:"Buckets>Bucket"`
	}{NS: s3NS, Owner: defaultOwner}
	for _, b := range s.Store.ListBuckets() {
		out.Buckets = append(out.Buckets, bucketXML{b.Name, isoTime(b.Created)})
	}
	writeXML(w, http.StatusOK, out)
}

// --- buckets ---

func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, bucket string, q url.Values) {
	switch r.Method {
	case http.MethodPut:
		switch {
		case q.Has("policy"):
			body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			s.storeResult(w, r, s.Store.SetPolicy(bucket, string(body)), http.StatusNoContent)
		case len(q) == 0:
			if err := s.Store.CreateBucket(bucket); err != nil {
				s.storeErr(w, r, err)
				return
			}
			w.Header().Set("Location", "/"+bucket)
			w.WriteHeader(http.StatusOK)
		default:
			s.notImplemented(w, r)
		}
	case http.MethodHead:
		if _, err := s.Store.Bucket(bucket); err != nil {
			s.storeErr(w, r, err)
			return
		}
		w.Header().Set("X-Amz-Bucket-Region", s.Region)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		switch {
		case q.Has("policy"):
			s.storeResult(w, r, s.Store.SetPolicy(bucket, ""), http.StatusNoContent)
		case len(q) == 0:
			s.storeResult(w, r, s.Store.DeleteBucket(bucket), http.StatusNoContent)
		default:
			s.notImplemented(w, r)
		}
	case http.MethodPost:
		if q.Has("delete") {
			s.deleteObjects(w, r, bucket)
			return
		}
		s.notImplemented(w, r)
	case http.MethodGet:
		meta, err := s.Store.Bucket(bucket)
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		switch {
		case q.Has("location"):
			writeXML(w, http.StatusOK, struct {
				XMLName xml.Name `xml:"LocationConstraint"`
				NS      string   `xml:"xmlns,attr"`
				Region  string   `xml:",chardata"`
			}{NS: s3NS, Region: s.Region})
		case q.Has("policy"):
			if meta.Policy == "" {
				s.fail(w, r, http.StatusNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, meta.Policy)
		case q.Has("versioning"):
			writeXML(w, http.StatusOK, struct {
				XMLName xml.Name `xml:"VersioningConfiguration"`
				NS      string   `xml:"xmlns,attr"`
			}{NS: s3NS})
		case q.Has("uploads"), q.Has("cors"), q.Has("lifecycle"), q.Has("tagging"), q.Has("object-lock"), q.Has("encryption"), q.Has("acl"), q.Has("notification"), q.Has("replication"), q.Has("website"):
			s.notImplemented(w, r)
		default:
			s.listObjects(w, r, bucket, q)
		}
	default:
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	}
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
		Owner        *owner `xml:",omitempty"`
	}
	type commonPrefix struct{ Prefix string }
	contents := make([]content, 0, len(res.Objects))
	for _, o := range res.Objects {
		contents = append(contents, content{Key: enc(o.Key), LastModified: isoTime(o.LastModified), ETag: o.ETag, Size: o.Size, StorageClass: "STANDARD"})
	}
	prefixes := make([]commonPrefix, 0, len(res.CommonPrefixes))
	for _, p := range res.CommonPrefixes {
		prefixes = append(prefixes, commonPrefix{enc(p)})
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

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Quiet   bool
		Objects []struct{ Key string } `xml:"Object"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed")
		return
	}
	type deleted struct{ Key string }
	type delErr struct{ Key, Code, Message string }
	out := struct {
		XMLName xml.Name `xml:"DeleteResult"`
		NS      string   `xml:"xmlns,attr"`
		Deleted []deleted
		Error   []delErr
	}{NS: s3NS}
	for _, o := range req.Objects {
		if err := s.Store.DeleteObject(bucket, o.Key); err != nil {
			out.Error = append(out.Error, delErr{o.Key, err.Error(), err.Error()})
			continue
		}
		if !req.Quiet {
			out.Deleted = append(out.Deleted, deleted{o.Key})
		}
	}
	writeXML(w, http.StatusOK, out)
}

// --- objects ---

func (s *Server) objectOp(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	switch r.Method {
	case http.MethodPut:
		switch {
		case q.Has("uploadId") && q.Has("partNumber") && r.Header.Get("X-Amz-Copy-Source") != "":
			s.notImplemented(w, r) // UploadPartCopy
		case q.Has("uploadId") && q.Has("partNumber"):
			s.uploadPart(w, r, bucket, key, q)
		case r.Header.Get("X-Amz-Copy-Source") != "":
			s.copyObject(w, r, bucket, key)
		case len(q) == 0 || onlyAuthQuery(q):
			s.putObject(w, r, bucket, key)
		default:
			s.notImplemented(w, r)
		}
	case http.MethodGet, http.MethodHead:
		if q.Has("uploadId") {
			s.listParts(w, r, bucket, key, q)
			return
		}
		s.getObject(w, r, bucket, key, q)
	case http.MethodDelete:
		if id := q.Get("uploadId"); id != "" {
			s.storeResult(w, r, s.Store.AbortUpload(bucket, key, id), http.StatusNoContent)
			return
		}
		s.storeResult(w, r, s.Store.DeleteObject(bucket, key), http.StatusNoContent)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.createUpload(w, r, bucket, key)
		case q.Has("uploadId"):
			s.completeUpload(w, r, bucket, key, q.Get("uploadId"))
		default:
			s.notImplemented(w, r)
		}
	default:
		s.fail(w, r, http.StatusMethodNotAllowed, "MethodNotAllowed", "")
	}
}

// onlyAuthQuery is true for presigned PUTs, whose query carries nothing but the X-Amz-* auth pairs.
func onlyAuthQuery(q url.Values) bool {
	for k := range q {
		if !strings.HasPrefix(k, "X-Amz-") && !strings.HasPrefix(k, "x-amz-") {
			return false
		}
	}
	return true
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
	return meta
}

func requestBody(r *http.Request) io.Reader {
	if isAWSChunked(r) {
		return newChunkedReader(r.Body)
	}
	return r.Body
}

// checksumHashes are the x-amz-checksum-* algorithms PutObject answers for. The value is computed over the
// stored bytes, not copied from the request; a client-sent value is not compared against it.
// ponytail: no CRC64NVME (no SDK we serve defaults to it); add a crc64 table with poly 0x9a6c9329ac4bc9b5 if one does.
var checksumHashes = map[string]func() hash.Hash{
	"crc32":  func() hash.Hash { return crc32.NewIEEE() },
	"crc32c": func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"sha1":   sha1.New,
	"sha256": sha256.New,
}

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
	body := requestBody(r)
	alg := requestedChecksum(r)
	var sum hash.Hash
	if newHash, ok := checksumHashes[alg]; ok {
		sum = newHash()
		body = io.TeeReader(body, sum)
	}
	meta, err := s.Store.PutObject(bucket, objectMetaFromRequest(r, key), body)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	w.Header().Set("ETag", meta.ETag)
	if sum != nil {
		w.Header().Set("X-Amz-Checksum-"+alg, base64.StdEncoding.EncodeToString(sum.Sum(nil)))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	src, _, _ := strings.Cut(r.Header.Get("X-Amz-Copy-Source"), "?")
	if decoded, err := url.PathUnescape(src); err == nil {
		src = decoded
	}
	srcBucket, srcKey := splitPath("/" + strings.TrimPrefix(src, "/"))
	var replace *store.ObjectMeta
	if strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE") {
		m := objectMetaFromRequest(r, key)
		replace = &m
	}
	meta, err := s.Store.CopyObject(srcBucket, srcKey, bucket, key, replace)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		NS           string   `xml:"xmlns,attr"`
		LastModified string
		ETag         string
	}{NS: s3NS, LastModified: isoTime(meta.LastModified), ETag: meta.ETag})
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
	if r.Method == http.MethodHead {
		// HEAD never reads the body: answer from the in-memory index without touching the disk.
		m, err := s.Store.HeadObject(bucket, key)
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		meta, body = m, io.NewSectionReader(zeros{}, 0, m.Size)
	} else {
		m, f, err := s.Store.OpenObject(bucket, key)
		if err != nil {
			s.storeErr(w, r, err)
			return
		}
		defer f.Close()
		meta, body = m, f
	}
	h := w.Header()
	h.Set("Content-Type", meta.ContentType)
	h.Set("ETag", meta.ETag)
	h.Set("Accept-Ranges", "bytes")
	for name, v := range meta.Headers {
		h.Set(name, v)
	}
	for name, v := range meta.UserMeta {
		h.Set("X-Amz-Meta-"+name, v)
	}
	for param, header := range responseOverrides {
		if v := q.Get(param); v != "" {
			h.Set(header, v)
		}
	}
	// ServeContent handles Range, If-Range, If-None-Match and HEAD.
	http.ServeContent(w, r, "", meta.LastModified, body)
}

// zeros backs the HEAD body; ServeContent only seeks it (a multi-range HEAD may read and discard).
type zeros struct{}

func (zeros) ReadAt(p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}

// --- multipart ---

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id, err := s.Store.CreateUpload(bucket, objectMetaFromRequest(r, key))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Bucket   string
		Key      string
		UploadId string
	}{NS: s3NS, Bucket: bucket, Key: key, UploadId: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	n, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || n < 1 || n > 10000 {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
		return
	}
	etag, err := s.Store.PutPart(bucket, key, q.Get("uploadId"), n, requestBody(r))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed")
		return
	}
	parts := make([]store.Part, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = store.Part{Number: p.PartNumber, ETag: p.ETag}
	}
	meta, err := s.Store.CompleteUpload(bucket, key, id, parts)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		NS       string   `xml:"xmlns,attr"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{NS: s3NS, Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: meta.ETag})
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values) {
	parts, err := s.Store.ListParts(bucket, key, q.Get("uploadId"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	type partXML struct {
		PartNumber int
		ETag       string
		Size       int64
	}
	out := struct {
		XMLName  xml.Name `xml:"ListPartsResult"`
		NS       string   `xml:"xmlns,attr"`
		Bucket   string
		Key      string
		UploadId string
		Parts    []partXML `xml:"Part"`
	}{NS: s3NS, Bucket: bucket, Key: key, UploadId: q.Get("uploadId")}
	for _, p := range parts {
		out.Parts = append(out.Parts, partXML{p.Number, p.ETag, p.Size})
	}
	writeXML(w, http.StatusOK, out)
}

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
}

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
