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
	if _, err := c.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{Bucket: aws.String("bkt"), ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}}); errCode(err) != "NotImplemented" {
		t.Fatalf("PutObjectLockConfiguration: want NotImplemented, got %v", err)
	}
	// Reading it is answered as for any bucket without object lock.
	if _, err := c.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String("bkt")}); errCode(err) != "ObjectLockConfigurationNotFoundError" {
		t.Fatalf("GetObjectLockConfiguration: want ObjectLockConfigurationNotFoundError, got %v", err)
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
		body := "x"
		if strings.Count(path, "/") == 1 {
			body = "" // CreateBucket: a body would have to be a CreateBucketConfiguration
		}
		req := httptest.NewRequest(method, "http://s3.local"+path, strings.NewReader(body))
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

	// A form upload gets the bucket's default encryption, like any other write.
	if _, err := c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String("bkt"), ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
		Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}}}}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := post("encrypted", limit, map[string]string{"Content-Type": "text/plain"}, "x"); resp.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" {
		t.Fatalf("POST under a bucket default: %v", resp.Header)
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

// MinIO's ?metadata=true listing extension, which the official mc sends for `diff`, `mirror -a` and
// `find --metadata`: each object carries <UserMetadata> and <UserTags>.
func TestListWithMetadata(t *testing.T) {
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("v"),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"owner": "me"}, Tagging: aws.String("env=test")})

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/bkt/?list-type=2&fetch-owner=true&metadata=true&encoding-type=url", nil)
	sigv4.Sign(req, sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, "us-east-1", time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"<UserMetadata><content-type>text/plain</content-type><X-Amz-Meta-Owner>me</X-Amz-Meta-Owner></UserMetadata>", "<UserTags>env=test</UserTags>"} {
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Fatalf("%s: want %s in\n%s", resp.Status, want, body)
		}
	}
}

// ListParts pages with max-parts and always reports IsTruncated and the markers: the Docker registry's S3
// driver dereferences them and crashed when they were missing.
func TestListPartsPaging(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	for n := int32(1); n <= 3; n++ {
		if _, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("bkt"), Key: aws.String("k"), UploadId: up.UploadId,
			PartNumber: aws.Int32(n), Body: strings.NewReader("part")}); err != nil {
			t.Fatal(err)
		}
	}
	var got []int32
	in := &s3.ListPartsInput{Bucket: aws.String("bkt"), Key: aws.String("k"), UploadId: up.UploadId, MaxParts: aws.Int32(2)}
	for {
		page, err := c.ListParts(ctx, in)
		if err != nil || page.IsTruncated == nil || page.MaxParts == nil || page.NextPartNumberMarker == nil {
			t.Fatalf("ListParts: %v %+v", err, page)
		}
		for _, p := range page.Parts {
			got = append(got, aws.ToInt32(p.PartNumber))
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		in.PartNumberMarker = page.NextPartNumberMarker
	}
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("paged parts: %v", got)
	}
}

// GET ?partNumber=N serves one part of a multipart object, which multipart downloaders (the Java SDK's
// multipart client, the CRT) fetch in parallel. A single-part object has exactly one part.
func TestGetObjectPartNumber(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	first, second := bytes.Repeat([]byte("a"), 5<<20), []byte("tail")
	up, _ := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("mp")})
	var done []types.CompletedPart
	for i, body := range [][]byte{first, second} {
		n := int32(i + 1)
		p, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), UploadId: up.UploadId, PartNumber: &n, Body: bytes.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		done = append(done, types.CompletedPart{PartNumber: &n, ETag: p.ETag})
	}
	if _, err := c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done}}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), PartNumber: aws.Int32(2)})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(out.Body)
	out.Body.Close()
	if string(body) != "tail" || aws.ToInt32(out.PartsCount) != 2 || aws.ToString(out.ContentRange) != fmt.Sprintf("bytes %d-%d/%d", 5<<20, 5<<20+3, 5<<20+4) {
		t.Fatalf("part 2: %q parts=%d range=%q", body, aws.ToInt32(out.PartsCount), aws.ToString(out.ContentRange))
	}
	if h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), PartNumber: aws.Int32(1)}); err != nil || aws.ToInt64(h.ContentLength) != 5<<20 {
		t.Fatalf("HEAD part 1: %v %v", h, err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("mp"), PartNumber: aws.Int32(3)}); errCode(err) != "InvalidPartNumber" {
		t.Fatalf("part 3 of 2: %v", err)
	}

	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("single"), Body: strings.NewReader("whole")})
	one, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("single"), PartNumber: aws.Int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(one.Body)
	one.Body.Close()
	if string(body) != "whole" || one.PartsCount != nil {
		t.Fatalf("single-part object, part 1: %q parts=%v", body, one.PartsCount)
	}
}

