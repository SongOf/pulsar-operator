#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/lib/common.sh"
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo 'Usage: init.sh (requires Node 22/npm)'
  exit 0
fi
require npm
cd "$REPO_ROOT/web"
npm ci
