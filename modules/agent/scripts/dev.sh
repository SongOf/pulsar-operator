#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: dev.sh {init|build|image|run|unit|integration|test|render|deploy|discover} [args]; implementations: init/build/run/test/deploy.sh' ;;
  init) exec bash "$REPO_ROOT/modules/agent/scripts/init.sh" "$@" ;;
  build) exec bash "$REPO_ROOT/modules/agent/scripts/build.sh" all "$@" ;;
  image) exec bash "$REPO_ROOT/modules/agent/scripts/build.sh" image "$@" ;;
  run) exec bash "$REPO_ROOT/modules/agent/scripts/run.sh" run "$@" ;;
  unit|integration) exec bash "$REPO_ROOT/modules/agent/scripts/test.sh" "$action" "$@" ;;
  test) exec bash "$REPO_ROOT/modules/agent/scripts/test.sh" all "$@" ;;
  render) exec bash "$REPO_ROOT/modules/agent/scripts/deploy.sh" render "$@" ;;
  deploy) exec bash "$REPO_ROOT/modules/agent/scripts/deploy.sh" apply "$@" ;;
  discover) exec bash "$REPO_ROOT/modules/agent/scripts/run.sh" discover "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
