#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)/lib/common.sh"
case "${1:-all}" in
  all|storage) target="bundles/${1:-all}" ;;
  two-node) target=overlays/two-node ;;
  system-disk-storage) target=labs/system-disk/storage ;;
  --help|-h) echo 'Usage: render.sh [all|storage|two-node|system-disk-storage]. Render only; no cluster connection.'; exit 0 ;;
  *) die 'Unknown deployment; see --help' ;;
esac
require "${KUBECTL:-kubectl}"
exec "${KUBECTL:-kubectl}" kustomize "$REPO_ROOT/deploy/$target"