// ListBuckets pages with max-buckets and a continuation token and filters by prefix (the 2024 API).
func TestListBucketsPaging(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	for _, b := range []string{"app-a", "app-b", "app-c", "other"} {
		mustBucket(t, c, b)
	}
	var names []string
	in := &s3.ListBucketsInput{Prefix: aws.String("app-"), MaxBuckets: aws.Int32(2)}
	for {
		page, err := c.ListBuckets(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Buckets {
			names = append(names, aws.ToString(b.Name))
		}
		if page.ContinuationToken == nil {
			break
		}
		in.ContinuationToken = page.ContinuationToken
	}
	if fmt.Sprint(names) != "[app-a app-b app-c]" {
		t.Fatalf("paged buckets: %v", names)
	}
}

// CompleteMultipartUpload honours If-None-Match: * and If-Match like PutObject, and a failed condition
// leaves the upload to retry.
func TestConditionalCompleteMultipartUpload(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("exists")})
	up, _ := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	part, err := c.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String("bkt"), Key: aws.String("k"), UploadId: up.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("new")})
	if err != nil {
		t.Fatal(err)
	}
	complete := &s3.CompleteMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("k"), UploadId: up.UploadId, IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}}
	if _, err := c.CompleteMultipartUpload(ctx, complete); errCode(err) != "PreconditionFailed" {
		t.Fatalf("If-None-Match: * over an existing key: %v", err)
	}
	complete.IfNoneMatch = nil
	if _, err := c.CompleteMultipartUpload(ctx, complete); err != nil {
		t.Fatalf("retry without the condition: %v", err)
	}
	if got := getBody(t, c, "bkt", "k", ""); got != "new" {
		t.Fatalf("after completing: %q", got)
	}
}

// CreateBucket can tag the bucket in its CreateBucketConfiguration; the Terraform AWS provider does.
func TestCreateBucketWithTags(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("bkt"), CreateBucketConfiguration: &types.CreateBucketConfiguration{
		Tags: []types.Tag{{Key: aws.String("team"), Value: aws.String("platform")}}}}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("bkt")})
	if err != nil || len(out.TagSet) != 1 || aws.ToString(out.TagSet[0].Value) != "platform" {
		t.Fatalf("GetBucketTagging: %+v %v", out, err)
	}
}

// The bucket configurations Terraform's aws_s3_bucket reads and its companion resources write: stored and
// returned, with S3's "not found" or default answers before they are set.
func TestTerraformBucketConfigurations(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	b := aws.String("bkt")

	if _, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b}); errCode(err) != "NoSuchPublicAccessBlockConfiguration" {
		t.Fatalf("GetPublicAccessBlock before Put: %v", err)
	}
	if _, err := c.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{Bucket: b, PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
		BlockPublicAcls: aws.Bool(true)}}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: b}); err != nil || !aws.ToBool(out.PublicAccessBlockConfiguration.BlockPublicAcls) {
		t.Fatalf("GetPublicAccessBlock: %+v %v", out, err)
	}
	if _, err := c.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: b}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.PutBucketOwnershipControls(ctx, &s3.PutBucketOwnershipControlsInput{Bucket: b, OwnershipControls: &types.OwnershipControls{
		Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerPreferred}}}}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.GetBucketOwnershipControls(ctx, &s3.GetBucketOwnershipControlsInput{Bucket: b}); err != nil || out.OwnershipControls.Rules[0].ObjectOwnership != types.ObjectOwnershipBucketOwnerPreferred {
		t.Fatalf("GetBucketOwnershipControls: %+v %v", out, err)
	}

	if _, err := c.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: b}); errCode(err) != "NoSuchWebsiteConfiguration" {
		t.Fatalf("GetBucketWebsite before Put: %v", err)
	}
	if _, err := c.PutBucketWebsite(ctx, &s3.PutBucketWebsiteInput{Bucket: b, WebsiteConfiguration: &types.WebsiteConfiguration{
		IndexDocument: &types.IndexDocument{Suffix: aws.String("index.html")}}}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{Bucket: b}); err != nil || aws.ToString(out.IndexDocument.Suffix) != "index.html" {
		t.Fatalf("GetBucketWebsite: %+v %v", out, err)
	}
	if _, err := c.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: b}); errCode(err) != "ReplicationConfigurationNotFoundError" {
		t.Fatalf("GetBucketReplication: %v", err)
	}

	// Read before they are set: S3's defaults.
	if out, err := c.GetBucketRequestPayment(ctx, &s3.GetBucketRequestPaymentInput{Bucket: b}); err != nil || out.Payer != types.PayerBucketOwner {
		t.Fatalf("GetBucketRequestPayment: %+v %v", out, err)
	}
	if out, err := c.GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: b}); err != nil || out.LoggingEnabled != nil {
		t.Fatalf("GetBucketLogging: %+v %v", out, err)
	}
	if out, err := c.GetBucketAccelerateConfiguration(ctx, &s3.GetBucketAccelerateConfigurationInput{Bucket: b}); err != nil || out.Status != "" {
		t.Fatalf("GetBucketAccelerateConfiguration: %+v %v", out, err)
	}

	// The Terraform provider waits until the lifecycle's transition size reads back as it sent it.
	if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: b,
		TransitionDefaultMinimumObjectSize: types.TransitionDefaultMinimumObjectSizeVariesByStorageClass,
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{ID: aws.String("r"), Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilter{Prefix: aws.String("tmp/")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(7)}}}}}); err != nil {
		t.Fatal(err)
	}
	lc, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: b})
	if err != nil || lc.TransitionDefaultMinimumObjectSize != types.TransitionDefaultMinimumObjectSizeVariesByStorageClass || len(lc.Rules) != 1 {
		t.Fatalf("GetBucketLifecycleConfiguration: %+v %v", lc, err)
	}
}
