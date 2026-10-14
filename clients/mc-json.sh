#!/usr/bin/env bash
# Built-in `mc --json` against the official MinIO client, one endpoint, separate config dirs.
# usage: bash clients/mc-json.sh <endpoint> <official-mc> <ss33-mc-command…>
#   go build -o /tmp/ss33 ./cmd/ss33 && bash clients/mc-json.sh http://127.0.0.1:9000 /tmp/mc-latest /tmp/ss33 mc
# The ss33 side runs on the host, not via `docker exec`: `cp` uploads files from this machine.
set -uo pipefail
if [ $# -lt 3 ]; then
  echo "usage: bash clients/mc-json.sh <endpoint> <official-mc> <ss33-mc-command…>" >&2
  exit 2
fi
ep=${1%/}
off=$2
shift 2
ss=("$@")

# A host MC_JSON / MC_HOST_* / MC_CONFIG_DIR would change one side only.
while IFS='=' read -r name _; do
  case "$name" in
    MC_HOST_* | MC_JSON | MC_CONFIG_DIR) unset "$name" ;;
  esac
done < <(env)

off_cfg=$(mktemp -d)
ss_cfg=$(mktemp -d)
work=$(mktemp -d)
trap 'rm -rf "$off_cfg" "$ss_cfg" "$work"' EXIT
fails=0
id=$(date +%s)-$$
seed=mcjson-$id
seedb=mcjson-$id-b
offb=mcjson-off-$id
ssb=mcjson-ss-$id

run_off() { MC_CONFIG_DIR="$off_cfg" "$off" "$@"; }
run_ss() { MC_CONFIG_DIR="$ss_cfg" "${ss[@]}" "$@"; }

# Top-level keys, the error object walked recursively, and the values both sides must share.
read_filter='{k: keys_unsorted,
  e: (if (.error|type)=="object" then
        (.error | [.. | objects | keys_unsorted])
      else null end),
  v: {status, type, key, size, etag, name, url, permission, prefix, objects}}'

note_fail() {
  fails=$((fails + 1))
  echo "FAIL $1"
  diff -u "$work/off.sig" "$work/ss.sig" | sed 's/^/     /' || true
  echo "     --- stdout official ---"; sed 's/^/     /' "$work/off.out"
  echo "     --- stdout ss33 ---"; sed 's/^/     /' "$work/ss.out"
  echo "     --- stderr official ---"; sed 's/^/     /' "$work/off.err"
  echo "     --- stderr ss33 ---"; sed 's/^/     /' "$work/ss.err"
}

sig_of() {
  local src=$1 code=$2 dst=$3 filter=$4
  echo "exit $code" >"$dst"
  if [ ! -s "$src" ]; then
    return 0
  fi
  if ! jq -c "$filter" "$src" >>"$dst"; then
    echo "JQFAIL" >>"$dst"
    return 1
  fi
}

cmp_read() {
  local name=$1
  shift
  local oe=0 se=0
  run_off "$@" >"$work/off.out" 2>"$work/off.err" || oe=$?
  run_ss "$@" >"$work/ss.out" 2>"$work/ss.err" || se=$?
  sig_of "$work/off.out" "$oe" "$work/off.sig" "$read_filter" || { note_fail "$name"; return; }
  sig_of "$work/ss.out" "$se" "$work/ss.sig" "$read_filter" || { note_fail "$name"; return; }
  if ! diff -q "$work/off.sig" "$work/ss.sig" >/dev/null; then
    note_fail "$name"
    return
  fi
  echo "PASS $name"
}

# ls ALIAS lists every bucket on the server. Keep only this script's.
cmp_alias() {
  local name=$1
  shift
  local oe=0 se=0
  run_off "$@" >"$work/off.out" 2>"$work/off.err" || oe=$?
  run_ss "$@" >"$work/ss.out" 2>"$work/ss.err" || se=$?
  jq -c --arg a "$seed" --arg b "$seedb" \
    'select(.key == ($a+"/") or .key == ($b+"/") or .key == $a or .key == $b)' \
    "$work/off.out" >"$work/off.f" || { note_fail "$name"; return; }
  jq -c --arg a "$seed" --arg b "$seedb" \
    'select(.key == ($a+"/") or .key == ($b+"/") or .key == $a or .key == $b)' \
    "$work/ss.out" >"$work/ss.f" || { note_fail "$name"; return; }
  if [ ! -s "$work/off.f" ] || [ ! -s "$work/ss.f" ]; then
    fails=$((fails + 1))
    echo "FAIL $name (no lines for the script's buckets)"
    echo "     --- stdout official ---"; sed 's/^/     /' "$work/off.out"
    echo "     --- stdout ss33 ---"; sed 's/^/     /' "$work/ss.out"
    return
  fi
  sig_of "$work/off.f" "$oe" "$work/off.sig" "$read_filter" || { note_fail "$name"; return; }
  sig_of "$work/ss.f" "$se" "$work/ss.sig" "$read_filter" || { note_fail "$name"; return; }
  if ! diff -q "$work/off.sig" "$work/ss.sig" >/dev/null; then
    note_fail "$name"
    return
  fi
  echo "PASS $name"
}

