package server_test

// Operations that common clients call beyond the basic object round trip: tagging, ACLs, CORS
// configuration, versions and uploads listings, conditional writes, the AWS CLI's default checksum and
// boto3's default (SigV2) presigned URLs.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc64"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

func put(t *testing.T, c *s3.Client, in *s3.PutObjectInput) {
	t.Helper()
	if _, err := c.PutObject(context.Background(), in); err != nil {
		t.Fatalf("PutObject %s: %v", aws.ToString(in.Key), err)
	}
}

// A subresource request must never fall through to the plain object operation: DELETE ?tagging once
// deleted the object itself.
func TestSubresourcesDoNotFallThrough(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("v"), Tagging: aws.String("env=test&team=a")})

	if _, err := c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); err != nil {
		t.Fatalf("DeleteObjectTagging: %v", err)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); err != nil {
		t.Fatalf("object gone after DeleteObjectTagging: %v", err)
	}
	if _, err := c.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); errCode(err) != "NotImplemented" {
		t.Fatalf("GetObjectRetention: want NotImplemented, got %v", err)
	}
	if _, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("bkt")}); errCode(err) != "NotImplemented" {
		t.Fatalf("GetObjectLockConfiguration: want NotImplemented, got %v", err)
	}
}

func TestObjectTagging(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("v"), Tagging: aws.String("env=test&team=a%20b")})

	tags := func() map[string]string {
		t.Helper()
		out, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
		if err != nil {
			t.Fatalf("GetObjectTagging: %v", err)
		}
		m := map[string]string{}
		for _, tag := range out.TagSet {
			m[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
		return m
	}
	if got := tags(); len(got) != 2 || got["team"] != "a b" {
		t.Fatalf("tags from x-amz-tagging: %v", got)
	}
	obj, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || aws.ToInt32(obj.TagCount) != 2 {
		t.Fatalf("GetObject tag count: %v %d", err, aws.ToInt32(obj.TagCount))
	}
	obj.Body.Close()
	if _, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("only"), Value: aws.String("one")}}}}); err != nil {
		t.Fatalf("PutObjectTagging: %v", err)
	}
	if got := tags(); len(got) != 1 || got["only"] != "one" {
		t.Fatalf("tags after PutObjectTagging: %v", got)
	}
	// CopyObject keeps the tags unless told to replace them.
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/k")}); err != nil {
		t.Fatal(err)
	}
	if out, _ := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("copy")}); out == nil || len(out.TagSet) != 1 {
		t.Fatalf("copy lost its tags: %+v", out)
	}
	if _, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("missing"),
		Tagging: &types.Tagging{TagSet: []types.Tag{}}}); errCode(err) != "NoSuchKey" {
		t.Fatalf("tagging a missing key: %v", err)
	}
}

func TestObjectACL(t *testing.T) {
	ctx := context.Background()
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("pub"), Body: strings.NewReader("pub"), ACL: types.ObjectCannedACLPublicRead})
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("priv"), Body: strings.NewReader("priv")})

	status := func(key string) int {
		resp, err := http.Get(ts.URL + "/bkt/" + key)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status("pub") != http.StatusOK || status("priv") != http.StatusForbidden {
		t.Fatalf("anonymous GET: public=%d private=%d", status("pub"), status("priv"))
	}
	// An anonymous SDK client adds ?x-id=GetObject; that must not count as a subresource.
	anon := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ts.URL), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}})
	if out, err := anon.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("pub")}); err != nil {
		t.Fatalf("anonymous SDK GetObject on a public-read object: %v", err)
	} else {
		out.Body.Close()
	}
	acl, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("pub")})
	if err != nil || len(acl.Grants) != 2 || acl.Grants[1].Permission != types.PermissionRead || aws.ToString(acl.Grants[1].Grantee.URI) != "http://acs.amazonaws.com/groups/global/AllUsers" {
		t.Fatalf("GetObjectAcl: %v %+v", err, acl)
	}
	if _, err := c.PutObjectAcl(ctx, &s3.PutObjectAclInput{Bucket: aws.String("bkt"), Key: aws.String("pub"), ACL: types.ObjectCannedACLPrivate}); err != nil {
		t.Fatalf("PutObjectAcl: %v", err)
	}
	if status("pub") != http.StatusForbidden {
		t.Fatal("object still public after PutObjectAcl private")
	}
	bacl, err := c.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String("bkt")})
	if err != nil || len(bacl.Grants) != 1 || bacl.Grants[0].Permission != types.PermissionFullControl {
		t.Fatalf("GetBucketAcl: %v %+v", err, bacl)
	}
}

