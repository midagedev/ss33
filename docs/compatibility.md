# S3 compatibility

ss33 implements the subset below, verified through `aws-sdk-go-v2` in `internal/server/sdk_test.go` and
`internal/server/compat_test.go`. Anything else answers `501 NotImplemented` with an S3 XML error. That
includes an unknown subresource on a known path: `DELETE /b/k?legal-hold` is refused, it never deletes the
object.

| Area | Supported |
| --- | --- |
| Addressing | Path-style only (`/<bucket>/<key>`) |
| Auth | SigV4 Authorization header (unsigned `x-amz-*` headers are rejected, as S3 does); SigV4 presigned query (expiry enforced); SigV2 presigned query (`AWSAccessKeyId`/`Expires`/`Signature`, boto3's default for `generate_presigned_url`, tested with raw requests because the Go SDK cannot sign SigV2); POST policy for browser form uploads (SigV4 or SigV2); anonymous GET/HEAD on buckets whose policy grants `s3:GetObject` to `*` and on objects stored with a `public-read` ACL |
| Service | ListBuckets |
| Buckets | CreateBucket, HeadBucket, DeleteBucket (must be empty), GetBucketLocation, Get/Put/DeleteBucketPolicy, ListObjects (v1), ListObjectsV2 (prefix, delimiter, max-keys, continuation-token, start-after, encoding-type=url), ListObjectVersions (each object is its only version, ID `null`), ListMultipartUploads (prefix; one page), DeleteObjects, GetBucketAcl (owner FULL_CONTROL), PutBucketAcl (`private` only) |
| Stored bucket configuration | Get/Put/Delete for CORS, lifecycle, encryption and bucket tagging, Get/PutBucketVersioning: the XML is stored and returned as sent, but not acted on (see below) |
| Objects | PutObject (`If-None-Match: *`, `If-Match`), GetObject (Range, If-None-Match, `response-*` overrides), HeadObject, GetObjectAttributes (ETag, size, storage class, parts), DeleteObject (also with `versionId=null`), CopyObject (COPY/REPLACE metadata and tagging directives), PostObject (`content-length-range`, `eq`, `starts-with`, `${filename}`, `success_action_status`) |
| Tagging | `x-amz-tagging` on PUT/CreateMultipartUpload, Get/Put/DeleteObjectTagging, `x-amz-tagging-count` on GET |
| ACLs | Canned `x-amz-acl` on PUT/CreateMultipartUpload/CopyObject and PutObjectAcl (canned or grant body); only "everyone may read" is kept. GetObjectAcl reports it |
| Multipart | CreateMultipartUpload, UploadPart, UploadPartCopy (with `x-amz-copy-source-range`), CompleteMultipartUpload (S3-style `-N` ETag), AbortMultipartUpload, ListParts |
| Bodies | Plain and `aws-chunked` (signed or unsigned, with trailers); chunk signatures and checksums are not verified |
| Checksums | PutObject answers `x-amz-checksum-{crc32,crc32c,crc64nvme,sha1,sha256}` computed over the stored bytes when the request names that algorithm (header, trailer, or `x-amz-sdk-checksum-algorithm`); client-sent values are not compared |
| Metadata | Content-Type, Content-Disposition, Cache-Control, Content-Language, Content-Encoding, Expires, `x-amz-meta-*` (returned lowercase, as S3 does) |
| CORS | Every origin is allowed; preflights echo the requested headers. A stored CORS configuration does not narrow this |
| Health | `/minio/health/live`, `/minio/health/ready`, `/healthz` |

Stored-but-inert configuration: these calls succeed so bootstrap scripts run to the end, but nothing
enforces them. Versioning can be set to `Enabled`, yet only the latest version of each object is kept.
Lifecycle rules never expire anything. Default encryption is not applied. CORS stays open to every origin.

Not supported: keeping old versions, object lock, retention and legal hold, bucket
notifications, SelectObjectContent, SSE-C and SSE-KMS, virtual-hosted addressing, SigV2 header auth, and
pagination of ListMultipartUploads and ListParts.

## MinIO drop-in

- `ss33 server <dir> [--address :9000] [--console-address ...]` and `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`
  (or the legacy `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`) are accepted, so a compose service or a
  Testcontainers MinIO module can switch images without other changes.
- The image ships the binary as `mc` too, covering:
  - `alias set` and the older `config host add`; `MC_HOST_<alias>=http://KEY:SECRET@host:port` also defines an alias
  - `mb [--ignore-existing]`, `rb [--force]`
  - `ls [--recursive]`; a bare `ALIAS` lists buckets
  - `cp [--recursive]`: local→remote, remote→remote and remote→local
  - `anonymous set download|public|none`, `ready`
