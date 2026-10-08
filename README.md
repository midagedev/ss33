# ss33

[![CI](https://github.com/midagedev/ss33/actions/workflows/ci.yml/badge.svg)](https://github.com/midagedev/ss33/actions/workflows/ci.yml)
[![GHCR](https://img.shields.io/badge/ghcr.io-midagedev%2Fss33-2496ED?logo=docker&logoColor=white)](https://github.com/midagedev/ss33/pkgs/container/ss33)
[![Go](https://img.shields.io/github/go-mod/go-version/midagedev/ss33)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

**A drop-in replacement for the `minio/minio` and `minio/mc` images in local dev and CI.**
It is a small S3-compatible server and an `mc`-compatible CLI in one 10 MB image.

## Why

As of October 2026, `docker pull minio/minio` and `docker pull minio/mc` fail: both images are gone from Docker
Hub, and the upstream repositories are archived. Compose stacks and CI jobs that used MinIO as a stand-in for
S3 now break at the pull step. ss33 covers the part of MinIO those setups need, and nothing more.

## Drop-in replacement

Change the two `image:` lines. Leave `command`, `environment` and `healthcheck` as they are.

```diff
 services:
   minio:
-    image: minio/minio
+    image: ghcr.io/midagedev/ss33:0.1
     command: server /data --console-address ":9001"
     environment:
       MINIO_ROOT_USER: minioadmin
       MINIO_ROOT_PASSWORD: minioadmin
     healthcheck:
       test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]

   createbuckets:
-    image: minio/mc
+    image: ghcr.io/midagedev/ss33:0.1
     depends_on:
       minio:
         condition: service_healthy
     entrypoint: >
       /bin/sh -c "
       /usr/bin/mc alias set local http://minio:9000 minioadmin minioadmin;
       /usr/bin/mc mb --ignore-existing local/uploads;
       /usr/bin/mc anonymous set download local/uploads;
       "
```

These carry over unchanged:

- **Command:** `server <dir> [--address :9000] [--console-address ...]`.
- **Credentials:** `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`. The legacy `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` also work.
- **Health:** `/minio/health/live`, `/minio/health/ready` and `/healthz`. `curl` is in the image.
- **mc:** `mc` is at `/usr/bin/mc` and `/usr/local/bin/mc`, and `/bin/sh` is available for `entrypoint: /bin/sh -c "..."` scripts.

There are three things to watch for:

- **Bare `minio/mc` commands.** If an mc service runs commands like `command: ["ls", "local"]` and relies on the
  image's default `mc` entrypoint, add `entrypoint: mc`. Aliases can come from `mc alias set`, `mc config host add`
  or `MC_HOST_<alias>`.
- **Existing MinIO volumes.** ss33 cannot read MinIO's on-disk format. Start from an empty volume and re-seed.
- **The console.** `--console-address` is accepted but no console is served.

## Quick start

```sh
docker run -d -p 9000:9000 ghcr.io/midagedev/ss33        # credentials: ss33 / ss33secret
```

```sh
export AWS_ACCESS_KEY_ID=ss33 AWS_SECRET_ACCESS_KEY=ss33secret AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://localhost:9000 s3 mb s3://demo
aws --endpoint-url http://localhost:9000 s3 cp ./README.md s3://demo/
aws --endpoint-url http://localhost:9000 s3 ls s3://demo/
```

Without Docker:

```sh
go install github.com/midagedev/ss33/cmd/ss33@latest
ss33 server ./data                 # S3 API on :9000
ss33 mc alias set local http://localhost:9000 ss33 ss33secret
```

Point SDKs at the endpoint with path-style addressing, for example `forcePathStyle: true` (JS),
`UsePathStyle: true` (Go) or `pathStyleAccessEnabled(true)` (Java). boto3 uses path-style on its own when
`endpoint_url` is set.

**Testcontainers.** The MinIO modules start the image with MinIO's command and credentials, which ss33
accepts. In Java, declare the substitution:

```java
new MinIOContainer(DockerImageName.parse("ghcr.io/midagedev/ss33:0.1").asCompatibleSubstituteFor("minio/minio"))
```

In Go, Python and Node, pass the image name where the module takes one.

### Fast or durable

By default ss33 does not fsync: a write is acknowledged once it is in the OS page cache. That is the right
trade for tests, and it is why small writes are fast. `--durable` (or `SS33_DURABLE=1`) fsyncs object data,
the metadata journal and directory entries before answering, for stacks that must survive a host crash.

## Numbers

### Throughput

Measured with one client ([`bench/`](bench), aws-sdk-go-v2) against every server, on the same GitHub
Actions runner (4 vCPU, Linux), each server in Docker with its data on the runner's disk. Higher is better
for ops/s and MiB/s; lower is better for ms. The [Benchmark workflow](.github/workflows/bench.yml) reruns it.

| Test | ss33 | ss33 --durable | MinIO | RustFS 1.0.1 | SeaweedFS 4.48 | versitygw 1.8.0 |
|---|---:|---:|---:|---:|---:|---:|
| PUT 4 KiB, 16 concurrent (ops/s) | **4,716** | 2,621 | 1,674 | 964 | 1,986 | 2,162 |
| GET 4 KiB, 16 concurrent (ops/s) | **7,237** | 7,197 | 3,606 | 4,726 | 3,544 | 2,786 |
| HEAD, 16 concurrent (ops/s) | 8,617 | **8,659** | 4,366 | 7,002 | 5,172 | 2,958 |
| PUT 256 MiB, best of 3 (MiB/s) | **283** | 199 | 223 | 181 | 224 | 245 |
| GET 256 MiB, best of 3 (MiB/s) | **2,228** | 1,809 | 1,643 | 1,443 | 1,388 | 2,200 |
| UploadPart 32 × 8 MiB, 32 concurrent (MiB/s) | **793** | 563 | 500 | 395 | 341 | 679 |
| ListParts, 32 parts (ms) | 2.0 | **1.1** | 3.0 | 2.8 | 3.6 | 2.1 |
| CompleteMultipartUpload, 256 MiB (ms) | 2.3 | **2.2** | 4.7 | 9.7 | 19.0 | 109.6 |
| PUT 0 B × 20,000, 32 concurrent (ops/s) | **6,099** | 3,033 | 1,618 | 957 | 4,312 | 2,343 |
| ListObjectsV2, all 20,000 keys (ms) | 544.5 | **303.5** | 747.8 | 3,172.5 | 379.1 | 842.5 |
| ListObjectsV2, one prefix page (ms) | 1.8 | **1.6** | 3.0 | 9.5 | 2.1 | 3.0 |

MinIO is built from source (the last published module version, 2026-02-12), since its images are gone. The
others are their published images with default settings. Only ss33 skips fsync by default; `ss33 --durable`
is the like-for-like column. Shared runners are noisy, and single runs vary by 20–30 %.

### Footprint

| | ss33 0.1 | minio/minio `RELEASE.2025-01-20T14-49-07Z` |
|---|---:|---:|
| Image, compressed | **9.6 MB** | 58.6 MB |
| Image, on disk | **32.5 MB** | 235 MB |
| Cold start → `/minio/health/live` 200 (median of 5) | **168 ms** | 429 ms |
| Idle memory | **2.1 MiB** | 85.5 MiB |

Measured on an Apple M4 Pro with Docker 29.5.2 (linux/arm64), 2026-10-08.
MinIO is a full distributed object store and does far more; these numbers only show what a test double
costs in a dev stack.

<details>
<summary>How these were measured</summary>

```sh
img=ghcr.io/midagedev/ss33:0.1     # or minio/minio:RELEASE.2025-01-20T14-49-07Z
docker save "$img" | gzip | wc -c                     # compressed size
docker images "$img"                                   # size on disk
now() { python3 -c 'import time; print(int(time.time() * 1000))'; }
for i in 1 2 3 4 5; do                                 # cold start
  docker rm -f bench >/dev/null 2>&1
  t0=$(now)
  docker run -d --name bench -p 9000:9000 -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
    "$img" server /data --console-address :9001 >/dev/null
  until curl -sf -o /dev/null http://127.0.0.1:9000/minio/health/live; do sleep 0.02; done
  echo "$(( $(now) - t0 )) ms"
done
sleep 10; docker stats --no-stream --format '{{.MemUsage}}' bench   # idle memory
```

</details>

## What's supported

| Area | Supported | Not supported |
|---|---|---|
| Objects | Put (incl. `If-None-Match: *`, `If-Match`), Get (Range, conditional, `response-*` overrides), Head, GetObjectAttributes, Delete, DeleteObjects, CopyObject, `x-amz-meta-*`, tagging, canned ACLs (`public-read` allows anonymous GET) | Object lock, SelectObjectContent |
| Buckets | Create, Delete, Head, List, Location, ListObjects v1/v2, ListObjectVersions, bucket policy (public-read), ACL (read) | Notifications, replication, website |
| Bucket configuration | Versioning, CORS, lifecycle, encryption and tags are stored and returned, not enforced | Keeping old versions, expiring objects |
| Multipart | Create, UploadPart, UploadPartCopy, Complete, Abort, ListParts, ListMultipartUploads | |
| Auth | SigV4 header and presigned URLs (expiry enforced), SigV2 presigned URLs (boto3's default), browser POST policy uploads, `aws-chunked` streaming, anonymous GET on public buckets and objects | SigV2 headers, multiple users, STS |
| Checksums | `x-amz-checksum-{crc32,crc32c,crc64nvme,sha1,sha256}` returned on PutObject | |
| Addressing | Path-style | Virtual-hosted |
| Browser | CORS allows every origin and exposes `ETag` | |
| `mc` | `alias set`, `config host add`, `MC_HOST_<alias>`, `mb [--ignore-existing\|-p]`, `rb [--force]`, `ls [--recursive]`, `cp [--recursive]`, `rm [--recursive --force]`, `anonymous\|policy set download\|public\|none`, `version enable`, `ready` | Everything else, including `mc admin` and `mc mirror` |

Unsupported operations return `501 NotImplemented` as an S3 XML error. They never fail silently.
The full list is in [docs/compatibility.md](docs/compatibility.md).

## Verified clients

- **In CI on every change:** aws-sdk-go-v2 (`service/s3` v1.114.1), which the test suite in
  [`internal/server/sdk_test.go`](internal/server/sdk_test.go) drives. The suite also replays an `mc` bootstrap script.
- **Checked by hand:**
  - AWS SDK for Java 2.31.1: `S3Client`, `S3AsyncClient` with `multipartEnabled`, and `S3Presigner` for GET, PUT and UploadPart.
  - AWS SDK for JavaScript v3: `@aws-sdk/client-s3` 3.1143.0.
  - boto3 1.43: `upload_fileobj`/`download_fileobj`, managed `copy`, paginators, `generate_presigned_url`,
    `generate_presigned_post`, tagging, ACLs, conditional PUT, versions and uploads listings.
  - AWS CLI 2.37.4: `mb`, `cp` (including multipart), `ls`, `sync`, `presign`, `s3api` tagging and `rb --force`.

## Non-goals

ss33 is a test double, not a storage system. It has no production durability guarantees, no web console, no
version history or replication, no clustering, and only one set of credentials. For those, use real S3 or a full
S3-compatible server.

## License

[Apache-2.0](LICENSE)

MinIO is a trademark of MinIO, Inc. ss33 is an independent project, not affiliated with or endorsed by MinIO, Inc.,
and contains no MinIO code.
