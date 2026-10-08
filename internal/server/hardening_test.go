package server_test

// Requests a careless or hostile client can send: policies that deny, stale signatures, malformed
// bodies. Each case once got a wrong answer.

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/midagedev/ss33/internal/sigv4"
)

func anonymousStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Anonymous access follows the policy's Effect, Action and Resource, not a substring search for
// "s3:GetObject" and "*".
func TestBucketPolicyEvaluation(t *testing.T) {
	ctx := context.Background()
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("secret"), Body: strings.NewReader("s")})
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("pub/a"), Body: strings.NewReader("a")})

	stmt := func(effect, action, resource string) string {
		return `{"Effect":"` + effect + `","Principal":"*","Action":"` + action + `","Resource":"arn:aws:s3:::` + resource + `"}`
	}
	for _, tc := range []struct {
		name       string
		statements string
		want       map[string]int // path -> anonymous GET status
	}{
		{"deny", stmt("Deny", "s3:GetObject", "bkt/*"),
			map[string]int{"/bkt/secret": 403, "/bkt": 403, "/bkt?policy": 403}},
		{"allow another action", stmt("Allow", "s3:GetObjectTagging", "bkt/*"),
			map[string]int{"/bkt/secret": 403}},
		{"allow a prefix", stmt("Allow", "s3:GetObject", "bkt/pub/*"),
			map[string]int{"/bkt/pub/a": 200, "/bkt/secret": 403, "/bkt": 403}},
		{"allow, then deny one key", stmt("Allow", "s3:*", "bkt/*") + "," + stmt("Deny", "s3:GetObject", "bkt/secret"),
			map[string]int{"/bkt/pub/a": 200, "/bkt/secret": 403}},
		{"allow listing", stmt("Allow", "s3:ListBucket", "bkt") + "," + stmt("Allow", "s3:GetObject", "bkt/*"),
			map[string]int{"/bkt": 200, "/bkt?list-type=2": 200, "/bkt/secret": 200, "/bkt?policy": 403}},
		{"conditional allow", `{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.1/32"}}}`,
			map[string]int{"/bkt/secret": 403}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := `{"Version":"2012-10-17","Statement":[` + tc.statements + `]}`
			if _, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("bkt"), Policy: aws.String(policy)}); err != nil {
				t.Fatal(err)
			}
			for path, want := range tc.want {
				if got := anonymousStatus(t, ts.URL+path); got != want {
					t.Errorf("anonymous GET %s: %d, want %d", path, got, want)
				}
			}
		})
	}
}

// A captured Authorization header must not work forever.
func TestHeaderSignatureTimeSkew(t *testing.T) {
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	for _, age := range []time.Duration{time.Hour, -time.Hour} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/bkt/k", strings.NewReader("v"))
		sigv4.Sign(req, sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, "us-east-1", time.Now().Add(-age))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "<Code>RequestTimeTooSkewed</Code>") {
			t.Fatalf("request signed %v ago: %s %s, want 403 RequestTimeTooSkewed", age, resp.Status, body)
		}
	}
}

// A completion without parts is malformed; it once stored an object that became unreadable on restart.
func TestCompleteWithoutParts(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	up, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("bkt"), Key: aws.String("k"),
		UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{}})
	if errCode(err) != "MalformedXML" {
		t.Fatalf("CompleteMultipartUpload with no parts: want MalformedXML, got %v", err)
	}
}

// A negative aws-chunked size once panicked the handler and dropped the connection.
func TestMalformedChunkedBody(t *testing.T) {
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/bkt/k", strings.NewReader("-1\r\nabc\r\n0\r\n\r\n"))
	req.Header.Set("Content-Encoding", "aws-chunked")
	sigv4.Sign(req, sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, "us-east-1", time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "<Code>IncompleteBody</Code>") {
		t.Fatalf("negative chunk size: %s %s, want 400 IncompleteBody", resp.Status, body)
	}
}

// `mc anonymous set public` grants anonymous writes as well; `download` grants reads and listing only.
func TestAnonymousWritesFollowPolicy(t *testing.T) {
	ctx := context.Background()
	ts, c := newServer(t)
	mustBucket(t, c, "bkt")
	anon := func(method, path string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader("anon"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := anon(http.MethodPut, "/bkt/k"); got != http.StatusForbidden {
		t.Fatalf("anonymous PUT without a policy: %d", got)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::bkt/*"]}]}`
	if _, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("bkt"), Policy: aws.String(policy)}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPut, "/bkt/k", 200},
		{http.MethodGet, "/bkt/k", 200},
		{http.MethodPut, "/bkt/k?tagging", 403},
		{http.MethodDelete, "/bkt/k", 204},
		{http.MethodDelete, "/bkt", 403},
	} {
		if got := anon(step.method, step.path); got != step.want {
			t.Errorf("anonymous %s %s: %d, want %d", step.method, step.path, got, step.want)
		}
	}
}

// A body that does not match its declared Content-MD5 or x-amz-checksum-* is refused and not stored, as
// S3 does; integrity tests depend on it.
func TestDeclaredDigestsAreChecked(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	wrongMD5 := base64.StdEncoding.EncodeToString(md5.New().Sum(nil)) // the MD5 of nothing
	for _, tc := range []struct {
		name string
		in   *s3.PutObjectInput
		code string
	}{
		{"wrong Content-MD5", &s3.PutObjectInput{ContentMD5: aws.String(wrongMD5)}, "BadDigest"},
		{"malformed Content-MD5", &s3.PutObjectInput{ContentMD5: aws.String("nope")}, "InvalidDigest"},
		{"wrong CRC32", &s3.PutObjectInput{ChecksumCRC32: aws.String("AAAAAA==")}, "BadDigest"},
		{"wrong SHA256", &s3.PutObjectInput{ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(make([]byte, 32)))}, "BadDigest"},
	} {
		tc.in.Bucket, tc.in.Key, tc.in.Body = aws.String("bkt"), aws.String("k"), strings.NewReader("payload")
		if _, err := c.PutObject(ctx, tc.in); errCode(err) != tc.code {
			t.Errorf("%s: want %s, got %v", tc.name, tc.code, err)
		}
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); errCode(err) != "NotFound" {
		t.Fatalf("an upload with a bad digest was stored: %v", err)
	}
	sum := md5.Sum([]byte("payload"))
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("payload"),
		ContentMD5: aws.String(base64.StdEncoding.EncodeToString(sum[:]))}); err != nil {
		t.Fatalf("correct Content-MD5: %v", err)
	}
}
