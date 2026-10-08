package server_test

// Encryption is reported, not applied: SSE headers round-trip, a bucket default applies, SSE-C objects need
// their key to be read, and storage classes are kept.

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestServerSideEncryptionHeaders(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")

	out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("kms"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("alias/app"), StorageClass: types.StorageClassStandardIa})
	if err != nil || out.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(out.SSEKMSKeyId) != "alias/app" {
		t.Fatalf("PutObject SSE-KMS: %+v %v", out, err)
	}
	h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("kms")})
	if err != nil || h.ServerSideEncryption != types.ServerSideEncryptionAwsKms || h.StorageClass != types.StorageClassStandardIa {
		t.Fatalf("HeadObject: %+v %v", h, err)
	}
	if l, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt")}); l.Contents[0].StorageClass != types.ObjectStorageClassStandardIa {
		t.Fatalf("listed storage class: %q", l.Contents[0].StorageClass)
	}
	if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("bad"), Body: strings.NewReader("x"),
		StorageClass: "NOT_A_CLASS"}); errCode(err) != "InvalidStorageClass" {
		t.Fatalf("unknown storage class: %v", err)
	}

	// The bucket default applies to objects written without their own encryption, copies included.
	if _, err := c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: aws.String("bkt"), ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
		Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}}}}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("plain"), Body: strings.NewReader("x")}); err != nil || out.ServerSideEncryption != types.ServerSideEncryptionAes256 {
		t.Fatalf("PutObject under a bucket default: %+v %v", out, err)
	}
	if cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/kms")}); err != nil || cp.ServerSideEncryption != types.ServerSideEncryptionAes256 {
		t.Fatalf("CopyObject takes the destination's encryption: %+v %v", cp, err)
	}
}

func TestCustomerProvidedKeys(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	key := func(b byte) (string, string) {
		k := []byte(strings.Repeat(string(rune(b)), 32))
		sum := md5.Sum(k)
		return base64.StdEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(sum[:])
	}
	k1, k1md5 := key('a')
	k2, k2md5 := key('b')
	out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("secret"), Body: strings.NewReader("s"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1), SSECustomerKeyMD5: aws.String(k1md5)})
	if err != nil || aws.ToString(out.SSECustomerKeyMD5) != k1md5 {
		t.Fatalf("PutObject SSE-C: %+v %v", out, err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("secret")}); errCode(err) != "InvalidRequest" {
		t.Fatalf("GET without the key: %v", err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("secret"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k2), SSECustomerKeyMD5: aws.String(k2md5)}); errCode(err) != "AccessDenied" {
		t.Fatalf("GET with another key: %v", err)
	}
	got, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("secret"),
		SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(k1), SSECustomerKeyMD5: aws.String(k1md5)})
	if err != nil {
		t.Fatalf("GET with the key: %v", err)
	}
	got.Body.Close()
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/secret")}); errCode(err) != "InvalidRequest" {
		t.Fatalf("copy without the source key: %v", err)
	}
	if _, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("copy"), CopySource: aws.String("bkt/secret"),
		CopySourceSSECustomerAlgorithm: aws.String("AES256"), CopySourceSSECustomerKey: aws.String(k1), CopySourceSSECustomerKeyMD5: aws.String(k1md5)}); err != nil {
		t.Fatalf("copy with the source key: %v", err)
	}
	if _, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("copy")}); err != nil {
		t.Fatalf("the copy was written without a customer key, so it reads without one: %v", err)
	}
}
