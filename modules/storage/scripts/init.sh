#!/usr/bin/env bash
set -euo pipefail
source "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/lib/common.sh"
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo 'Usage: init.sh (local dependencies only)'
  exit 0
fi
[[ $# == 0 ]] || die 'Usage: init'; go_init; require node; require "${PYTHON:-python3}"
