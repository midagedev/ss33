#!/usr/bin/env bash
# Terraform apply, a no-change plan and destroy of clients/terraform against an S3 endpoint.
# usage: bash terraform.sh http://127.0.0.1:9000   (terraform on PATH, or TERRAFORM="docker run ..." )
set -euo pipefail
ep=$1 tf=${TERRAFORM:-terraform}
cd "$(dirname "$0")/terraform"
$tf init -input=false -no-color >/dev/null
$tf apply -input=false -auto-approve -no-color -var "endpoint=$ep" >/dev/null
echo "PASS apply"
$tf plan -input=false -detailed-exitcode -no-color -var "endpoint=$ep" >/dev/null # exit 2 means changes pending
echo "PASS plan shows no changes"
$tf destroy -input=false -auto-approve -no-color -var "endpoint=$ep" >/dev/null
echo "PASS destroy"
echo "terraform $($tf version -json | grep -o '"terraform_version": *"[^"]*"' | cut -d'"' -f4): ALL PASS"
