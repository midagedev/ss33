# S3 compatibility

ss33 implements the subset below, verified through `aws-sdk-go-v2` in `internal/server/*_test.go`. CI also
runs the scripts in [`clients/`](../clients) against the image built from each commit: boto3, the
JavaScript and Java SDKs, the AWS CLI, two releases of the official `mc`, MinIO-style webhook
notifications, the Docker registry's S3 driver, the Terraform AWS provider, Testcontainers and the README's
compose example. Anything else answers `501 NotImplemented` with an S3 XML error. That includes an unknown
subresource on a known path: `DELETE /b/k?legal-hold` is refused, it never deletes the object.

| Area | Supported |
| --- | --- |
| Addressing | Path-style (`/<bucket>/<key>`) and virtual-hosted (`<bucket>.localhost`, and `<bucket>.<domain>` for each domain in `MINIO_DOMAIN` or `SS33_DOMAIN`, comma-separated) |
| Auth | SigV4 Authorization header (unsigned `x-amz-*` headers are rejected, and the request time must be within 15 minutes of the server's, as in S3); SigV4 presigned query (expiry enforced); SigV2 presigned query (`AWSAccessKeyId`/`Expires`/`Signature`, boto3's default for `generate_presigned_url` in us-east-1, tested with raw requests because the Go SDK cannot sign SigV2); POST policy for browser form uploads (SigV4 or SigV2); anonymous requests, see below |
| Anonymous access | GetObject/HeadObject (GetObjectVersion with `versionId`), ListObjects/HeadBucket, GetBucketLocation, PutObject and DeleteObject (DeleteObjectVersion) when the bucket policy allows that action on that resource to every principal (`"*"`, `{"AWS":"*"}` or `{"AWS":["*"]}`), with `*`/`?` wildcards. A matching Deny wins. Statements with a `Condition` or a `Not*` element never grant, and a Deny of that kind is assumed to apply. GET/HEAD also on objects stored with a `public-read` ACL |
| Service | ListBuckets (`prefix`, `max-buckets`, `continuation-token`) |
| Buckets | CreateBucket (tags in `CreateBucketConfiguration` are kept), HeadBucket, DeleteBucket (must hold no versions or delete markers), GetBucketLocation, Get/Put/DeleteBucketPolicy, ListObjects (v1), ListObjectsV2 (prefix, delimiter, max-keys, continuation-token, start-after, encoding-type=url, and MinIO's `metadata=true`, which adds `UserMetadata` and `UserTags` to each object as the official `mc diff` and `mirror -a` expect), ListObjectVersions (prefix, delimiter, key-marker, version-id-marker, max-keys), ListMultipartUploads (prefix; one page), DeleteObjects (with `VersionId`), GetBucketAcl (owner FULL_CONTROL), PutBucketAcl (`private` only), GetObjectLockConfiguration (always `ObjectLockConfigurationNotFoundError`, as for any S3 bucket without object lock) |
| Versioning | Put/GetBucketVersioning (`Enabled`, `Suspended`). With versioning every write gets a version ID and replaced versions are kept; a delete without `versionId` adds a delete marker; GET, HEAD, CopyObject (`?versionId=` in `x-amz-copy-source`), UploadPartCopy, tagging, ACLs and GetObjectAttributes take `versionId`; DeleteObject with `versionId` removes that version for good and the newest remaining one becomes current. Suspended buckets write and delete the `null` version. Responses carry `x-amz-version-id` and `x-amz-delete-marker` |
| Notifications | Get/PutBucketNotificationConfiguration with queue destinations `arn:minio:sqs::<id>:webhook` for webhook targets set up as in MinIO: `MINIO_NOTIFY_WEBHOOK_ENABLE_<ID>=on`, `MINIO_NOTIFY_WEBHOOK_ENDPOINT_<ID>=<url>`, optional `MINIO_NOTIFY_WEBHOOK_AUTH_TOKEN_<ID>` (the variables without `_<ID>` define target `_`). Events: `s3:ObjectCreated:{Put,Post,Copy,CompleteMultipartUpload}`, `s3:ObjectRemoved:{Delete,DeleteMarkerCreated}`, `s3:ObjectAccessed:{Get,Head}`, with `*` patterns and prefix/suffix filters. Each event is POSTed as S3 event JSON in MinIO's envelope (`EventName`, `Key`, `Records`), in order per target, retried 5 times. Other destinations (SNS, Lambda, AWS SQS, unknown targets) are refused with `InvalidArgument`, as S3 refuses ones it cannot validate |
| Stored bucket configuration | Get/Put/Delete for CORS, lifecycle (with `x-amz-transition-default-minimum-object-size`), encryption, bucket tagging, website, replication, public access block and ownership controls; Get/Put for logging, accelerate and request payment (which read as their S3 defaults until set). The XML is stored and returned as sent; what ss33 does with each is below |
| Objects | PutObject (`If-None-Match: *`, `If-Match`), GetObject (Range, `partNumber`, If-None-Match, `response-*` overrides), HeadObject (`partNumber`), GetObjectAttributes (ETag, size, storage class, parts), DeleteObject, CopyObject (COPY/REPLACE metadata and tagging directives), PostObject (`content-length-range`, `eq`, `starts-with`, `${filename}`, `success_action_status`) |
| Tagging | `x-amz-tagging` on PUT/CreateMultipartUpload, Get/Put/DeleteObjectTagging, `x-amz-tagging-count` on GET |
| ACLs | Canned `x-amz-acl` on PUT/CreateMultipartUpload/CopyObject and PutObjectAcl (canned or grant body); only "everyone may read" is kept. GetObjectAcl reports it |
| Multipart | CreateMultipartUpload, UploadPart, UploadPartCopy (with `x-amz-copy-source-range`), CompleteMultipartUpload (S3-style `-N` ETag; `If-None-Match: *` and `If-Match`, checked before the parts are used so a failed condition can be retried), AbortMultipartUpload, ListParts (`max-parts`, `part-number-marker`, and every field S3 returns) |
| Bodies | Plain and `aws-chunked` (signed or unsigned, with trailers); chunk signatures are not verified |
| Integrity | `Content-MD5` and `x-amz-checksum-{crc32,crc32c,crc64nvme,sha1,sha256}` (as a header or an `aws-chunked` trailer) are compared with the body on PutObject and UploadPart; a mismatch is `BadDigest` and nothing is stored. The requested checksum is answered, computed over the stored bytes |
| Encryption | `x-amz-server-side-encryption` (`AES256`, `aws:kms`, `aws:kms:dsse`) with its KMS key ID and bucket-key headers is recorded and returned on PUT, GET, HEAD, CopyObject and CreateMultipartUpload; a bucket's default encryption applies to objects written without their own. SSE-C (`x-amz-server-side-encryption-customer-*`) records the key's MD5: GET, HEAD and copies of the object need that key (`InvalidRequest` without it, `AccessDenied` with another). Nothing is actually encrypted |
| Storage classes | `x-amz-storage-class` is validated, kept and reported on GET, HEAD, listings and GetObjectAttributes. Every class reads immediately; there is no archive tier or RestoreObject |
| Metadata | Content-Type, Content-Disposition, Cache-Control, Content-Language, Content-Encoding, Expires, `x-amz-meta-*` (returned lowercase, as S3 does) |
| CORS | Every origin is allowed; preflights echo the requested headers. A stored CORS configuration does not narrow this |
| Health | `/minio/health/live`, `/minio/health/ready`, `/minio/health/cluster`, `/minio/health/cluster/read`, `/healthz` |

Stored-but-inert configuration: these calls succeed and read back as written, so bootstrap scripts and
Terraform run to the end, but nothing enforces them. Lifecycle rules never expire anything. CORS stays open
to every origin. Public access blocks, ownership controls, website, logging, accelerate, request payment and
replication settings change nothing. Encryption is reported, never applied (see above).

Not supported: object lock, retention and legal hold, SelectObjectContent, RestoreObject, website hosting,
SigV2 header auth, multiple users or STS, notification targets other than webhooks, and pagination of
ListMultipartUploads.

## MinIO drop-in

- `ss33 server <dir> [--address :9000] [--console-address ...]` and `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`
  (or the legacy `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`) are accepted, so a compose service or a
  Testcontainers MinIO module can switch images without other changes. Without them the credentials are
  MinIO's defaults, `minioadmin` / `minioadmin`. `MINIO_DOMAIN` and `MINIO_NOTIFY_WEBHOOK_*` work as in MinIO.
- The image ships the binary as `mc` too, covering:
  - `alias set` and the older `config host add` (`--api`/`--path` values are ignored); `MC_HOST_<alias>=http://KEY:SECRET@host:port` also defines an alias
  - `mb [--ignore-existing]`, `rb [--force]` (a versioned bucket's versions included)
  - `ls [--recursive]`, `ls --versions`; a bare `ALIAS` lists buckets
  - `cp [--recursive]`: local→remote, remote→remote and remote→local
  - `rm ALIAS/BUCKET/KEY`, `rm --version-id ID ALIAS/BUCKET/KEY`, `rm --recursive --force ALIAS/BUCKET[/PREFIX]`
  - `version enable|suspend`
  - `event add ALIAS/BUCKET ARN [--event put,delete,get] [--prefix P] [--suffix S]`, `event ls`, `event rm ARN` or `--force`
  - `anonymous set download|public|none` (writing the same policy statements as the official mc), `ready`
