#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: test.sh {unit|integration|e2e|all}; e2e requires a dedicated real cluster' ;;
  unit) repo go test ./api/storage/... ./internal/storage/... ./cmd/storage-api/... ./cmd/storage-plan/... ./cmd/storage-benchmark/... ./web/storage/... -count=1 "$@" ;;
  integration) repo go test ./test/integration/storage/... -count=1 "$@"
    go_build diskfleet-api storage-api
    repo node web/scripts/check-storage-ui.cjs
    python_tests storage ;;
  e2e) exec bash "$REPO_ROOT/test/e2e/storage/volumes.sh" "$@" ;;
  all) bash "${BASH_SOURCE[0]}" unit "$@"; bash "${BASH_SOURCE[0]}" integration "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
