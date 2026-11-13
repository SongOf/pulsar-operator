#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: generate.sh {objects|manifests}' ;;
  objects) controller_gen object paths=./api/pulsar/... ;;
  manifests) controller_gen crd paths=./api/pulsar/... output:crd:artifacts:config=config/pulsar/crd
    controller_gen rbac:roleName=pulsar-operator paths=./internal/pulsar/... output:rbac:artifacts:config=config/pulsar/rbac ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