func TestBucketCORSConfiguration(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	if _, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("bkt")}); errCode(err) != "NoSuchCORSConfiguration" {
		t.Fatalf("GetBucketCors before put: %v", err)
	}
	rule := types.CORSRule{AllowedOrigins: []string{"https://app.example"}, AllowedMethods: []string{"GET", "PUT"}, AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"}}
	if _, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: aws.String("bkt"), CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{rule}}}); err != nil {
		t.Fatalf("PutBucketCors: %v", err)
	}
	got, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("bkt")})
	if err != nil || len(got.CORSRules) != 1 || got.CORSRules[0].AllowedOrigins[0] != "https://app.example" || len(got.CORSRules[0].AllowedMethods) != 2 {
		t.Fatalf("GetBucketCors: %v %+v", err, got)
	}
	if _, err := c.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatalf("DeleteBucketCors: %v", err)
	}
	if _, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String("bkt")}); errCode(err) != "NoSuchCORSConfiguration" {
		t.Fatalf("GetBucketCors after delete: %v", err)
	}
}

// Bootstrap scripts enable versioning, lifecycle rules and default encryption; the settings are kept and
// reported back even though ss33 does not act on them.
func TestBucketConfigurationsAreStored(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	if v, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("bkt")}); err != nil || v.Status != "" {
		t.Fatalf("GetBucketVersioning before put: %v %q", err, v.Status)
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("bkt"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
		t.Fatalf("PutBucketVersioning: %v", err)
	}
	if v, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("bkt")}); err != nil || v.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("GetBucketVersioning: %v %q", err, v.Status)
	}

	if _, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("bkt")}); errCode(err) != "NoSuchLifecycleConfiguration" {
		t.Fatalf("GetBucketLifecycleConfiguration before put: %v", err)
	}
	rule := types.LifecycleRule{ID: aws.String("expire-tmp"), Status: types.ExpirationStatusEnabled,
		Filter: &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}}
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String("bkt"),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{rule}}}); err != nil {
		t.Fatalf("PutBucketLifecycleConfiguration: %v", err)
	}
	lc, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("bkt")})
	if err != nil || len(lc.Rules) != 1 || aws.ToString(lc.Rules[0].ID) != "expire-tmp" {
		t.Fatalf("GetBucketLifecycleConfiguration: %v %+v", err, lc)
	}

	if _, err := c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String("bkt"), ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
		Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}}}}); err != nil {
		t.Fatalf("PutBucketEncryption: %v", err)
	}
	if enc, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: aws.String("bkt")}); err != nil || len(enc.ServerSideEncryptionConfiguration.Rules) != 1 {
		t.Fatalf("GetBucketEncryption: %v", err)
	}

	if _, err := c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String("bkt"), Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("env"), Value: aws.String("dev")}}}}); err != nil {
		t.Fatalf("PutBucketTagging: %v", err)
	}
	if tg, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("bkt")}); err != nil || len(tg.TagSet) != 1 {
		t.Fatalf("GetBucketTagging: %v", err)
	}
	if _, err := c.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatalf("DeleteBucketLifecycle: %v", err)
	}
	if _, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("bkt")}); errCode(err) != "NoSuchLifecycleConfiguration" {
		t.Fatalf("lifecycle after delete: %v", err)
	}
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: aws.String("bkt"),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{}}); errCode(err) != "MalformedXML" && err == nil {
		t.Fatalf("empty lifecycle accepted")
	}
}

// "Empty this bucket" helpers list versions, not objects.
func TestListObjectVersions(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	for _, k := range []string{"a", "b", "c", "dir/d"} {
		put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String(k), Body: strings.NewReader(k)})
	}
	var keys []string
	p := s3.NewListObjectVersionsPaginator(c, &s3.ListObjectVersionsInput{Bucket: aws.String("bkt"), MaxKeys: aws.Int32(2)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatalf("ListObjectVersions: %v", err)
		}
		for _, v := range page.Versions {
			if aws.ToString(v.VersionId) != "null" || !aws.ToBool(v.IsLatest) {
				t.Fatalf("version: %+v", v)
			}
			keys = append(keys, aws.ToString(v.Key))
		}
	}
	if strings.Join(keys, ",") != "a,b,c,dir/d" {
		t.Fatalf("versions: %v", keys)
	}
	// Deleting the "null" version deletes the object.
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("a"), VersionId: aws.String("null")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("a")}); err == nil {
		t.Fatal("object survived DeleteObject with versionId=null")
	}
}

