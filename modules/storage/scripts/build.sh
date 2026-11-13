#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: build.sh {all|image|api|plan|benchmark}' ;;
  all) go_build diskfleet-api storage-api; go_build storage-plan storage-plan; go_build storage-benchmark storage-benchmark ;;
  image) module_image storage "${STORAGE_IMG:-pulsar-operator-storage:dev}" ;;
  api) go_build diskfleet-api storage-api ;;
  plan) go_build storage-plan storage-plan ;;
  benchmark) go_build storage-benchmark storage-benchmark ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
