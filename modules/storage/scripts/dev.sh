#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: dev.sh {init|build|image|run|unit|integration|test|render|deploy|build-api|build-plan|build-benchmark|plan|benchmark|e2e|generate|csi-install} [args]; implementations: init/build/run/test/deploy/generate.sh' ;;
  init) exec bash "$REPO_ROOT/modules/storage/scripts/init.sh" "$@" ;;
  build) exec bash "$REPO_ROOT/modules/storage/scripts/build.sh" all "$@" ;;
  image) exec bash "$REPO_ROOT/modules/storage/scripts/build.sh" image "$@" ;;
  run) exec bash "$REPO_ROOT/modules/storage/scripts/run.sh" run "$@" ;;
  unit|integration) exec bash "$REPO_ROOT/modules/storage/scripts/test.sh" "$action" "$@" ;;
  test) exec bash "$REPO_ROOT/modules/storage/scripts/test.sh" all "$@" ;;
  render) exec bash "$REPO_ROOT/modules/storage/scripts/deploy.sh" render "$@" ;;
  deploy) exec bash "$REPO_ROOT/modules/storage/scripts/deploy.sh" apply "$@" ;;
  build-api) exec bash "$REPO_ROOT/modules/storage/scripts/build.sh" api "$@" ;;
  build-plan) exec bash "$REPO_ROOT/modules/storage/scripts/build.sh" plan "$@" ;;
  build-benchmark) exec bash "$REPO_ROOT/modules/storage/scripts/build.sh" benchmark "$@" ;;
  plan) exec bash "$REPO_ROOT/modules/storage/scripts/run.sh" plan "$@" ;;
  benchmark) exec bash "$REPO_ROOT/modules/storage/scripts/run.sh" benchmark "$@" ;;
  e2e) exec bash "$REPO_ROOT/modules/storage/scripts/test.sh" e2e "$@" ;;
  generate) exec bash "$REPO_ROOT/modules/storage/scripts/generate.sh" objects "$@" ;;
  csi-install) exec bash "$REPO_ROOT/modules/storage/scripts/deploy.sh" csi-install "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
