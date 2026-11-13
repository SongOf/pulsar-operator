#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: test.sh {unit|integration|all}' ;;
  unit) repo go test ./internal/agent/... ./cmd/agent/... -count=1 "$@" ;;
  integration) envtest_init
    repo go test -tags=integration -run '^TestIntegration' ./internal/agent/controller -count=1 "$@" ;;
  all) bash "${BASH_SOURCE[0]}" unit "$@"; bash "${BASH_SOURCE[0]}" integration "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
