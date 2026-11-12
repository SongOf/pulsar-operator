#!/usr/bin/env bash
# Real Kubernetes/CSI smoke test against an EXISTING, ready DiskFleet class.
# Never discovers/enrolls disks, initializes a VG, installs CSI, or deletes a PV.
set -Eeuo pipefail

if [[ $# -ne 1 ]]; then
  printf 'Usage: %s EXISTING_DISKFLEET_STORAGECLASS\n' "$0" >&2
  printf 'Optional: TIMEOUT=300 TEST_IMAGE=busybox:1.37.0\n' >&2
  printf 'PREFLIGHT_ONLY=1 validates identity and scheduling without creating resources.\n' >&2
  exit 2
fi
STORAGE_CLASS=$1
TIMEOUT=${TIMEOUT:-180}
TEST_IMAGE=${TEST_IMAGE:-busybox:1.37.0}
if [[ ! "$TIMEOUT" =~ ^[0-9]+$ ]] || (( TIMEOUT < 30 || TIMEOUT > 1800 )); then
  printf 'TIMEOUT must be an integer from 30 to 1800 seconds.\n' >&2
  exit 2
fi
for tool in kubectl python3 timeout mktemp awk; do
  command -v "$tool" >/dev/null || { printf 'Missing command: %s\n' "$tool" >&2; exit 2; }
done

# Bound every Kubernetes operation, including exec and diagnostic collection.
k() { timeout "$((TIMEOUT + 15))s" kubectl --request-timeout="$((TIMEOUT + 5))s" "$@"; }
diag() { timeout 15s kubectl --request-timeout=10s "$@"; }
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/diskfleet-e2e.XXXXXXXX")
NAMESPACE="diskfleet-e2e-$(date -u +%Y%m%d%H%M%S)-$(python3 -c 'import uuid; print(uuid.uuid4().hex[:8])')"
MARKER="diskfleet-$(python3 -c 'import uuid; print(uuid.uuid4().hex)')"
PV_NAME=''
NAMESPACE_CREATED=false

on_exit() {
  local result=$?
  trap - EXIT
  if (( result != 0 )); then
    printf '\nFAILED: resources are retained for diagnosis; no cleanup was attempted.\n' >&2
    if [[ "$NAMESPACE_CREATED" == true ]]; then
      printf 'Namespace: %s\n' "$NAMESPACE" >&2
      diag get pod,pvc -n "$NAMESPACE" -o wide > "$WORK_DIR/resources.log" 2>&1 || true
      diag describe pod probe -n "$NAMESPACE" > "$WORK_DIR/pod.log" 2>&1 || true
      diag describe pvc probe -n "$NAMESPACE" > "$WORK_DIR/pvc.log" 2>&1 || true
      diag get events -n "$NAMESPACE" --sort-by=.metadata.creationTimestamp > "$WORK_DIR/events.log" 2>&1 || true
      cat "$WORK_DIR/resources.log" "$WORK_DIR/pod.log" "$WORK_DIR/pvc.log" "$WORK_DIR/events.log" >&2
    fi
    if [[ -n "$PV_NAME" ]]; then
      diag get pv "$PV_NAME" -o yaml > "$WORK_DIR/pv.log" 2>&1 || true
      cat "$WORK_DIR/pv.log" >&2
    fi
    printf 'Diagnostic files retained at: %s\n' "$WORK_DIR" >&2
  else
    # This is our own mktemp directory, never a user-supplied cleanup target.
    rm -rf -- "$WORK_DIR"
  fi
  exit "$result"
}
trap on_exit EXIT

printf 'Context: %s\n' "$(k config current-context)"
printf 'Existing StorageClass: %s\n' "$STORAGE_CLASS"
k get storageclass "$STORAGE_CLASS" -o json > "$WORK_DIR/class.json"
python3 - "$WORK_DIR/class.json" <<'PY'
import json, sys
obj = json.load(open(sys.argv[1]))
required = {'provisioner': 'local.csi.openebs.io', 'reclaimPolicy': 'Retain',
            'volumeBindingMode': 'WaitForFirstConsumer', 'allowVolumeExpansion': True}
for key, expected in required.items():
    if obj.get(key) != expected:
        raise SystemExit(f'StorageClass {key} must be {expected!r}; found {obj.get(key)!r}')
if not obj.get('metadata', {}).get('labels', {}).get('storage.diskfleet.io/pool'):
    raise SystemExit('StorageClass is not labeled as a DiskFleet pool')
if obj.get('parameters', {}).get('thinProvision', 'no') != 'no':
    raise SystemExit('This test requires thick provisioning (thinProvision=no)')
print('Validated CSI driver, Retain, WFFC, expansion, and DiskFleet ownership label.')
PY
# Label values can be a hash for long names. Resolve the immutable UID instead.
k get diskpools -o json > "$WORK_DIR/pools.json"
python3 - "$WORK_DIR" <<'PY'
import json, sys
from pathlib import Path
root = Path(sys.argv[1])
sc = json.loads((root / 'class.json').read_text())
uid = sc['metadata'].get('annotations', {}).get('storage.diskfleet.io/pool-uid')
matches = [p for p in json.loads((root / 'pools.json').read_text())['items']
           if uid and p['metadata'].get('uid') == uid]
if len(matches) != 1:
    raise SystemExit('StorageClass must identify exactly one existing DiskPool by UID')
(root / 'pool.json').write_text(json.dumps(matches[0]))
PY
python3 - "$WORK_DIR/class.json" "$WORK_DIR/pool.json" <<'PY'
import json, sys
sc, pool = (json.load(open(p)) for p in sys.argv[1:])
if pool.get('status', {}).get('phase') != 'Ready':
    raise SystemExit('DiskPool is not Ready')
if pool['metadata'].get('deletionTimestamp') or not pool['spec'].get('approved'):
    raise SystemExit('DiskPool must remain approved and must not be deleting')
if pool.get('status', {}).get('storageClass') != sc['metadata']['name']:
    raise SystemExit('DiskPool does not reference the selected StorageClass')
if sc['metadata'].get('annotations', {}).get('storage.diskfleet.io/pool-uid') != pool['metadata']['uid']:
    raise SystemExit('StorageClass ownership UID does not match the actual DiskPool')
if int(pool.get('status', {}).get('availableBytes', 0)) < 3 * 1024**3:
    raise SystemExit('DiskPool reports less than 3 GiB free; this test needs a retained 3 GiB allocation')
if sc.get('parameters', {}).get('vgpattern') != '^' + pool['spec']['vgName'] + '$':
    raise SystemExit('StorageClass VG pattern does not match the approved single pool')
PY
NODE_NAME=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["nodeName"])' "$WORK_DIR/pool.json")
k get node "$NODE_NAME" -o json > "$WORK_DIR/node.json"
python3 - "$WORK_DIR" <<'PY'
import json, sys
from pathlib import Path
root = Path(sys.argv[1])
node, pool, sc = (json.loads((root / f'{name}.json').read_text()) for name in ('node', 'pool', 'class'))
meta, spec = node['metadata'], node.get('spec', {})
hostname = meta.get('labels', {}).get('kubernetes.io/hostname')
if (meta['name'] != pool['spec']['nodeName'] or meta.get('deletionTimestamp') or spec.get('unschedulable')
        or not any(c['type'] == 'Ready' and c['status'] == 'True' for c in node.get('status', {}).get('conditions', []))):
    raise SystemExit('Pool node must be Ready, schedulable and not deleting')
if not hostname or hostname != pool['status'].get('topologyHostname'):
    raise SystemExit('Pool topology must match the live node hostname')
if sc.get('allowedTopologies') != [{'matchLabelExpressions': [{'key': 'kubernetes.io/hostname', 'values': [hostname]}]}]:
    raise SystemExit('StorageClass must select only the verified pool hostname')
allowed = {'node-role.kubernetes.io/control-plane', 'node-role.kubernetes.io/master'}
tolerations = []
for taint in spec.get('taints', []):
    if taint.get('effect') == 'NoExecute' or (taint.get('effect') == 'NoSchedule' and taint['key'] not in allowed):
        raise SystemExit('Pool node has an unsupported scheduling taint; no resources created')
    if taint.get('effect') == 'NoSchedule':
        tolerations.append({'key': taint['key'], 'operator': 'Exists', 'effect': 'NoSchedule'})
(root / 'scheduling.json').write_text(json.dumps({'nodeSelector': {'kubernetes.io/hostname': hostname}, 'tolerations': tolerations}))
print('Validated live node topology and probe scheduling before allocating storage.')
PY

if [[ "${PREFLIGHT_ONLY:-0}" == 1 ]]; then
  printf 'PASS: pool UID, live node and scheduling validated; no resources created.\n'
  cat "$WORK_DIR/scheduling.json"
  printf '\n'
  exit 0
fi

printf 'Creating isolated namespace %s; this test reserves a retained 3 GiB volume.\n' "$NAMESPACE"
k create namespace "$NAMESPACE"
NAMESPACE_CREATED=true
python3 - "$STORAGE_CLASS" "$NAMESPACE" <<'PY' | k apply -f -
import json, sys
print(json.dumps({'apiVersion': 'v1', 'kind': 'PersistentVolumeClaim',
 'metadata': {'name': 'probe', 'namespace': sys.argv[2], 'labels': {'app': 'diskfleet-e2e'}},
 'spec': {'storageClassName': sys.argv[1], 'accessModes': ['ReadWriteOnce'],
          'volumeMode': 'Filesystem', 'resources': {'requests': {'storage': '2Gi'}}}}))
PY

create_pod() {
  python3 - "$TEST_IMAGE" "$NAMESPACE" "$WORK_DIR/scheduling.json" <<'PY' | k apply -f -
import json, sys
print(json.dumps({'apiVersion': 'v1', 'kind': 'Pod',
 'metadata': {'name': 'probe', 'namespace': sys.argv[2], 'labels': {'app': 'diskfleet-e2e'}},
 'spec': {**json.load(open(sys.argv[3])), 'terminationGracePeriodSeconds': 5,
          'containers': [{'name': 'probe', 'image': sys.argv[1],
              'command': ['sh', '-c', 'while :; do sleep 3600; done'],
              'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'},
                            'limits': {'cpu': '100m', 'memory': '64Mi'}},
              'volumeMounts': [{'name': 'data', 'mountPath': '/data'}]}],
          'volumes': [{'name': 'data', 'persistentVolumeClaim': {'claimName': 'probe'}}]}}))
