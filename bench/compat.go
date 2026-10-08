package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// compatChecks are S3 features that dev stacks and test suites commonly rely on beyond plain PUT/GET. Each
// check passes or returns why it did not; a server is never asked to fake support.
func compatChecks(c *s3.Client, ep, ak, sk, b string) []struct {
	name string
	fn   func() error
} {
	str := aws.String
	putText := func(key, body string, edit ...func(*s3.PutObjectInput)) error {
		in := &s3.PutObjectInput{Bucket: &b, Key: str(key), Body: strings.NewReader(body)}
		for _, e := range edit {
			e(in)
		}
		_, err := c.PutObject(ctx, in)
		return err
	}
	// anonRead checks that privateURL is refused and publicURL served without credentials: a server that
	// lets everyone read everything must not pass.
	anonRead := func(privateURL, publicURL string) error {
		for _, u := range []struct {
			url  string
			want int
		}{{privateURL, http.StatusForbidden}, {publicURL, http.StatusOK}} {
			resp, err := http.Get(u.url)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != u.want {
				return fmt.Errorf("anonymous GET %s: %s, want %d", u.url, resp.Status, u.want)
			}
		}
		return nil
	}
	return []struct {
		name string
		fn   func() error
	}{
		{"SigV4 presigned GET", func() error {
			if err := putText("presign.txt", "hi"); err != nil {
				return err
			}
			req, err := s3.NewPresignClient(c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &b, Key: str("presign.txt")})
			if err != nil {
				return err
			}
			return expectBody(http.Get(req.URL))
		}},
		{"SigV2 presigned GET (boto3 default)", func() error {
			if err := putText("presign.txt", "hi"); err != nil {
				return err
			}
			exp := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
			mac := hmac.New(sha1.New, []byte(sk))
			fmt.Fprintf(mac, "GET\n\n\n%s\n/%s/presign.txt", exp, b)
			sig := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			return expectBody(http.Get(fmt.Sprintf("%s/%s/presign.txt?AWSAccessKeyId=%s&Signature=%s&Expires=%s", ep, b, ak, sig, exp)))
		}},
		{"Presigned POST (browser form upload)", func() error {
			req, err := s3.NewPresignClient(c).PresignPostObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: str("form.txt")})
			if err != nil {
				return err
			}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			for k, v := range req.Values {
				mw.WriteField(k, v)
			}
			fw, _ := mw.CreateFormFile("file", "form.txt")
			io.WriteString(fw, "form")
			mw.Close()
			resp, err := http.Post(req.URL, mw.FormDataContentType(), &body)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				return fmt.Errorf("POST: %s", resp.Status)
			}
			_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: str("form.txt")})
			return err
		}},
		{"Bucket policy: anonymous read", func() error {
			pb := b + "-pub"
			if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &pb}); err != nil {
				return err
			}
			policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/*"]}]}`, pb)
			if _, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: &pb, Policy: &policy}); err != nil {
				return err
			}
			if _, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &pb, Key: str("x"), Body: strings.NewReader("x")}); err != nil {
				return err
			}
			if err := putText("private.txt", "p"); err != nil {
				return err
			}
			return anonRead(ep+"/"+b+"/private.txt", ep+"/"+pb+"/x")
		}},
		{"Object ACL public-read: anonymous read", func() error {
			if err := putText("acl.txt", "pub", func(in *s3.PutObjectInput) { in.ACL = types.ObjectCannedACLPublicRead }); err != nil {
				return err
			}
			if err := putText("private.txt", "p"); err != nil {
				return err
			}
			return anonRead(ep+"/"+b+"/private.txt", ep+"/"+b+"/acl.txt")
		}},
		{"GetObjectAcl", func() error {
			_, err := c.GetObjectAcl(ctx, &s3.GetObjectAclInput{Bucket: &b, Key: str("acl.txt")})
			return err
		}},
		{"Object tagging", func() error {
			if err := putText("tag.txt", "t", func(in *s3.PutObjectInput) { in.Tagging = str("env=test") }); err != nil {
				return err
			}
			out, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: &b, Key: str("tag.txt")})
			if err != nil {
				return err
			}
			if len(out.TagSet) != 1 {
				return fmt.Errorf("%d tags, want 1", len(out.TagSet))
			}
			if _, err := c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: &b, Key: str("tag.txt")}); err != nil {
				return err
			}
			_, err = c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: str("tag.txt")})
			return err
		}},
		{"Conditional PUT (If-None-Match: *)", func() error {
			if err := putText("lock", "1", func(in *s3.PutObjectInput) { in.IfNoneMatch = str("*") }); err != nil {
				return err
			}
			err := putText("lock", "2", func(in *s3.PutObjectInput) { in.IfNoneMatch = str("*") })
			var api smithy.APIError
			if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
				return nil
			}
			return fmt.Errorf("overwrite with If-None-Match: * gave %v", err)
		}},
		{"CRC64NVME checksum (AWS CLI v2 default)", func() error {
			out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: &b, Key: str("crc"), Body: strings.NewReader("123456789"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc64nvme})
			if err != nil {
				return err
			}
			if aws.ToString(out.ChecksumCRC64NVME) != "rosUhgp5mIg=" {
				return fmt.Errorf("ChecksumCRC64NVME = %q", aws.ToString(out.ChecksumCRC64NVME))
			}
			return nil
		}},
		{"UploadPartCopy", func() error {
			if err := putText("src", strings.Repeat("x", 1<<20)); err != nil {
				return err
			}
			mp, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b, Key: str("pc")})
			if err != nil {
				return err
			}
			part, err := c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: &b, Key: str("pc"), UploadId: mp.UploadId, PartNumber: aws.Int32(1),
				CopySource: str(b + "/src"), CopySourceRange: str("bytes=0-1023")})
			if err != nil {
				return err
			}
			_, err = c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &b, Key: str("pc"), UploadId: mp.UploadId,
				MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.CopyPartResult.ETag}}}})
			if err != nil {
				return err
			}
			h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &b, Key: str("pc")})
			if err == nil && aws.ToInt64(h.ContentLength) != 1024 {
				err = fmt.Errorf("size %d, want 1024", aws.ToInt64(h.ContentLength))
			}
			return err
		}},
		{"ListMultipartUploads", func() error {
			if _, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &b, Key: str("open")}); err != nil {
				return err
			}
			out, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: &b})
			if err == nil && len(out.Uploads) == 0 {
				err = errors.New("open upload not listed")
			}
			return err
		}},
		{"ListObjectVersions", func() error {
			out, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: &b})
			if err == nil && len(out.Versions) == 0 {
				err = errors.New("no versions listed")
			}
			return err
		}},
		{"Put/GetBucketVersioning", func() error {
			vb := b + "-ver"
			if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &vb}); err != nil {
				return err
			}
			if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: &vb,
				VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
				return err
			}
			out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &vb})
			if err == nil && out.Status != types.BucketVersioningStatusEnabled {
				err = fmt.Errorf("status %q", out.Status)
			}
			return err
		}},
		{"Put/GetBucketCors", func() error {
			if _, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{Bucket: &b, CORSConfiguration: &types.CORSConfiguration{
				CORSRules: []types.CORSRule{{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET", "PUT"}}}}}); err != nil {
				return err
			}
			_, err := c.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: &b})
			return err
		}},
		{"Put/GetBucketLifecycleConfiguration", func() error {
			rule := types.LifecycleRule{ID: str("tmp"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: str("tmp/")},
				Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}}
			if _, err := c.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{Bucket: &b,
				LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{rule}}}); err != nil {
				return err
			}
			_, err := c.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: &b})
			return err
		}},
		{"Put/GetBucketEncryption", func() error {
			if _, err := c.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{Bucket: &b, ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
				Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}}}}); err != nil {
				return err
			}
			_, err := c.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: &b})
			return err
		}},
		{"Put/GetBucketTagging", func() error {
			if _, err := c.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &b, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: str("env"), Value: str("dev")}}}}); err != nil {
				return err
			}
			_, err := c.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &b})
			return err
		}},
		{"GetObjectAttributes", func() error {
			_, err := c.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: &b, Key: str("presign.txt"),
				ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesObjectSize}})
			return err
		}},
	}
}

func expectBody(resp *http.Response, err error) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "hi" {
		return fmt.Errorf("%s %.80s", resp.Status, body)
	}
	return nil
}
