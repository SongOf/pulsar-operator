#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: test.sh {unit|integration|system|offline|all|shared|go|e2e-pulsar-storage|e2e-pulsar-shared-disk|e2e-system-disk} [args]' ;;
  unit) for module in pulsar storage agent; do bash "$REPO_ROOT/modules/$module/scripts/test.sh" unit "$@"; done
    bash "$REPO_ROOT/web/scripts/test.sh"
    bash "$REPO_ROOT/web/scripts/build.sh"
    bash "${BASH_SOURCE[0]}" shared "$@" ;;
  integration) for module in pulsar storage agent; do bash "$REPO_ROOT/modules/$module/scripts/test.sh" integration "$@"; done ;;
  system) exec bash "$REPO_ROOT/test/integration/system/run.sh" "$@" ;;
  offline) bash "${BASH_SOURCE[0]}" unit "$@"
    bash "$REPO_ROOT/modules/storage/scripts/test.sh" integration "$@"
    bash "${BASH_SOURCE[0]}" system ;;
  all) bash "${BASH_SOURCE[0]}" unit "$@"; bash "${BASH_SOURCE[0]}" integration "$@"; bash "${BASH_SOURCE[0]}" system ;;
  shared) repo go test ./internal/kubeconfig/... -count=1 "$@" ;;
  go) envtest_init; repo go test -tags='integration systemintegration' ./... -count=1 "$@" ;;
  e2e-pulsar-storage) exec bash "$REPO_ROOT/test/e2e/system/pulsar-storage.sh" "$@" ;;
  e2e-pulsar-shared-disk) exec bash "$REPO_ROOT/test/e2e/system/pulsar-shared-disk.sh" "$@" ;;
  e2e-system-disk) require "${PYTHON:-python3}"; exec "${PYTHON:-python3}" "$REPO_ROOT/test/e2e/system/system-disk-smoke.py" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
