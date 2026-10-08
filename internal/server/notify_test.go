package server_test

// Bucket notifications to a webhook, configured the MinIO way.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

type event struct {
	EventName string
	Key       string
	Records   []struct {
		EventName string `json:"eventName"`
		S3        struct {
			Object struct {
				Key  string `json:"key"`
				Size int64  `json:"size"`
			} `json:"object"`
		} `json:"s3"`
	}
}

func TestWebhookNotifications(t *testing.T) {
	ctx := context.Background()
	events := make(chan event, 16)
	auth := make(chan string, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &e); err != nil {
			t.Errorf("event body: %v: %s", err, body)
		}
		auth <- r.Header.Get("Authorization")
		events <- e
	}))
	defer hook.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: testAK, SecretKey: testSK}, Region: "us-east-1",
		Webhooks: server.WebhooksFromEnv([]string{
			"MINIO_NOTIFY_WEBHOOK_ENABLE_PRIMARY=on",
			"MINIO_NOTIFY_WEBHOOK_ENDPOINT_PRIMARY=" + hook.URL,
			"MINIO_NOTIFY_WEBHOOK_AUTH_TOKEN_PRIMARY=s3cret",
		})}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(ts.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(testAK, testSK, "")})
	mustBucket(t, c, "bkt")

	if _, err := c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String("bkt"),
		NotificationConfiguration: &types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
			QueueArn: aws.String("arn:aws:sqs:us-east-1:123456789012:queue"), Events: []types.Event{"s3:ObjectCreated:*"}}}}}); errCode(err) != "InvalidArgument" {
		t.Fatalf("a queue ss33 cannot deliver to: %v", err)
	}
	if _, err := c.PutBucketNotificationConfiguration(ctx, &s3.PutBucketNotificationConfigurationInput{Bucket: aws.String("bkt"),
		NotificationConfiguration: &types.NotificationConfiguration{QueueConfigurations: []types.QueueConfiguration{{
			Id: aws.String("uploads"), QueueArn: aws.String("arn:minio:sqs::PRIMARY:webhook"),
			Events: []types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:*"},
			Filter: &types.NotificationConfigurationFilter{Key: &types.S3KeyFilter{FilterRules: []types.FilterRule{{Name: "prefix", Value: aws.String("in/")}}}},
		}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("bkt")})
	if err != nil || len(got.QueueConfigurations) != 1 {
		t.Fatalf("GetBucketNotificationConfiguration: %+v %v", got, err)
	}

	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("out/skipped.txt"), Body: strings.NewReader("x")})
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("in/a b.txt"), Body: strings.NewReader("hello")})
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("in/a b.txt")}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ name, key string }{{"s3:ObjectCreated:Put", "in/a b.txt"}, {"s3:ObjectRemoved:Delete", "in/a b.txt"}} {
		select {
		case e := <-events:
			if e.EventName != want.name || e.Key != "bkt/"+want.key || len(e.Records) != 1 || e.Records[0].S3.Object.Key != "in%2Fa+b.txt" {
				t.Fatalf("event: %+v, want %s on %s", e, want.name, want.key)
			}
			if a := <-auth; a != "Bearer s3cret" {
				t.Fatalf("Authorization: %q", a)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s event", want.name)
		}
	}
	select {
	case e := <-events:
		t.Fatalf("unexpected event %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}
