#!/usr/bin/env bash
# Shared by module entrypoints; sourced, not executed.
set -euo pipefail
REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

die() { printf '%s\n' "$*" >&2; exit 2; }
require() { command -v "$1" >/dev/null 2>&1 || die "Missing tool: $1"; }
repo() { (cd "$REPO_ROOT" && "$@"); }
python_tests() {
  require "${PYTHON:-python3}"
  repo "${PYTHON:-python3}" -m unittest discover -s "test/integration/$1" -v
}
go_init() { require go; repo go mod download; }
go_build() { require go; repo go build -o "bin/$1" "./cmd/$2"; }
# Overrides accept an executable path or a whitespace-separated command prefix.
# Shell expressions are deliberately not evaluated.
run_tool() {
  local prefix="$1"
  shift
  local -a command_parts
  if [[ -x "$prefix" ]]; then command_parts=("$prefix"); else read -r -a command_parts <<< "$prefix"; fi
  repo "${command_parts[@]}" "$@"
}
envtest_init() {
  if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
    require go
    KUBEBUILDER_ASSETS="$(run_tool "${SETUP_ENVTEST:-go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23}" use "${ENVTEST_K8S_VERSION:-1.36.x}" -p path)"
  fi
  [[ -x "$KUBEBUILDER_ASSETS/kube-apiserver" && -x "$KUBEBUILDER_ASSETS/etcd" ]] || die 'envtest needs executable kube-apiserver and etcd in KUBEBUILDER_ASSETS'
  export KUBEBUILDER_ASSETS
  export USE_EXISTING_CLUSTER=false
}
module_init() {
  [[ $# == 0 || ( $# == 1 && "$1" == --integration ) ]] || die 'Usage: init [--integration]'
  go_init
  if [[ ${1:-} == --integration ]]; then envtest_init; fi
}
module_image() {
  require docker
  repo docker build -f "modules/$1/Dockerfile" -t "$2" .
}
module_render() { require "${KUBECTL:-kubectl}"; "${KUBECTL:-kubectl}" kustomize "$REPO_ROOT/config/$1/default"; }
module_deploy() {
  local module="$1"
  shift
  require "${KUBECTL:-kubectl}"
  "${KUBECTL:-kubectl}" apply -k "$REPO_ROOT/config/$module/default" "$@"
}
controller_gen() { run_tool "${CONTROLLER_GEN:-go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1}" "$@"; }
