#!/usr/bin/env bash
# Sourced by the lifecycle E2E. Credentials travel through curl stdin, never a URL/argv.
console_request() {
  printf 'Authorization: Bearer %s\n' "$CONSOLE_TOKEN" |
    curl --fail --silent --show-error --max-time 10 --header @- \
      --header 'Content-Type: application/json' "$@"
}

console_identity() {
  python3 -c 'import json,sys
m=json.load(sys.stdin).get("metadata", {})
values=[m.get("uid"), m.get("resourceVersion")]
if any(not isinstance(v,str) or not v or any(c.isspace() for c in v) for v in values):
    sys.exit("Console response lacks a valid UID/resourceVersion")
print(*values)'
}

console_init() {
  # A caller using bash -x must not trace secret reads or HTTP authentication.
  set +x
  [[ "$NS" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && "$NAME" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || {
    echo 'Invalid namespace or PulsarCluster name' >&2; return 1;
  }
  if [[ -n "${CONSOLE_TOKEN_FILE:-}" ]]; then
    CONSOLE_TOKEN=$(cat -- "$CONSOLE_TOKEN_FILE") || return 1
  else
    CONSOLE_TOKEN=$(kubectl -n pulsar-operator-system get secret pulsar-operator-console \
      -o 'jsonpath={.data.token}' | base64 --decode) || return 1
  fi
  [[ ${#CONSOLE_TOKEN} -ge 24 && ! "$CONSOLE_TOKEN" =~ [[:space:]] ]] || {
    echo 'Console token is missing or invalid; use CONSOLE_TOKEN_FILE or the console Secret' >&2; return 1;
  }
  local identity
  identity=$(console_request "$API/pulsarclusters/$NS/$NAME" | console_identity) || return 1
  read -r CONSOLE_CLUSTER_UID CONSOLE_CLUSTER_VERSION <<< "$identity"
}

console_patch() {
  local identity uid version
  identity=$(console_request "$API/pulsarclusters/$NS/$NAME" | console_identity) || return 1
  read -r uid version <<< "$identity"
  [[ "$uid" == "$CONSOLE_CLUSTER_UID" ]] || {
    echo 'PulsarCluster was replaced; refusing to modify the replacement' >&2; return 1;
  }
  console_request --output /dev/null --request PATCH \
    --header "X-Resource-UID: $uid" --header "X-Resource-Version: $version" \
    --data "{\"spec\":$1}" "$API/pulsarclusters/$NS/$NAME"
}
