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
  image's default `mc` entrypoint, add `entrypoint: mc`. `MC_HOST_<alias>` environment variables are not read,
  so run `mc alias set` first.
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
`UsePathStyle: true` (Go) or `pathStyleAccessEnabled(true)` (Java).

## Numbers

| | ss33 0.1 | minio/minio `RELEASE.2025-01-20T14-49-07Z` |
|---|---:|---:|
| Image, compressed | **9.6 MB** | 58.6 MB |
| Image, on disk | **32.5 MB** | 235 MB |
| Cold start → `/minio/health/live` 200 (median of 5) | **168 ms** | 429 ms |
| Idle memory | **2.1 MiB** | 85.5 MiB |

Measured on an Apple M4 Pro with Docker 29.5.2 (linux/arm64), 2026-10-08.

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
| Objects | Put, Get (Range, conditional, `response-*` overrides), Head, Delete, DeleteObjects, CopyObject, `x-amz-meta-*` | Tagging, ACLs, object lock, GetObjectAttributes |
| Buckets | Create, Delete, Head, List, Location, ListObjects v1/v2, bucket policy (public-read) | Versioning, lifecycle, CORS config API, notifications, replication, website |
| Multipart | Create, UploadPart, Complete, Abort, ListParts | UploadPartCopy, ListMultipartUploads |
| Auth | SigV4 header and presigned URLs (expiry enforced), `aws-chunked` streaming, anonymous GET on public buckets | SigV2, multiple users, STS |
| Checksums | `x-amz-checksum-{crc32,crc32c,sha1,sha256}` returned on PutObject | CRC64NVME |
| Addressing | Path-style | Virtual-hosted |
| Browser | CORS allows every origin and exposes `ETag` | |
| `mc` | `alias set`, `mb [--ignore-existing\|-p]`, `rb [--force]`, `ls [--recursive]`, `cp [--recursive]`, `anonymous\|policy set download\|public\|none`, `ready` | Everything else, including `mc admin` and `mc mirror` |

Unsupported operations return `501 NotImplemented` as an S3 XML error. They never fail silently.
The full list is in [docs/compatibility.md](docs/compatibility.md).

## Verified clients

- **In CI on every change:** aws-sdk-go-v2 (`service/s3` v1.114.1), which the test suite in
  [`internal/server/sdk_test.go`](internal/server/sdk_test.go) drives. The suite also replays an `mc` bootstrap script.
- **Checked by hand:**
  - AWS SDK for Java 2.31.1: `S3Client`, `S3AsyncClient` with `multipartEnabled`, and `S3Presigner` for GET, PUT and UploadPart.
  - AWS SDK for JavaScript v3: `@aws-sdk/client-s3` 3.1143.0.
  - AWS CLI 2.37.4: `mb`, `cp` (including multipart), `ls`, `sync`, `presign` and `rb --force`.

## Non-goals

ss33 is a test double, not a storage system. It has no production durability guarantees, no web console, no
versioning or replication, no clustering, and only one set of credentials. For those, use real S3 or a full
S3-compatible server.

## License

[Apache-2.0](LICENSE)
