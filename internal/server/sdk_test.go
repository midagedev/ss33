package server_test

// The AWS SDK is the compatibility oracle: every supported operation is driven through aws-sdk-go-v2,
// so signing, aws-chunked bodies, checksums and XML shapes are exercised the way real clients send them.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

const (
	testAK = "testkey"
	testSK = "testsecret"
)

func newServer(t *testing.T) (*httptest.Server, *s3.Client) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, Region: "us-east-1"})
	t.Cleanup(ts.Close)
	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(ts.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(testAK, testSK, ""),
	})
	return ts, client
}

func mustBucket(t *testing.T, c *s3.Client, name string) {
	t.Helper()
	if _, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
}

func errCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func TestObjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")

	body := []byte("hello ss33")
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt"), Body: bytes.NewReader(body),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"owner": "me"},
		ContentDisposition: aws.String(`attachment; filename="x.txt"`),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(data, body) || aws.ToString(got.ContentType) != "text/plain" || got.Metadata["owner"] != "me" ||
		aws.ToString(got.ContentDisposition) != `attachment; filename="x.txt"` {
		t.Fatalf("GetObject mismatch: %q %q %v %q", data, aws.ToString(got.ContentType), got.Metadata, aws.ToString(got.ContentDisposition))
	}

	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")})
	if err != nil || aws.ToInt64(head.ContentLength) != int64(len(body)) {
		t.Fatalf("HeadObject: %v len=%d", err, aws.ToInt64(head.ContentLength))
	}

	rng, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt"), Range: aws.String("bytes=6-9")})
	if err != nil {
		t.Fatalf("ranged GetObject: %v", err)
	}
	part, _ := io.ReadAll(rng.Body)
	rng.Body.Close()
	if string(part) != "ss33" {
		t.Fatalf("range = %q", part)
	}

	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	_, err = c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("dir/a b+c.txt")})
	var nsk *types.NoSuchKey
	if !errors.As(err, &nsk) {
		t.Fatalf("GetObject after delete: want NoSuchKey, got %v", err)
	}
}

// An unseekable body over TLS makes the SDK send aws-chunked framing with a trailing checksum
// (STREAMING-UNSIGNED-PAYLOAD-TRAILER) — the shape the Java SDK uses for ordinary uploads.
func TestStreamingUpload(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewTLSServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, Region: "us-east-1"})
	defer ts.Close()
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ts.URL), UsePathStyle: true, HTTPClient: ts.Client(),
		Credentials: credentials.NewStaticCredentialsProvider(testAK, testSK, "")})
	mustBucket(t, c, "bkt")
	payload := bytes.Repeat([]byte("0123456789"), 20000)
	_, err = c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("bkt"), Key: aws.String("stream.bin"),
		Body: io.NopCloser(bytes.NewReader(payload)), ContentLength: aws.Int64(int64(len(payload))),
	})
	if err != nil {
		t.Fatalf("streaming PutObject: %v", err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("stream.bin")})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(data, payload) {
		t.Fatalf("streamed object corrupted: len %d want %d", len(data), len(payload))
	}
}

func TestListObjectsV2(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	for _, k := range []string{"a/1", "a/2", "a/sub/3", "b/4", "c"} {
		if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String(k), Body: strings.NewReader(k)}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt"), Delimiter: aws.String("/")})
	if err != nil {
		t.Fatal(err)
	}
	var prefixes []string
	for _, p := range out.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	if fmt.Sprint(prefixes) != "[a/ b/]" || len(out.Contents) != 1 || aws.ToString(out.Contents[0].Key) != "c" {
		t.Fatalf("delimiter listing: prefixes=%v contents=%d", prefixes, len(out.Contents))
	}

	var keys []string
	p := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String("bkt"), Prefix: aws.String("a/"), MaxKeys: aws.Int32(1)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	if fmt.Sprint(keys) != "[a/1 a/2 a/sub/3]" {
		t.Fatalf("paginated keys = %v", keys)
	}
}

