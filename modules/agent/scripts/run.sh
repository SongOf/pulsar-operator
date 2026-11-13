#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: run.sh {run|discover} [process flags]' ;;
  run) go_build diskfleet-agent agent; exec "$REPO_ROOT/bin/diskfleet-agent" "$@" ;;
  discover) go_build diskfleet-agent agent; exec "$REPO_ROOT/bin/diskfleet-agent" --discover "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