# Each binary mutates its own bucket. Compare keys and status, not names or timings.
cmp_mut() {
  local name=$1
  shift
  local n=$(($# / 2)) oe=0 se=0
  run_off --json "${@:1:n}" >"$work/off.out" 2>"$work/off.err" || oe=$?
  run_ss --json "${@:n+1}" >"$work/ss.out" 2>"$work/ss.err" || se=$?
  local filter='{k: keys_unsorted, s: .status}'
  sig_of "$work/off.out" "$oe" "$work/off.sig" "$filter" || { note_fail "$name"; return; }
  sig_of "$work/ss.out" "$se" "$work/ss.sig" "$filter" || { note_fail "$name"; return; }
  if ! diff -q "$work/off.sig" "$work/ss.sig" >/dev/null; then
    note_fail "$name"
    return
  fi
  echo "PASS $name"
}

seed_run() {
  if ! "$@" >"$work/seed.out" 2>&1; then
    echo "FAIL seed: $*"
    sed 's/^/     /' "$work/seed.out"
    exit 1
  fi
}

mkdir -p "$work/d/sub" "$work/dir"
echo abc >"$work/a.txt"
echo abc >"$work/dir/p.txt"
echo a >"$work/d/a.txt"
echo bb >"$work/d/sub/b.txt"
echo abc >"$work/z.txt"

seed_run run_off alias set local "$ep" minioadmin minioadmin
seed_run run_ss alias set local "$ep" minioadmin minioadmin
seed_run run_off mb "local/$seed"
seed_run run_off mb "local/$seedb"
seed_run run_off cp "$work/a.txt" "local/$seed/a.txt"
seed_run run_off cp --attr 'X-Amz-Meta-Owner=kim;Cache-Control=no-cache' "$work/a.txt" "local/$seed/meta.txt"
seed_run run_off cp --recursive "$work/d/" "local/$seed/tree/"
seed_run run_off cp "$work/dir/p.txt" "local/$seed/dir/p.txt"
seed_run run_off tag set "local/$seed/a.txt" 'k1=v1&k2=v2'

cmp_read "ls" --json ls "local/$seed"
cmp_read "ls --recursive --json" ls --recursive --json "local/$seed/tree"
cmp_read "ls prefix" --json ls "local/$seed/dir"
cmp_alias "ls alias" --json ls local
cmp_read "stat" --json stat "local/$seed/a.txt"
cmp_read "stat missing" --json stat "local/$seed/nope.txt"
cmp_read "ls missing bucket" --json ls "local/mcjson-missing-$id"
cmp_read "find --name" --json find "local/$seed" --name 'b*'
cmp_read "du" --json du "local/$seed"
cmp_read "tag list" --json tag list "local/$seed/a.txt"
cmp_read "anonymous get" --json anonymous get "local/$seed"
cmp_read "version info" --json version info "local/$seed"

cmp_mut "mb" mb "local/$offb" mb "local/$ssb"
cmp_mut "cp" cp "$work/z.txt" "local/$offb/z.txt" cp "$work/z.txt" "local/$ssb/z.txt"
cmp_mut "tag set" tag set "local/$offb/z.txt" 'k=v&k2=v2' tag set "local/$ssb/z.txt" 'k=v&k2=v2'
cmp_mut "rm" rm "local/$offb/z.txt" rm "local/$ssb/z.txt"
cmp_mut "anonymous set" anonymous set download "local/$offb" anonymous set download "local/$ssb"
cmp_mut "rb" rb "local/$offb" rb "local/$ssb"

run_off rb --force "local/$seed" >/dev/null 2>&1 || true
run_off rb --force "local/$seedb" >/dev/null 2>&1 || true

echo "mc-json ($off vs ${ss[*]}): $([ "$fails" = 0 ] && echo ALL PASS || echo "$fails FAILED")"
exit $((fails > 0))
