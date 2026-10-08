#!/usr/bin/env bash
# The Docker registry (distribution) with its S3 storage driver on ss33: push a multi-layer image, pull it
# back, delete the manifest and garbage-collect.
# usage: bash registry.sh <ss33 image> [registry port, default 5000]
set -euo pipefail
image=$1 port=${2:-5000} net=ss33-registry-check
cleanup() { docker rm -f ss33-registry-s3 ss33-registry >/dev/null 2>&1 || true; docker network rm $net >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup
docker network create $net >/dev/null
docker run -d --name ss33-registry-s3 --network $net "$image" >/dev/null
docker run --rm --network $net --entrypoint /bin/sh "$image" -c \
  'until mc alias set s3 http://ss33-registry-s3:9000 minioadmin minioadmin >/dev/null 2>&1; do sleep 0.2; done; mc mb s3/registry >/dev/null'
docker run -d --name ss33-registry --network $net -p "$port:5000" \
  -e REGISTRY_STORAGE=s3 -e REGISTRY_STORAGE_S3_REGIONENDPOINT=http://ss33-registry-s3:9000 -e REGISTRY_STORAGE_S3_REGION=us-east-1 \
  -e REGISTRY_STORAGE_S3_BUCKET=registry -e REGISTRY_STORAGE_S3_ACCESSKEY=minioadmin -e REGISTRY_STORAGE_S3_SECRETKEY=minioadmin \
  -e REGISTRY_STORAGE_S3_FORCEPATHSTYLE=true -e REGISTRY_STORAGE_S3_SECURE=false -e REGISTRY_STORAGE_DELETE_ENABLED=true \
  registry:3 >/dev/null
until curl -fsS "http://127.0.0.1:$port/v2/" >/dev/null 2>&1; do sleep 0.2; done

src=python:3.13-slim dst=127.0.0.1:$port/check/python:latest
docker pull -q $src >/dev/null
docker tag $src $dst
docker push -q $dst >/dev/null
echo "PASS push $(docker image inspect $src --format '{{len .RootFS.Layers}}') layers"
digest=$(curl -fsS -I -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://127.0.0.1:$port/v2/check/python/manifests/latest" | tr -d '\r' | awk -F': ' 'tolower($1)=="docker-content-digest" {print $2}')
docker rmi -f $dst >/dev/null
docker pull -q $dst >/dev/null
echo "PASS pull"
curl -fsS -X DELETE "http://127.0.0.1:$port/v2/check/python/manifests/$digest"
echo "PASS delete manifest"
docker exec ss33-registry registry garbage-collect /etc/distribution/config.yml >/dev/null 2>&1
echo "PASS garbage-collect"
docker rmi -f $dst >/dev/null
echo "registry:3 on ss33: ALL PASS"