func TestListMultipartUploads(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	mustBucket(t, c, "other")
	var ids []string
	for _, k := range []string{"b/2", "a/1"} {
		out, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String(k)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, aws.ToString(out.UploadId))
	}
	if _, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("other"), Key: aws.String("x")}); err != nil {
		t.Fatal(err)
	}
	out, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("bkt")})
	if err != nil || len(out.Uploads) != 2 || aws.ToString(out.Uploads[0].Key) != "a/1" || aws.ToString(out.Uploads[0].UploadId) != ids[1] {
		t.Fatalf("ListMultipartUploads: %v %+v", err, out)
	}
	out, err = c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("bkt"), Prefix: aws.String("b/")})
	if err != nil || len(out.Uploads) != 1 {
		t.Fatalf("ListMultipartUploads with prefix: %v %+v", err, out)
	}
	c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("b/2"), UploadId: aws.String(ids[0])})
	if out, _ := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String("bkt")}); len(out.Uploads) != 1 {
		t.Fatalf("after abort: %+v", out.Uploads)
	}
}

func TestConditionalPut(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	in := func(body string) *s3.PutObjectInput {
		return &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("lock"), Body: strings.NewReader(body)}
	}
	first := in("first")
	first.IfNoneMatch = aws.String("*")
	out, err := c.PutObject(ctx, first)
	if err != nil {
		t.Fatalf("create-if-absent: %v", err)
	}
	again := in("second")
	again.IfNoneMatch = aws.String("*")
	if _, err := c.PutObject(ctx, again); errCode(err) != "PreconditionFailed" {
		t.Fatalf("create-if-absent over an existing key: %v", err)
	}
	stale := in("third")
	stale.IfMatch = aws.String(`"0123456789abcdef0123456789abcdef"`)
	if _, err := c.PutObject(ctx, stale); errCode(err) != "PreconditionFailed" {
		t.Fatalf("If-Match with a stale ETag: %v", err)
	}
	fresh := in("fourth")
	fresh.IfMatch = out.ETag
	if _, err := c.PutObject(ctx, fresh); err != nil {
		t.Fatalf("If-Match with the current ETag: %v", err)
	}
}

// The AWS CLI v2 sends CRC64NVME by default.
func TestPutObjectCRC64NVME(t *testing.T) {
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	body := []byte("123456789")
	out, err := c.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		Body: bytes.NewReader(body), ChecksumAlgorithm: types.ChecksumAlgorithmCrc64nvme})
	if err != nil {
		t.Fatal(err)
	}
	sum := crc64.Checksum(body, crc64.MakeTable(0x9a6c9329ac4bc9b5))
	if sum != 0xae8b14860a799888 { // the published CRC-64/NVME check value
		t.Fatalf("crc64nvme table: %x", sum)
	}
	want := base64.StdEncoding.EncodeToString(binary.BigEndian.AppendUint64(nil, sum))
	if aws.ToString(out.ChecksumCRC64NVME) != want {
		t.Fatalf("ChecksumCRC64NVME = %q, want %q", aws.ToString(out.ChecksumCRC64NVME), want)
	}
}

// boto3 sends user metadata keys as it reads them off the wire, so x-amz-meta-* must go out lowercase.
func TestUserMetadataHeadersAreLowercase(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	creds := sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}
	srv := &server.Server{Store: st, Creds: creds, Region: "us-east-1"}
	do := func(method, path string, header http.Header) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://s3.local"+path, strings.NewReader("x"))
		for k, v := range header {
			req.Header[k] = v
		}
		sigv4.Sign(req, creds, "us-east-1", time.Now())
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	do(http.MethodPut, "/bkt", nil)
	do(http.MethodPut, "/bkt/k", http.Header{"X-Amz-Meta-Owner": {"me"}})
	rec := do(http.MethodHead, "/bkt/k", nil)
	if got := rec.Result().Header["x-amz-meta-owner"]; len(got) != 1 || got[0] != "me" {
		t.Fatalf("HEAD headers: %v", rec.Result().Header)
	}
}

// presignV2 builds the URL boto3's generate_presigned_url returns without an s3v4 config.
func presignV2(base, method, path string, expires time.Time, secret string) string {
	exp := strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha1.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n\n\n%s\n%s", method, exp, path)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("%s%s?AWSAccessKeyId=%s&Signature=%s&Expires=%s", base, path, testAK, url.QueryEscape(sig), exp)
}