PY
  k wait pod/probe -n "$NAMESPACE" --for=condition=Ready "--timeout=${TIMEOUT}s"
}

create_pod
k wait pvc/probe -n "$NAMESPACE" '--for=jsonpath={.status.phase}=Bound' "--timeout=${TIMEOUT}s"
k get pvc probe -n "$NAMESPACE" -o json > "$WORK_DIR/claim.json"
PV_NAME=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["volumeName"])' "$WORK_DIR/claim.json")
k get pv "$PV_NAME" -o json > "$WORK_DIR/pv.json"
python3 - "$WORK_DIR/pv.json" "$WORK_DIR/claim.json" <<'PY'
import json, sys
pv, pvc = (json.load(open(p)) for p in sys.argv[1:])
if pv['spec'].get('persistentVolumeReclaimPolicy') != 'Retain':
    raise SystemExit('Actual PV is not Retain; stopping before deleting any PVC')
if pv['spec'].get('csi', {}).get('driver') != 'local.csi.openebs.io':
    raise SystemExit('Actual PV was not provisioned by the expected LVM CSI driver')
if pv['spec'].get('claimRef', {}).get('uid') != pvc['metadata']['uid']:
    raise SystemExit('Actual PV claim identity does not match this test')
PY

printf 'Writing and syncing a marker on the actual CSI filesystem.\n'
k exec -n "$NAMESPACE" probe -- sh -c 'printf "%s\n" "$1" > /data/diskfleet-probe; sync; test "$(cat /data/diskfleet-probe)" = "$1"' sh "$MARKER"
k exec -n "$NAMESPACE" probe -- df -Pk /data > "$WORK_DIR/df-before"
BEFORE_KIB=$(awk 'NR==2 {print $2}' "$WORK_DIR/df-before")
[[ "$BEFORE_KIB" =~ ^[0-9]+$ ]] || { printf 'Cannot parse original filesystem size.\n' >&2; exit 1; }

