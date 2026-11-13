#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: build.sh {all|image}' ;;
  all) go_build diskfleet-agent agent ;;
  image) module_image agent "${AGENT_IMG:-pulsar-operator-agent:dev}" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
