#!/usr/bin/env bash
# AWS CLI v2 against an S3 endpoint: the high-level `s3` commands and a few `s3api` calls.
# usage: bash awscli.sh http://127.0.0.1:9000   (credentials minioadmin / minioadmin)
set -uo pipefail
ep=$1
export AWS_ACCESS_KEY_ID=minioadmin AWS_SECRET_ACCESS_KEY=minioadmin AWS_DEFAULT_REGION=us-east-1
aws() { command aws --endpoint-url "$ep" "$@"; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fails=0
check() { if (set -e; eval "$2") >"$work/out" 2>&1; then echo "PASS $1"; else fails=$((fails + 1)); echo "FAIL $1"; sed 's/^/     /' "$work/out"; fi; }

head -c 20000000 /dev/urandom >"$work/big.bin"
mkdir -p "$work/tree/a/b" && echo one >"$work/tree/a/1.txt" && echo two >"$work/tree/a/b/2.txt"

check "s3 mb" 'aws s3 mb s3://clients-awscli'
check "s3 cp upload (multipart, default CRC64NVME checksum)" 'aws s3 cp "$work/big.bin" s3://clients-awscli/big.bin'
check "s3 cp download round-trips" 'aws s3 cp s3://clients-awscli/big.bin "$work/back.bin" && cmp "$work/big.bin" "$work/back.bin"'
check "s3 sync + ls --recursive" 'aws s3 sync "$work/tree" s3://clients-awscli/tree && [ "$(aws s3 ls --recursive s3://clients-awscli/tree/ | wc -l)" -eq 2 ]'
check "s3 sync is a no-op the second time" 'out=$(aws s3 sync "$work/tree" s3://clients-awscli/tree --dryrun) && [ -z "$out" ]'
check "s3 presign" '[ "$(curl -fsS "$(aws s3 presign s3://clients-awscli/tree/a/1.txt --expires-in 60)")" = one ]'
check "s3api object tagging" 'aws s3api put-object-tagging --bucket clients-awscli --key tree/a/1.txt --tagging "TagSet=[{Key=k,Value=v}]" &&
  [ "$(aws s3api get-object-tagging --bucket clients-awscli --key tree/a/1.txt --query "TagSet[0].Value" --output text)" = v ]'
check "s3api head-object" '[ "$(aws s3api head-object --bucket clients-awscli --key big.bin --query ContentLength)" = 20000000 ]'
check "s3 rb --force" 'aws s3 rb --force s3://clients-awscli'
echo "$(command aws --version | cut -d' ' -f1): $([ $fails = 0 ] && echo ALL PASS || echo "$fails FAILED")"
exit $((fails > 0))
