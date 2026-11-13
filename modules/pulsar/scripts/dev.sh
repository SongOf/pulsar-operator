#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: dev.sh {init|build|image|run|unit|integration|test|render|deploy|e2e|generate|manifests} [args]; implementations: init/build/run/test/deploy/generate.sh' ;;
  init) exec bash "$REPO_ROOT/modules/pulsar/scripts/init.sh" "$@" ;;
  build) exec bash "$REPO_ROOT/modules/pulsar/scripts/build.sh" all "$@" ;;
  image) exec bash "$REPO_ROOT/modules/pulsar/scripts/build.sh" image "$@" ;;
  run) exec bash "$REPO_ROOT/modules/pulsar/scripts/run.sh" run "$@" ;;
  unit|integration) exec bash "$REPO_ROOT/modules/pulsar/scripts/test.sh" "$action" "$@" ;;
  test) exec bash "$REPO_ROOT/modules/pulsar/scripts/test.sh" all "$@" ;;
  render) exec bash "$REPO_ROOT/modules/pulsar/scripts/deploy.sh" render "$@" ;;
  deploy) exec bash "$REPO_ROOT/modules/pulsar/scripts/deploy.sh" apply "$@" ;;
  e2e) exec bash "$REPO_ROOT/modules/pulsar/scripts/test.sh" e2e "$@" ;;
  generate) exec bash "$REPO_ROOT/modules/pulsar/scripts/generate.sh" objects "$@" ;;
  manifests) exec bash "$REPO_ROOT/modules/pulsar/scripts/generate.sh" manifests "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
