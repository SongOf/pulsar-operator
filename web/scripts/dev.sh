#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: dev.sh {init|build|run|unit|test} [args]' ;;
  init|build|run) exec bash "$REPO_ROOT/web/scripts/$action.sh" "$@" ;;
  unit|test) exec bash "$REPO_ROOT/web/scripts/test.sh" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