func TestMultipartAndCopy(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	payload := make([]byte, 12<<20)
	rand.Read(payload)
	up := manager.NewUploader(c, func(u *manager.Uploader) { u.PartSize = 5 << 20 })
	if _, err := up.Upload(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("big.bin"), Body: bytes.NewReader(payload)}); err != nil {
		t.Fatalf("multipart upload: %v", err)
	}
	head, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("big.bin")})
	if err != nil || aws.ToInt64(head.ContentLength) != int64(len(payload)) || !strings.HasSuffix(strings.Trim(aws.ToString(head.ETag), `"`), "-3") {
		t.Fatalf("multipart head: %v len=%d etag=%s", err, aws.ToInt64(head.ContentLength), aws.ToString(head.ETag))
	}

	mustBucket(t, c, "other")
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("other"), Key: aws.String("copy.bin"), CopySource: aws.String("bkt/big.bin")}); err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("other"), Key: aws.String("copy.bin")})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if !bytes.Equal(data, payload) {
		t.Fatal("copied object differs")
	}

	// Abort leaves nothing behind.
	created, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("aborted")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("aborted"), UploadId: created.UploadId}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
}

func TestPresignedGetAndPut(t *testing.T) {
	ctx := context.Background()
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	ps := s3.NewPresignClient(c)

	put, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("up/file name.txt"), ContentType: aws.String("text/plain")})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, put.URL, strings.NewReader("browser upload"))
	for k, v := range put.SignedHeader {
		req.Header[k] = v
	}
	req.Header.Set("Origin", "https://app.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("presigned PUT: %s acao=%q", resp.Status, resp.Header.Get("Access-Control-Allow-Origin"))
	}

	get, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("up/file name.txt"),
		ResponseContentDisposition: aws.String("attachment")}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(get.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(data) != "browser upload" || resp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("presigned GET: %s %q cd=%q", resp.Status, data, resp.Header.Get("Content-Disposition"))
	}

	// Tampering with the signed path must fail.
	resp, err = http.Get(strings.Replace(get.URL, "file%20name", "other", 1))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("tampered presign: %s", resp.Status)
	}

	// Anonymous access is refused until the bucket is made public.
	resp, _ = http.Get(ts.URL + "/bkt/up/file%20name.txt")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous GET on private bucket: %s", resp.Status)
	}
}

func TestPublicBucketAndErrors(t *testing.T) {
	ctx := context.Background()
	ts, c := newServer(t)
	mustBucket(t, c, "pub")
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("pub"), Key: aws.String("x"), Body: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::pub/*"]}]}`
	if _, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("pub"), Policy: aws.String(policy)}); err != nil {
		t.Fatalf("PutBucketPolicy: %v", err)
	}
	resp, _ := http.Get(ts.URL + "/pub/x")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET on public bucket: %s", resp.Status)
	}
	resp, _ = http.Post(ts.URL+"/pub/y?uploads", "text/plain", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous write on public bucket: %s", resp.Status)
	}

	if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("pub")}); errCode(err) != "BucketAlreadyOwnedByYou" {
		t.Fatalf("duplicate bucket: %v", err)
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("pub")}); errCode(err) != "BucketNotEmpty" {
		t.Fatalf("delete non-empty bucket: %v", err)
	}
	if _, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("missing")}); err == nil {
		t.Fatal("HeadBucket on missing bucket succeeded")
	}
	del, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("pub"), Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("x")}}}})
	if err != nil || len(del.Deleted) != 1 {
		t.Fatalf("DeleteObjects: %v %+v", err, del)
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("pub")}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}

	bad := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ts.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(testAK, "wrong", "")})
	if _, err := bad.ListBuckets(ctx, &s3.ListBucketsInput{}); errCode(err) != "SignatureDoesNotMatch" {
		t.Fatalf("wrong secret: %v", err)
	}
}
