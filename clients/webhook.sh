#!/usr/bin/env bash
# Bucket notifications the MinIO way: ss33 started with MINIO_NOTIFY_WEBHOOK_* settings, the official mc
# subscribing a bucket with `mc event add`, and a webhook receiving the S3 event JSON.
# usage: bash webhook.sh <ss33 image> <path/to/official mc> [host port, default 9200]
set -euo pipefail
image=$1 mc=$2 port=${3:-9200} net=ss33-webhook-check
export MC_CONFIG_DIR=$(mktemp -d)
cleanup() { docker rm -f ss33-webhook-s3 ss33-webhook-hook >/dev/null 2>&1 || true; docker network rm $net >/dev/null 2>&1 || true; rm -rf "$MC_CONFIG_DIR"; }
trap cleanup EXIT
cleanup
docker network create $net >/dev/null
# The receiver prints each POSTed body on one line.
docker run -d --name ss33-webhook-hook --network $net python:3.13-slim python -u -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        print(self.rfile.read(int(self.headers["Content-Length"])).decode(), flush=True)
        self.send_response(200); self.end_headers()
http.server.HTTPServer(("", 8080), H).serve_forever()' >/dev/null
docker run -d --name ss33-webhook-s3 --network $net -p "$port:9000" \
  -e MINIO_NOTIFY_WEBHOOK_ENABLE_PRIMARY=on -e MINIO_NOTIFY_WEBHOOK_ENDPOINT_PRIMARY=http://ss33-webhook-hook:8080/ "$image" >/dev/null
until curl -fsS "http://127.0.0.1:$port/minio/health/live" >/dev/null 2>&1; do sleep 0.2; done

"$mc" alias set local "http://127.0.0.1:$port" minioadmin minioadmin >/dev/null
"$mc" mb local/events >/dev/null
"$mc" event add local/events arn:minio:sqs::PRIMARY:webhook --event put,delete --suffix .jpg >/dev/null
echo "PASS mc event add"
"$mc" event ls local/events | grep -q "arn:minio:sqs::PRIMARY:webhook"
echo "PASS mc event ls"
echo img >"$MC_CONFIG_DIR/cat.jpg" && echo txt >"$MC_CONFIG_DIR/notes.txt"
"$mc" cp "$MC_CONFIG_DIR/cat.jpg" "$MC_CONFIG_DIR/notes.txt" local/events/ >/dev/null
"$mc" rm local/events/cat.jpg >/dev/null
for _ in $(seq 50); do [ "$(docker logs ss33-webhook-hook 2>/dev/null | grep -c EventName)" -ge 2 ] && break; sleep 0.1; done
events=$(docker logs ss33-webhook-hook 2>/dev/null | grep EventName)
echo "$events" | grep -q '"EventName":"s3:ObjectCreated:Put","Key":"events/cat.jpg"'
echo "PASS ObjectCreated:Put delivered"
echo "$events" | grep -q '"EventName":"s3:ObjectRemoved:Delete","Key":"events/cat.jpg"'
echo "PASS ObjectRemoved:Delete delivered"
! echo "$events" | grep -q notes.txt
echo "PASS suffix filter"
echo "webhook notifications: ALL PASS"
