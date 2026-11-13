#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: test.sh {unit|integration|e2e|all}; e2e requires a dedicated real cluster' ;;
  unit) repo go test ./api/pulsar/... ./internal/pulsar/... ./cmd/pulsar/... -count=1 "$@" ;;
  integration) envtest_init
    repo go test -tags=integration -run '^TestIntegration' ./internal/pulsar/controller ./internal/pulsar/httpapi -count=1 "$@"
    python_tests pulsar ;;
  e2e) exec bash "$REPO_ROOT/test/e2e/pulsar/lifecycle.sh" "$@" ;;
  all) bash "${BASH_SOURCE[0]}" unit "$@"; bash "${BASH_SOURCE[0]}" integration "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