printf 'Recreating the Pod and checking that the same marker persists.\n'
k delete pod probe -n "$NAMESPACE" --wait=true "--timeout=${TIMEOUT}s"
create_pod
k exec -n "$NAMESPACE" probe -- sh -c 'test "$(cat /data/diskfleet-probe)" = "$1"' sh "$MARKER"

printf 'Expanding PVC 2Gi -> 3Gi and remounting to verify filesystem expansion.\n'
k patch pvc probe -n "$NAMESPACE" --type=merge -p '{"spec":{"resources":{"requests":{"storage":"3Gi"}}}}'
# A remount covers both online and mount-time filesystem expansion behavior.
k delete pod probe -n "$NAMESPACE" --wait=true "--timeout=${TIMEOUT}s"
create_pod
k wait pvc/probe -n "$NAMESPACE" '--for=jsonpath={.status.capacity.storage}=3Gi' "--timeout=${TIMEOUT}s"
k exec -n "$NAMESPACE" probe -- sh -c 'test "$(cat /data/diskfleet-probe)" = "$1"' sh "$MARKER"
k exec -n "$NAMESPACE" probe -- df -Pk /data > "$WORK_DIR/df-after"
AFTER_KIB=$(awk 'NR==2 {print $2}' "$WORK_DIR/df-after")
[[ "$AFTER_KIB" =~ ^[0-9]+$ ]] || { printf 'Cannot parse expanded filesystem size.\n' >&2; exit 1; }
if (( AFTER_KIB <= BEFORE_KIB || AFTER_KIB < 2621440 )); then
  printf 'Filesystem did not grow as expected: before=%s KiB after=%s KiB\n' "$BEFORE_KIB" "$AFTER_KIB" >&2
  exit 1
