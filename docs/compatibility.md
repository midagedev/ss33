# S3 compatibility

ss33 implements the subset below, verified through `aws-sdk-go-v2` in `internal/server/sdk_test.go`.
Anything else answers `501 NotImplemented` with an S3 XML error.

| Area | Supported |
| --- | --- |
| Addressing | Path-style only (`/<bucket>/<key>`) |
| Auth | SigV4 Authorization header; SigV4 presigned query (expiry enforced); anonymous GET/HEAD on buckets whose policy grants `s3:GetObject` to `*` |
| Service | ListBuckets |
| Buckets | CreateBucket, HeadBucket, DeleteBucket (must be empty), GetBucketLocation, Get/Put/DeleteBucketPolicy, GetBucketVersioning (always unversioned), ListObjects (v1), ListObjectsV2 (prefix, delimiter, max-keys, continuation-token, start-after, encoding-type=url), DeleteObjects |
| Objects | PutObject, GetObject (Range, If-None-Match, `response-*` overrides), HeadObject, DeleteObject, CopyObject (COPY/REPLACE metadata directive) |
| Multipart | CreateMultipartUpload, UploadPart, CompleteMultipartUpload (S3-style `-N` ETag), AbortMultipartUpload, ListParts |
| Bodies | Plain and `aws-chunked` (signed or unsigned, with trailers); chunk signatures and checksums are not verified |
| Checksums | PutObject answers `x-amz-checksum-{crc32,crc32c,sha1,sha256}` computed over the stored bytes when the request names that algorithm (header, trailer, or `x-amz-sdk-checksum-algorithm`); client-sent values are not compared, CRC64NVME is not computed |
| Metadata | Content-Type, Content-Disposition, Cache-Control, Content-Language, Content-Encoding, Expires, `x-amz-meta-*` |
| CORS | Every origin is allowed; preflights echo the requested headers |
| Health | `/minio/health/live`, `/minio/health/ready`, `/healthz` |

Not supported: versioning, object lock, tagging, ACLs, lifecycle, encryption headers, virtual-hosted addressing,
UploadPartCopy, SigV2.

## MinIO drop-in

- `ss33 server <dir> [--address :9000] [--console-address ...]` and `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`
  are accepted, so a compose service can switch images without other changes.
- The image ships the binary as `mc` too, covering `alias set`, `mb [--ignore-existing]`, `rb [--force]`,
  `ls [--recursive]` (bare `ALIAS` lists buckets), `cp [--recursive]` (local→remote, remote→remote, remote→local), `anonymous set
  download|public|none`, `ready`.
