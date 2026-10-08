# ss33

A small S3-compatible object store for local development and CI.

ss33 implements the practical subset of the S3 API that applications use day to day — path-style
buckets and objects, SigV4 (header and presigned query) authentication, multipart upload, Range GET,
ListObjectsV2, browser CORS, and anonymous read buckets — backed by a plain directory on disk.
The same binary carries a tiny `mc`-style CLI for bootstrapping buckets and seeding fixtures.

It is **not** a production object store. Use it as the fast, deterministic lane for local stacks and
CI; keep real S3 for behavior outside the documented subset.

## Quick start

```sh
go run ./cmd/ss33 serve --addr :9000 --data ./data --access-key ss33 --secret-key ss33secret
```

## Supported API

See [docs/compatibility.md](docs/compatibility.md).
