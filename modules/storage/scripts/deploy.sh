#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
action=${1:-help}
if [[ $# -gt 0 ]]; then shift; fi
case "$action" in
  help|-h|--help) printf '%s\n' 'Usage: deploy.sh {render|apply|csi-install} [tool flags]; apply/CSI installation change the selected cluster' ;;
  render) module_render storage ;;
  apply) module_deploy storage "$@" ;;
  csi-install) require helm
    helm repo add openebs-lvm https://openebs.github.io/lvm-localpv
    helm repo update openebs-lvm
    helm upgrade --install openebs-lvm openebs-lvm/lvm-localpv \
      --namespace openebs --create-namespace --version "${CSI_CHART_VERSION:-1.10.1}" "$@" ;;
  *) die "Unknown action: $action (see --help)" ;;
esac
