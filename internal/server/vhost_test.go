package server_test

// Virtual-hosted addressing: <bucket>.localhost always, <bucket>.<domain> for configured domains
// (MINIO_DOMAIN). The SDKs' default style, presigned URLs in both signature versions, and path style on
// the same server.

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

// dialAll sends every connection to addr whatever the host name, so <bucket>.localhost and made-up
// domains need no DNS.
func dialAll(addr string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}}
}

func TestVirtualHostedAddressing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: testAK, SecretKey: testSK},
		Region: "us-east-1", Domains: []string{"s3.example.test"}})
	t.Cleanup(ts.Close)
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	httpc := dialAll(ts.Listener.Addr().String())

	for _, host := range []string{"localhost", "s3.example.test"} {
		t.Run(host, func(t *testing.T) {
			endpoint := "http://" + host + ":" + port
			c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), HTTPClient: httpc,
				Credentials: credentials.NewStaticCredentialsProvider(testAK, testSK, "")}) // virtual-hosted: the SDK default
			bucket := "vh-" + strings.ReplaceAll(host, ".", "-")
			mustBucket(t, c, bucket)
			put(t, c, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("dir/a b.txt"), Body: strings.NewReader("hello")})
			if got := getBody(t, c, bucket, "dir/a b.txt", ""); got != "hello" {
				t.Fatalf("GET: %q", got)
			}
			list, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("dir/")})
			if err != nil || len(list.Contents) != 1 {
				t.Fatalf("ListObjectsV2: %v %+v", err, list)
			}

			// Presigned SigV4 GET, fetched with a plain HTTP client.
			url4, err := s3.NewPresignClient(c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("dir/a b.txt")})
			if err != nil || !strings.HasPrefix(url4.URL, "http://"+bucket+"."+host) {
				t.Fatalf("presign: %v %v", url4, err)
			}
			expectGet(t, httpc, url4.URL, "hello")

			// Presigned SigV2 GET as boto3 builds it for addressing_style="virtual": the canonical resource
			// keeps the bucket even though the path does not.
			expires := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
			mac := hmac.New(sha1.New, []byte(testSK))
			mac.Write([]byte("GET\n\n\n" + expires + "\n/" + bucket + "/dir/a%20b.txt"))
			url2 := "http://" + bucket + "." + host + ":" + port + "/dir/a%20b.txt?AWSAccessKeyId=" + testAK + "&Expires=" + expires +
				"&Signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			expectGet(t, httpc, url2, "hello")
		})
	}

	// Path style keeps working on the same server, including a host that only looks like a domain.
	path := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://localhost:" + port), HTTPClient: httpc, UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(testAK, testSK, "")})
	if got := getBody(t, path, "vh-localhost", "dir/a b.txt", ""); got != "hello" {
		t.Fatalf("path-style GET: %q", got)
	}
}

func expectGet(t *testing.T, c *http.Client, u, want string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("GET %s: %s %s", u, resp.Status, body)
	}
}
