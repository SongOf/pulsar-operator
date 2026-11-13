#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf 'Usage: %s EXISTING_DISKFLEET_STORAGECLASS\n' "$0"
  printf 'Run the storage E2E against an existing dedicated test cluster. Optional: TIMEOUT=300 TEST_IMAGE=busybox:1.37.0\n'
  printf 'Creates test resources; verifies persistence, expansion and Retain. Does not initialize disks or delete PVs.\n'
  exit 0
fi
REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
exec bash "$REPO_ROOT/test/e2e/lib/storage.sh" "$@"
