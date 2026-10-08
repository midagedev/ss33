package server_test

// Bucket versioning as the SDK drives it: version IDs, delete markers, reads and copies of old versions,
// listing, permanent deletes, suspension, and emptying a versioned bucket.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// captureHeaders records the raw response headers of an SDK call, error responses included.
func captureHeaders(dst *http.Header) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("captureHeaders",
			func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
				out, md, err := next.HandleDeserialize(ctx, in)
				if resp, ok := out.RawResponse.(*smithyhttp.Response); ok {
					*dst = resp.Header
				}
				return out, md, err
			}), middleware.After)
	}
}

func setBucketVersioning(t *testing.T, c *s3.Client, bucket string, status types.BucketVersioningStatus) {
	t.Helper()
	if _, err := c.PutBucketVersioning(context.Background(), &s3.PutBucketVersioningInput{Bucket: aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status}}); err != nil {
		t.Fatalf("PutBucketVersioning %s: %v", status, err)
	}
}

func getBody(t *testing.T, c *s3.Client, bucket, key, versionID string) string {
	t.Helper()
	in := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if versionID != "" {
		in.VersionId = aws.String(versionID)
	}
	out, err := c.GetObject(context.Background(), in)
	if err != nil {
		t.Fatalf("GetObject %s@%s: %v", key, versionID, err)
	}
	defer out.Body.Close()
	data, _ := io.ReadAll(out.Body)
	return string(data)
}

