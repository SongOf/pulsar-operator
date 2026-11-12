#!/usr/bin/env bash
# Explicit acceptance entry point for the two-node, one-data-disk-per-node lab.
set -euo pipefail
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf 'Usage: %s TEST_NAMESPACE PULSAR_CLUSTER_NAME\n' "$0"
  printf 'Requires two-node-shared-disk-lab integration/profile labels, exactly two Ready labeled nodes and the same two Ready pools for all roles.\n'
  printf 'Recreates bookie-0, tests 20 messages, grows every ledger by 1Gi and leaves a separate 1Gi Retain PV.\n'
  printf 'Optional: PREFLIGHT_ONLY=1 (read-only), TIMEOUT=600, TEST_IMAGE=busybox:1.37.0\n'
  printf 'This lab does not certify host-failure HA or journal/ledger physical isolation.\n'
  exit 0
fi
REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
exec bash "$REPO_ROOT/modules/storage/scripts/e2e-pulsar.sh" --profile two-node-shared-disk-lab "$@"
