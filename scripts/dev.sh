#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: dev.sh {init|test|unit-shared|test-go|render|preflight-two-node|deploy-two-node|e2e-pulsar-storage|e2e-pulsar-shared-disk|e2e-system-disk} [args]' ;;
  init) exec bash "$REPO_ROOT/scripts/init.sh" "$@" ;;
  test) if [[ $# == 0 ]]; then set -- all; fi; exec bash "$REPO_ROOT/scripts/test.sh" "$@" ;;
  unit-shared) exec bash "$REPO_ROOT/scripts/test.sh" shared "$@" ;;
  test-go) exec bash "$REPO_ROOT/scripts/test.sh" go "$@" ;;
  render) exec bash "$REPO_ROOT/scripts/deploy/render.sh" "$@" ;;
  preflight-two-node) exec bash "$REPO_ROOT/scripts/deploy/two-node.sh" preflight "$@" ;;
  deploy-two-node) exec bash "$REPO_ROOT/scripts/deploy/two-node.sh" install "$@" ;;
  e2e-pulsar-storage|e2e-pulsar-shared-disk|e2e-system-disk) exec bash "$REPO_ROOT/scripts/test.sh" "$action" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
