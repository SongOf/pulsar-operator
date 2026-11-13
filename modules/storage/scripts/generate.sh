#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: generate.sh {objects}' ;;
  objects) controller_gen object paths=./api/storage/... ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
