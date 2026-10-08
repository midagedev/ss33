# ss33 Agent Instructions

- Scope is the S3 subset in `docs/compatibility.md`. Adding an operation means adding it there and a test that
  drives it through `aws-sdk-go-v2` — the SDK is the compatibility oracle, not the AWS docs.
- Storage is a directory tree: object files named by key hash plus an append-only metadata journal per bucket
  (see the `internal/store` package comment). No database.
- Errors are S3 XML errors with the real S3 codes (`NoSuchKey`, `NoSuchBucket`, `SignatureDoesNotMatch`, …) —
  SDKs branch on the code.
- Keep the binary dependency-light: stdlib for the server; `aws-sdk-go-v2` only in tests.
- Public repo: no employer-specific names or hosts in code, docs, fixtures, or commit messages.