func TestVersioningWithSDK(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	setBucketVersioning(t, c, "bkt", types.BucketVersioningStatusEnabled)
	if got, _ := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("bkt")}); got.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("GetBucketVersioning: %q", got.Status)
	}

	putV := func(body string) string {
		t.Helper()
		out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader(body)})
		if err != nil {
			t.Fatal(err)
		}
		return aws.ToString(out.VersionId)
	}
	v1, v2 := putV("one"), putV("two")
	if v1 == "" || v1 == v2 {
		t.Fatalf("PutObject version IDs %q %q", v1, v2)
	}
	if got := getBody(t, c, "bkt", "k", v1); got != "one" {
		t.Fatalf("GetObject v1: %q", got)
	}
	if h, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); err != nil || aws.ToString(h.VersionId) != v2 {
		t.Fatalf("HeadObject current: %v %v", h, err)
	}

	// Tags belong to a version.
	if _, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v1),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("old"), Value: aws.String("yes")}}}}); err != nil {
		t.Fatal(err)
	}
	if tags, _ := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); len(tags.TagSet) != 0 {
		t.Fatalf("current version picked up v1's tags: %+v", tags.TagSet)
	}

	// A plain delete leaves a marker on top.
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || !aws.ToBool(del.DeleteMarker) || aws.ToString(del.VersionId) == "" {
		t.Fatalf("DeleteObject: %+v %v", del, err)
	}
	marker := aws.ToString(del.VersionId)
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}); errCode(err) != "NoSuchKey" {
		t.Fatalf("GetObject under a delete marker: %v", err)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(marker)}); errCode(err) != "MethodNotAllowed" {
		t.Fatalf("GetObject of the delete marker's version: %v", err)
	}
	if got := getBody(t, c, "bkt", "k", v2); got != "two" {
		t.Fatalf("old version under a marker: %q", got)
	}
	if l, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("bkt")}); len(l.Contents) != 0 {
		t.Fatalf("ListObjectsV2 shows a deleted key: %+v", l.Contents)
	}
	lv, err := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("bkt")})
	if err != nil || len(lv.Versions) != 2 || len(lv.DeleteMarkers) != 1 || !aws.ToBool(lv.DeleteMarkers[0].IsLatest) ||
		aws.ToString(lv.Versions[0].VersionId) != v2 || aws.ToBool(lv.Versions[0].IsLatest) {
		t.Fatalf("ListObjectVersions: %v %+v", err, lv)
	}

	// Copy an old version.
	cp, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("bkt"), Key: aws.String("restored"), CopySource: aws.String("bkt/k?versionId=" + v1)})
	if err != nil || aws.ToString(cp.CopySourceVersionId) != v1 {
		t.Fatalf("CopyObject from v1: %+v %v", cp, err)
	}
	if got := getBody(t, c, "bkt", "restored", ""); got != "one" {
		t.Fatalf("copied v1: %q", got)
	}

	// Removing the marker undeletes; removing the current version promotes the previous one.
	if out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{
		Objects: []types.ObjectIdentifier{{Key: aws.String("k"), VersionId: aws.String(marker)}}}}); err != nil || len(out.Deleted) != 1 || !aws.ToBool(out.Deleted[0].DeleteMarker) {
		t.Fatalf("DeleteObjects of the marker: %+v %v", out, err)
	}
	if got := getBody(t, c, "bkt", "k", ""); got != "two" {
		t.Fatalf("after removing the marker: %q", got)
	}
	if _, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2)}); err != nil {
		t.Fatal(err)
	}
	if got := getBody(t, c, "bkt", "k", ""); got != "one" {
		t.Fatalf("after removing v2: %q", got)
	}
	if _, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), VersionId: aws.String(v2)}); errCode(err) != "NoSuchVersion" {
		t.Fatalf("GetObject of a removed version: %v", err)
	}

	// Suspended: writes get the null version.
	setBucketVersioning(t, c, "bkt", types.BucketVersioningStatusSuspended)
	if v := putV("null-one"); v != "null" {
		t.Fatalf("PutObject while suspended: version %q", v)
	}
	if got := getBody(t, c, "bkt", "k", "null"); got != "null-one" {
		t.Fatalf("null version: %q", got)
	}
	if _, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("bkt"),
		VersioningConfiguration: &types.VersioningConfiguration{}}); errCode(err) != "MalformedXML" {
		t.Fatalf("PutBucketVersioning without a status: %v", err)
	}

	// Emptying a versioned bucket, as test cleanup does: list every version and marker, delete them all.
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("bkt")}); errCode(err) != "BucketNotEmpty" {
		t.Fatalf("DeleteBucket with versions left: %v", err)
	}
	var ids []types.ObjectIdentifier
	in := &s3.ListObjectVersionsInput{Bucket: aws.String("bkt"), MaxKeys: aws.Int32(1)}
	for {
		page, err := c.ListObjectVersions(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	if len(ids) != 3 { // k@null, k@v1, restored@<id>
		t.Fatalf("paged versions: %+v", ids)
	}
	if _, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String("bkt"), Delete: &types.Delete{Objects: ids}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("bkt")}); err != nil {
		t.Fatalf("DeleteBucket after removing every version: %v", err)
	}
}

// A delete marker is reported in the headers of a GET or HEAD that hits it.
func TestDeleteMarkerHeaders(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	setBucketVersioning(t, c, "bkt", types.BucketVersioningStatusEnabled)
	put(t, c, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("v")})
	del, _ := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")})

	var header http.Header
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")}, func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, captureHeaders(&header))
	})
	if err == nil || header.Get("X-Amz-Delete-Marker") != "true" || header.Get("X-Amz-Version-Id") != aws.ToString(del.VersionId) {
		t.Fatalf("HEAD on a deleted key: err=%v headers=%v", err, header)
	}
}

// Buckets that never had versioning keep answering without version IDs.
func TestUnversionedBucketHasNoVersionIDs(t *testing.T) {
	ctx := context.Background()
	_, c := newServer(t)
	mustBucket(t, c, "bkt")
	out, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k"), Body: strings.NewReader("v")})
	if err != nil || out.VersionId != nil {
		t.Fatalf("PutObject: version %v, err %v", aws.ToString(out.VersionId), err)
	}
	del, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("bkt"), Key: aws.String("k")})
	if err != nil || del.DeleteMarker != nil {
		t.Fatalf("DeleteObject: %+v %v", del, err)
	}
	if lv, _ := c.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("bkt")}); len(lv.Versions)+len(lv.DeleteMarkers) != 0 {
		t.Fatalf("versions left after delete: %+v", lv)
	}
}
