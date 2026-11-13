#!/usr/bin/env bash
set -euo pipefail
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf 'Usage: %s TEST_NAMESPACE PULSAR_CLUSTER_NAME\n' "$0"
  printf 'Requires an existing dedicated PulsarCluster and namespace labeled storage.diskfleet.io/integration=pulsar.\n'
  printf 'Recreates bookie-0, grows ledgers and checks Retain. Optional: TIMEOUT=600 TEST_IMAGE=busybox:1.37.0\n'
  exit 0
fi
REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)"
exec bash "$REPO_ROOT/test/e2e/lib/pulsar-storage.sh" "$@"