fi

printf 'Deleting only this test Pod/PVC; verifying that the backing PV is retained.\n'
k delete pod probe -n "$NAMESPACE" --wait=true "--timeout=${TIMEOUT}s"
# Recheck before deletion in case an administrator changed the actual PV policy.
[[ "$(k get pv "$PV_NAME" -o 'jsonpath={.spec.persistentVolumeReclaimPolicy}')" == Retain ]] || {
  printf 'PV policy changed; stopping without deleting the PVC.\n' >&2; exit 1;
}
k delete pvc probe -n "$NAMESPACE" --wait=true "--timeout=${TIMEOUT}s"
k wait "pv/$PV_NAME" '--for=jsonpath={.status.phase}=Released' "--timeout=${TIMEOUT}s"
k get pv "$PV_NAME" -o json > "$WORK_DIR/retained.json"
python3 - "$WORK_DIR/retained.json" "$WORK_DIR/claim.json" <<'PY'
import json, sys
pv, pvc = (json.load(open(p)) for p in sys.argv[1:])
if pv.get('status', {}).get('phase') != 'Released' or pv['spec'].get('persistentVolumeReclaimPolicy') != 'Retain':
    raise SystemExit('PV is not Released + Retain')
if pv['spec'].get('claimRef', {}).get('uid') != pvc['metadata']['uid']:
    raise SystemExit('Retained PV claim identity changed')
if pv['metadata'].get('deletionTimestamp'):
    raise SystemExit('Retained PV is unexpectedly being deleted')
PY

printf '\nPASS: binding, write/read, Pod recreation, filesystem expansion, and Retain lifecycle.\n'
printf 'Filesystem: %s KiB -> %s KiB\n' "$BEFORE_KIB" "$AFTER_KIB"
printf 'Retained PV: %s (3 GiB remains allocated; data was not destroyed).\n' "$PV_NAME"
printf 'Retained namespace: %s (test Pod and PVC have been removed).\n' "$NAMESPACE"
printf 'Inspect: kubectl get pv %s -o yaml\n' "$PV_NAME"
printf 'Empty namespace cleanup: kubectl delete namespace %s\n' "$NAMESPACE"
printf 'For PV/LV cleanup or recovery, follow docs/physical-disks.md after reviewing data ownership.\n'
printf 'This script intentionally does not remove PVs, LVs, VGs, signatures, or ownership journals.\n'
