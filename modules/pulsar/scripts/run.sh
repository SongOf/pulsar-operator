#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: run.sh {run} [process flags]' ;;
  run) go_build pulsar-operator pulsar
    export WEB_ROOT="${WEB_ROOT:-$REPO_ROOT/web/dist}"
    exec "$REPO_ROOT/bin/pulsar-operator" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