func TestPresignedV2(t *testing.T) {
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b.txt"), Body: strings.NewReader("hello")})

	get := func(u string) (int, string) {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, body := get(presignV2(ts.URL, "GET", "/bkt/dir/a%20b.txt", time.Now().Add(time.Minute), testSK)); code != http.StatusOK || body != "hello" {
		t.Fatalf("SigV2 presigned GET: %d %s", code, body)
	}
	if code, body := get(presignV2(ts.URL, "GET", "/bkt/dir/a%20b.txt", time.Now().Add(-time.Minute), testSK)); code != http.StatusForbidden || !strings.Contains(body, "expired") {
		t.Fatalf("expired SigV2 URL: %d %s", code, body)
	}
	if code, body := get(presignV2(ts.URL, "GET", "/bkt/dir/a%20b.txt", time.Now().Add(time.Minute), "wrong")); code != http.StatusForbidden || !strings.Contains(body, "SignatureDoesNotMatch") {
		t.Fatalf("wrong secret: %d %s", code, body)
	}
	req, _ := http.NewRequest(http.MethodPut, presignV2(ts.URL, "PUT", "/bkt/up.txt", time.Now().Add(time.Minute), testSK), strings.NewReader("uploaded"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SigV2 presigned PUT: %s", resp.Status)
	}
}

// Browser form uploads: the SDK signs a POST policy, the browser posts the fields and then the file.
func TestPresignedPost(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	ps := s3.NewPresignClient(c)
	post := func(key string, conditions []any, fields map[string]string, file string) (*http.Response, string) {
		t.Helper()
		req, err := ps.PresignPostObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String(key)}, func(o *s3.PresignPostOptions) {
			o.Conditions = conditions
		})
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range req.Values {
			mw.WriteField(k, v)
		}
		for k, v := range fields {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("file", "photo.txt")
		io.WriteString(fw, file)
		mw.Close()
		resp, err := http.Post(req.URL, mw.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp, string(out)
	}

	limit := []any{[]any{"content-length-range", 1, 10}, []any{"starts-with", "$Content-Type", "text/"}}
	resp, body := post("forms/${filename}", limit, map[string]string{"Content-Type": "text/plain", "x-amz-meta-owner": "me", "success_action_status": "201"}, "form-data")
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, "<Key>forms/photo.txt</Key>") {
		t.Fatalf("POST upload: %s %s", resp.Status, body)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("forms/photo.txt")})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if string(data) != "form-data" || aws.ToString(got.ContentType) != "text/plain" || got.Metadata["owner"] != "me" {
		t.Fatalf("posted object: %q type=%q meta=%v", data, aws.ToString(got.ContentType), got.Metadata)
	}

	if resp, body := post("big", limit, map[string]string{"Content-Type": "text/plain"}, "more than ten bytes"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "EntityTooLarge") {
		t.Fatalf("oversized POST: %s %s", resp.Status, body)
	}
	if resp, body := post("img", limit, map[string]string{"Content-Type": "image/png"}, "x"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Policy Condition failed") {
		t.Fatalf("POST violating starts-with: %s %s", resp.Status, body)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("big")}); err == nil {
		t.Fatal("rejected POST left an object behind")
	}
}

func TestGetObjectAttributes(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("hello")})
	all := []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesObjectSize, types.ObjectAttributesStorageClass, types.ObjectAttributesObjectParts}
	out, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("k"), ObjectAttributes: all})
	if err != nil || aws.ToString(out.ETag) != "5d41402abc4b2a76b9719d911017c592" || aws.ToInt64(out.ObjectSize) != 5 || out.StorageClass != types.StorageClassStandard || out.ObjectParts != nil || out.LastModified == nil {
		t.Fatalf("GetObjectAttributes: %v %+v", err, out)
	}

	mp, _ := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("mp")})
	var parts []types.CompletedPart
	for i, body := range []string{"one", "three"} {
		p, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), UploadId: mp.UploadId, PartNumber: aws.Int32(int32(i + 1)), Body: strings.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: p.ETag})
	}
	c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), UploadId: mp.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	out, err = c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}})
	if err != nil || out.ObjectParts == nil || aws.ToInt32(out.ObjectParts.TotalPartsCount) != 2 || aws.ToInt64(out.ObjectParts.Parts[1].Size) != 5 || out.ETag != nil {
		t.Fatalf("GetObjectAttributes parts: %v %+v", err, out.ObjectParts)
	}
}

// Health probes answer without credentials, under every path MinIO tooling polls.
func TestHealthProbes(t *testing.T) {
	ts, _ := newServer(t)
	for _, path := range []string{"/minio/health/live", "/minio/health/ready", "/minio/health/cluster", "/minio/health/cluster/read", "/healthz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, resp.StatusCode)
		}
	}
}
