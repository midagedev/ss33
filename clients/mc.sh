#!/usr/bin/env bash
# The official MinIO client (minio/mc, built from source) against an S3 endpoint: the commands bootstrap
# scripts use.
# usage: bash mc.sh http://127.0.0.1:9000 [path/to/mc]   (credentials minioadmin / minioadmin)
set -uo pipefail
ep=${1%/} mc=${2:-mc}
export MC_CONFIG_DIR=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$MC_CONFIG_DIR" "$work"' EXIT
fails=0
check() { if (set -e; eval "$2") >"$work/out" 2>&1; then echo "PASS $1"; else fails=$((fails + 1)); echo "FAIL $1"; sed 's/^/     /' "$work/out"; fi; }

mkdir -p "$work/seed/a" && echo hello >"$work/seed/a/1.txt" && head -c 70000000 /dev/urandom >"$work/seed/big.bin"

check "alias set" '"$mc" alias set local "$ep" minioadmin minioadmin'
check "ready" '"$mc" ready local'
check "mb --ignore-existing, twice" '"$mc" mb --ignore-existing local/clients-mc && "$mc" mb --ignore-existing local/clients-mc'
check "anonymous set download" '"$mc" anonymous set download local/clients-mc'
check "cp --recursive (with a multipart object)" '"$mc" cp --recursive "$work/seed/" local/clients-mc/'
check "anonymous GET" '[ "$(curl -fsS "$ep/clients-mc/a/1.txt")" = hello ]'
check "ls --recursive" '[ "$("$mc" ls --recursive local/clients-mc | wc -l)" -eq 2 ]'
check "cp download round-trips" '"$mc" cp local/clients-mc/big.bin "$work/back.bin" && cmp "$work/seed/big.bin" "$work/back.bin"'
check "cp --recursive bucket to bucket" '"$mc" mb local/clients-mc-copy && "$mc" cp --recursive local/clients-mc/ local/clients-mc-copy/ &&
  [ "$("$mc" ls --recursive local/clients-mc-copy | wc -l)" -eq 2 ]'
check "diff of identical buckets is empty" 'out=$("$mc" diff local/clients-mc local/clients-mc-copy) && [ -z "$out" ]'
check "diff reports a missing object" '"$mc" rm local/clients-mc-copy/a/1.txt && "$mc" diff local/clients-mc local/clients-mc-copy | grep -q "a/1.txt"'
check "mirror" '"$mc" mb local/clients-mc-mirror && "$mc" mirror local/clients-mc local/clients-mc-mirror &&
  [ "$("$mc" ls --recursive local/clients-mc-mirror | wc -l)" -eq 2 ]'
check "stat" '"$mc" stat local/clients-mc/a/1.txt'
check "cat" '[ "$("$mc" cat local/clients-mc/a/1.txt)" = hello ]'
check "rm" '"$mc" rm local/clients-mc/a/1.txt'
check "rb --force" '"$mc" rb --force local/clients-mc && "$mc" rb --force local/clients-mc-copy && "$mc" rb --force local/clients-mc-mirror'
echo "official mc ($mc): $([ $fails = 0 ] && echo ALL PASS || echo "$fails FAILED")"
exit $((fails > 0))
