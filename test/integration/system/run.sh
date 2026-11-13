#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
if [[ ${1:-} == --help ]]; then
  echo 'Usage: run.sh. Runs Storage/Agent contract, gateway, repository, installer and E2E preflight fixtures without a real cluster.'
  exit 0
fi
go_build storage-plan storage-plan
repo go test -tags=systemintegration -run '^TestSystem' ./internal/storage/cluster -count=1
repo node --test test/integration/system/gateway.test.mjs
python_tests system
