#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo 'Usage: init.sh [--integration]. Prepare shared Go, Node and optional envtest dependencies.'
  exit 0
fi
module_init "$@"
require node
require "${PYTHON:-python3}"
bash "$REPO_ROOT/web/scripts/init.sh"
