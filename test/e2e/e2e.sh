#!/usr/bin/env bash
# End-to-end checks against a real cluster where the operator runs and config/samples/pulsarcluster.yaml was
# applied in namespace "pulsar". It changes the cluster through the console API, so it also tests the console.
#   NS=pulsar NAME=demo test/e2e/e2e.sh
set -euo pipefail
NS=${NS:-pulsar}
NAME=${NAME:-demo}
API=${API:-http://$(kubectl -n pulsar-operator-system get svc pulsar-operator-console -o jsonpath='{.spec.clusterIP}'):8080/api}
fail() { echo "FAIL: $*" >&2; exit 1; }
patch() { curl -sf -m 10 -o /dev/null -X PATCH "$API/pulsarclusters/$NS/$NAME" -d "{\"spec\":$1}" || fail "patch $1"; }
broker() { timeout 120 kubectl -n "$NS" exec "$NAME-broker-0" -- bash -c "$1"; }
phase() { kubectl -n "$NS" get psc "$NAME" -o jsonpath='{.status.phase}'; }
wait_running() {
  for _ in $(seq 1 60); do [ "$(phase)" = Running ] && return 0; sleep 10; done
  fail "cluster not Running, phase $(phase)"
}
wait_bookies() {
  for _ in $(seq 1 60); do
    [ "$(kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.bookies.readyReplicas}')" = "$1" ] &&
      [ "$(kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.phase}')" = Running ] && return 0
    sleep 10
  done
  fail "bookies not $1"
}
# produce N messages to a topic that already has subscription "keep", so they are retained
produce() { broker "bin/pulsar-admin topics create-subscription persistent://public/default/$1 -s keep -m earliest 2>/dev/null || true
  bin/pulsar-client --url pulsar://localhost:6650 produce persistent://public/default/$1 -m m -n $2 2>&1 | grep -q '$2 messages successfully produced'"; }
consume() { broker "timeout 30 bin/pulsar-client --url pulsar://localhost:6650 consume persistent://public/default/$1 -s keep -n $2 2>&1 | grep -q '$2 messages successfully consumed'"; }

echo "1. cluster is running and serves messages through the proxy"
wait_running
broker "bin/pulsar-admin topics create-subscription persistent://public/default/e2e-proxy -s s -m earliest 2>/dev/null || true
  bin/pulsar-client --url pulsar://$NAME-proxy:6650 produce persistent://public/default/e2e-proxy -m x -n 20 2>&1 | grep -q '20 messages successfully produced' &&
  timeout 30 bin/pulsar-client --url pulsar://$NAME-proxy:6650 consume persistent://public/default/e2e-proxy -s s -n 20 2>&1 | grep -q '20 messages successfully consumed'" || fail "proxy produce/consume"

echo "2. bookie scale-out by 2, data on the new bookies, safe scale-in of both, one at a time"
bookies=$(kubectl -n "$NS" get psc "$NAME" -o jsonpath='{.spec.bookkeeper.replicas}')
patch "{\"bookkeeper\":{\"replicas\":$((bookies + 2))}}"
wait_bookies $((bookies + 2))
for t in 1 2 3 4 5 6; do produce "e2e-bk-$t" 300 || fail "produce e2e-bk-$t"; done
patch "{\"bookkeeper\":{\"replicas\":$bookies}}"
# Record every decommission phase seen, to show the bookies leave one at a time.
for _ in $(seq 1 120); do
  kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.decommission.ordinal}:{.status.decommission.phase} sts={.status.bookies.replicas}{"\n"}'
  [ "$(kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.phase}')" = Running ] && [ "$(kubectl -n "$NS" get sts "$NAME-bookie" -o jsonpath='{.spec.replicas}')" = "$bookies" ] && break
  sleep 3
done | uniq
wait_bookies "$bookies"
for o in $bookies $((bookies + 1)); do
  kubectl -n "$NS" get pvc "ledgers-$NAME-bookie-$o" >/dev/null 2>&1 && fail "PVC of decommissioned bookie $o kept"
done
for t in 1 2 3 4 5 6; do consume "e2e-bk-$t" 300 || fail "message loss on e2e-bk-$t after scale-in"; done

echo "3. scale-in below minReplicas is blocked"
min=$(kubectl -n "$NS" get psc "$NAME" -o jsonpath='{.spec.bookkeeper.minReplicas}')
patch "{\"bookkeeper\":{\"replicas\":$((min - 1))}}"
sleep 10
[ "$(kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.conditions[?(@.type=="ScaleInBlocked")].status}')" = True ] || fail "ScaleInBlocked not reported"
[ "$(kubectl -n "$NS" get sts "$NAME-bookie" -o jsonpath='{.spec.replicas}')" -ge "$min" ] || fail "scaled below minReplicas"
patch "{\"bookkeeper\":{\"replicas\":$bookies}}"

echo "4. storage shrink is rejected"
ledger=$(kubectl -n "$NS" get psc "$NAME" -o jsonpath='{.spec.bookkeeper.storage.ledger.size}')
patch '{"bookkeeper":{"storage":{"ledger":{"size":"1Gi"}}}}'
sleep 10
[ "$(kubectl -n "$NS" get bkc "$NAME" -o jsonpath='{.status.conditions[?(@.type=="SpecRejected")].status}')" = True ] || fail "shrink not rejected"
patch "{\"bookkeeper\":{\"storage\":{\"ledger\":{\"size\":\"$ledger\"}}}}"

echo "5. broker scale and config rolling restart"
brokers=$(kubectl -n "$NS" get psc "$NAME" -o jsonpath='{.spec.broker.replicas}')
patch "{\"broker\":{\"replicas\":$((brokers + 1)),\"config\":{\"maxMessageSize\":\"2097152\"}}}"
sleep 5; wait_running
broker "grep -q '^maxMessageSize=2097152' conf/broker.conf" || fail "broker config not applied"
patch "{\"broker\":{\"replicas\":$brokers}}"
sleep 5; wait_running
produce e2e-final 10 && consume e2e-final 10 || fail "produce/consume after broker changes"

echo "PASS"
