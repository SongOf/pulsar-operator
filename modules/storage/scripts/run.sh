#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: run.sh {run|plan|benchmark} [process flags]' ;;
  run) go_build diskfleet-api storage-api
    exec "$REPO_ROOT/bin/diskfleet-api" --state-dir "${STATE_DIR:-$REPO_ROOT/modules/storage/.state}" "$@" ;;
  plan) go_build storage-plan storage-plan; exec "$REPO_ROOT/bin/storage-plan" "$@" ;;
  benchmark) go_build storage-benchmark storage-benchmark; exec "$REPO_ROOT/bin/storage-benchmark" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
