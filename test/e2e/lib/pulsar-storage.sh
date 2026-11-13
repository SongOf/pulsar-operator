#!/usr/bin/env bash
# SongOf/pulsar-operator + DiskFleet real-cluster acceptance test.
# Uses the CRD/resource contract inspected at upstream revision 3c042c5.
# Requires an existing, dedicated and Running test PulsarCluster. Never enrolls disks.
set -Eeuo pipefail

PROFILE=three-node-e2e
if [[ ${1:-} == --profile ]]; then
  [[ $# -ge 2 ]] || { printf 'Missing --profile value.\n' >&2; exit 2; }
  PROFILE=$2
  shift 2
fi
if [[ "$PROFILE" != three-node-e2e && "$PROFILE" != two-node-shared-disk-lab ]]; then
  printf 'Unsupported E2E profile: %s\n' "$PROFILE" >&2; exit 2
fi
if [[ $# -ne 2 ]]; then
  printf 'Usage: %s [--profile three-node-e2e|two-node-shared-disk-lab] TEST_NAMESPACE PULSAR_CLUSTER_NAME\n' "$0" >&2
  printf 'Requires namespace AND PulsarCluster label storage.diskfleet.io/integration=pulsar.\n' >&2
  printf 'The shared-disk profile requires its own integration/profile labels; it does not certify host failure HA.\n' >&2
  printf 'Changes: recreate bookie-0, grow every ledger by 1Gi, create/delete a 1Gi probe PVC and retain its PV.\n' >&2
  exit 2
fi
NS=$1
NAME=$2
TIMEOUT=${TIMEOUT:-600}
TEST_IMAGE=${TEST_IMAGE:-busybox:1.37.0}
PREFLIGHT_ONLY=${PREFLIGHT_ONLY:-0}
[[ "$PREFLIGHT_ONLY" == 0 || "$PREFLIGHT_ONLY" == 1 ]] || { printf 'PREFLIGHT_ONLY must be 0 or 1.\n' >&2; exit 2; }
if [[ ! "$NS" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ || ! "$NAME" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#NS} > 63 || ${#NAME} > 40 )); then
  printf 'Use a valid namespace and a DNS-label cluster name of at most 40 characters.\n' >&2; exit 2
fi
[[ "$NS" == diskfleet-pulsar-* ]] || { printf 'Test namespace must start with diskfleet-pulsar-.\n' >&2; exit 2; }
if [[ ! "$TIMEOUT" =~ ^[0-9]+$ ]] || (( TIMEOUT < 60 || TIMEOUT > 1800 )); then
  printf 'TIMEOUT must be 60..1800 seconds.\n' >&2; exit 2
fi
for tool in kubectl python3 timeout mktemp awk; do
  command -v "$tool" >/dev/null || { printf 'Missing command: %s\n' "$tool" >&2; exit 2; }
done
k() { timeout "$((TIMEOUT + 15))s" kubectl --request-timeout="$((TIMEOUT + 5))s" "$@"; }
diag() { timeout 15s kubectl --request-timeout=10s "$@"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/diskfleet-pulsar-e2e.XXXXXXXX")
RUN_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')
PROBE="diskfleet-retain-$RUN_ID"
TOPIC="persistent://public/default/diskfleet-$RUN_ID"
SUB="diskfleet-$RUN_ID"
MARKER="diskfleet-payload-$RUN_ID"
PROBE_PV=''
PREFLIGHT_OK=false

on_exit() {
  local result=$?
  trap - EXIT
  if (( result != 0 )); then
    printf '\nFAILED: no automatic rollback or storage cleanup is performed.\n' >&2
    if [[ "$PREFLIGHT_OK" == true ]]; then
      diag get psc,bkc,pod,pvc -n "$NS" -o wide > "$WORK_DIR/diagnostics.log" 2>&1 || true
      diag get events -n "$NS" --sort-by=.metadata.creationTimestamp > "$WORK_DIR/events.log" 2>&1 || true
      cat "$WORK_DIR/diagnostics.log" "$WORK_DIR/events.log" >&2
    fi
    printf 'Evidence and diagnostics retained at: %s\n' "$WORK_DIR" >&2
  else
    printf '\nEvidence retained at: %s\n' "$WORK_DIR"
  fi
  exit "$result"
}
trap on_exit EXIT

CONTEXT=$(k config current-context)
printf 'Context: %s; E2E profile: %s\n' "$CONTEXT" "$PROFILE"
k get namespace "$NS" -o json > "$WORK_DIR/namespace.json"
k get pulsarclusters.pulsar.songof.dev "$NAME" -n "$NS" -o json > "$WORK_DIR/pulsar.json"
python3 - "$WORK_DIR" "$NS" "$NAME" "$PROFILE" <<'PY'
import json,re,sys
from pathlib import Path
root,ns,name=Path(sys.argv[1]),sys.argv[2],sys.argv[3]
profile=sys.argv[4]; shared=profile=='two-node-shared-disk-lab'
integration='pulsar-two-node-shared-disk-lab' if shared else 'pulsar'
namespace=json.loads((root/'namespace.json').read_text())
cr=json.loads((root/'pulsar.json').read_text())
for obj in (namespace,cr):
    labels=obj['metadata'].get('labels',{})
    if labels.get('storage.diskfleet.io/integration')!=integration or (shared and labels.get('storage.diskfleet.io/profile')!=profile):
        raise SystemExit('Refusing to mutate resources outside a labeled dedicated Pulsar integration namespace/CR')
if cr.get('apiVersion')!='pulsar.songof.dev/v1alpha1' or cr.get('kind')!='PulsarCluster':
    raise SystemExit('Expected SongOf/pulsar-operator PulsarCluster')
spec,status=cr['spec'],cr.get('status',{})
if status.get('phase')!='Running' or status.get('observedGeneration')!=cr['metadata']['generation']:
    raise SystemExit('PulsarCluster must be Running at its current generation before testing')
if spec.get('paused') or spec['bookkeeper'].get('storage',{}).get('reclaimPolicy','Retain')!='Retain':
    raise SystemExit('Require active reconciliation and BookKeeper storage.reclaimPolicy=Retain')
z,b=spec['zookeeper'],spec['bookkeeper']
zr,br=z.get('replicas',3),b['replicas']
if shared:
    if zr!=3 or br!=3 or b.get('minReplicas')!=3 or spec['broker'].get('replicas')!=2:
        raise SystemExit('Shared-disk lab requires exactly 3 ZooKeeper, 3 bookies, minReplicas=3 and 2 brokers')
    for component in (spec.get('proxy',{}),b.get('autoRecovery',{})):
        if component.get('enabled') is not True or component.get('replicas')!=1:
            raise SystemExit('Shared-disk lab requires one enabled proxy and autorecovery replica')
    for key in ('managedLedgerDefaultEnsembleSize','managedLedgerDefaultWriteQuorum','managedLedgerDefaultAckQuorum'):
        if str(spec['broker'].get('config',{}).get(key))!='2':
            raise SystemExit('Shared-disk lab requires ensemble/write/ack quorum 2/2/2')
    for component in (z,b,spec['broker'],spec['proxy'],b['autoRecovery']):
        pod=component.get('pod',{})
        if pod.get('antiAffinity')!='Preferred' or pod.get('nodeSelector',{}).get('pulsar.songof.dev/two-node')!='true':
            raise SystemExit('Shared-disk lab requires Preferred anti-affinity and the two-node selector for every component')
        for key in ('node-role.kubernetes.io/control-plane','node-role.kubernetes.io/master'):
            if not any(t.get('key')==key and t.get('operator')=='Exists' and t.get('effect')=='NoSchedule' for t in pod.get('tolerations',[])):
                raise SystemExit('Shared-disk lab requires explicit control-plane/master NoSchedule tolerations')
else:
    if zr<3 or br<3 or b.get('pod',{}).get('antiAffinity')!='Required':
        raise SystemExit('Require at least 3 ZooKeeper/bookie replicas and bookkeeper.pod.antiAffinity=Required')
    if z.get('pod',{}).get('antiAffinity')!='Required':
        raise SystemExit('Require zookeeper.pod.antiAffinity=Required')
classes={'zookeeper':z['storage'].get('storageClassName'),'journal':b['storage']['journal'].get('storageClassName'),'ledgers':b['storage']['ledger'].get('storageClassName')}
if any(not isinstance(v,str) or not re.fullmatch(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?',v) for v in classes.values()):
    raise SystemExit('All three storage roles must explicitly reference a named StorageClass')
old=b['storage']['ledger']['size']
if not re.fullmatch(r'[1-9][0-9]*Gi',old):
    raise SystemExit('Integration profile ledger size must be an integer Gi quantity')
new=str(int(old[:-2])+1)+'Gi'
required=[f'data-{name}-zookeeper-{i}' for i in range(zr)]
required += [f'{role}-{name}-bookie-{i}' for i in range(br) for role in ('journal','ledgers')]
(root/'required-pvcs.txt').write_text('\n'.join(required)+'\n')
(root/'classes.txt').write_text('\n'.join(sorted(set(classes.values())))+'\n')
(root/'facts.json').write_text(json.dumps(dict(namespace=ns,name=name,uid=cr['metadata']['uid'],profile=profile,integration=integration,shared=shared,bookies=br,zookeepers=zr,classes=classes,oldSize=old,newSize=new)))
patch=[{'op':'test','path':'/metadata/uid','value':cr['metadata']['uid']},
       {'op':'test','path':'/spec/bookkeeper/storage/ledger/size','value':old},
       {'op':'replace','path':'/spec/bookkeeper/storage/ledger/size','value':new}]
(root/'resize-patch.json').write_text(json.dumps(patch))
print(f'Preflight: {zr} ZooKeeper + {br} bookie replicas; {len(required)} PVCs; ledger expansion {old} -> {new}')
PY
PREFLIGHT_OK=true

mkdir "$WORK_DIR/classes" "$WORK_DIR/pvs" "$WORK_DIR/nodes" "$WORK_DIR/pools"
if [[ "$PROFILE" == two-node-shared-disk-lab ]]; then
  k get nodes -l pulsar.songof.dev/two-node=true -o json > "$WORK_DIR/target-nodes.json"
fi
while IFS= read -r class; do
  k get storageclass "$class" -o json > "$WORK_DIR/classes/$class.json"
done < "$WORK_DIR/classes.txt"
python3 - "$WORK_DIR" <<'PY' > "$WORK_DIR/pool-names.txt"
import json,re,sys
from pathlib import Path
root=Path(sys.argv[1]); names=set()
for path in (root/'classes').glob('*.json'):
    refs=json.loads(json.loads(path.read_text())['metadata'].get('annotations',{}).get('storage.diskfleet.io/pool-uids','{}'))
    if not isinstance(refs,dict) or not refs:
        raise SystemExit('Fleet StorageClass is missing its pool ownership map')
    for name,uid in refs.items():
        if not re.fullmatch(r'[a-z0-9]([-a-z0-9]*[a-z0-9])?',name) or not isinstance(uid,str) or not uid:
            raise SystemExit('Invalid pool name/UID in fleet StorageClass')
        names.add(name)
print('\n'.join(sorted(names)))
PY
while IFS= read -r pool; do k get diskpool "$pool" -o json > "$WORK_DIR/pools/$pool.json"; done < "$WORK_DIR/pool-names.txt"
python3 - "$WORK_DIR" <<'PY'
import json,sys
from pathlib import Path
root=Path(sys.argv[1]); facts=json.loads((root/'facts.json').read_text())
targets={}
if facts['shared']:
    items=json.loads((root/'target-nodes.json').read_text())['items']
    for node in items:
        meta=node['metadata']; labels=meta.get('labels',{}); hostname=labels.get('kubernetes.io/hostname')
        if labels.get('pulsar.songof.dev/two-node')!='true' or not hostname or meta.get('deletionTimestamp') or node.get('spec',{}).get('unschedulable') or not any(c.get('type')=='Ready' and c.get('status')=='True' for c in node.get('status',{}).get('conditions',[])):
            raise SystemExit('Shared-disk targets must be labeled, Ready, schedulable nodes with verified hostnames')
        targets[meta['name']]=hostname
    if len(items)!=2 or len(targets)!=2 or len(set(targets.values()))!=2:
        raise SystemExit('Shared-disk lab requires exactly two uniquely named and labeled Ready nodes')
    facts['targetNodes']=targets
    (root/'facts.json').write_text(json.dumps(facts))
hosts={}; pools={}
for role,name in facts['classes'].items():
    sc=json.loads((root/'classes'/f'{name}.json').read_text())
    expected={'provisioner':'local.csi.openebs.io','reclaimPolicy':'Retain','volumeBindingMode':'WaitForFirstConsumer','allowVolumeExpansion':True}
    if any(sc.get(k)!=v for k,v in expected.items()) or sc.get('parameters',{}).get('thinProvision')!='no':
        raise SystemExit(f'{name}: require OpenEBS LVM, Retain, WFFC, expansion, and thick provisioning')
    if sc['metadata'].get('labels',{}).get('storage.diskfleet.io/fleet')!='true':
        raise SystemExit(f'{name}: require a generated multi-node DiskFleet fleet StorageClass')
    if facts['shared'] and any(sc['metadata'].get('labels',{}).get('storage.diskfleet.io/'+key)!=facts[key] for key in ('integration','profile')):
        raise SystemExit(f'{name}: shared-disk StorageClass integration/profile label mismatch')
    refs=json.loads(sc['metadata'].get('annotations',{}).get('storage.diskfleet.io/pool-uids','{}'))
    pools[role]=set(refs)
    if not pools[role]: raise SystemExit(f'{name}: missing pool ownership map')
    vgs=set(); actual_hosts=set(); actual_nodes=set()
    for pool_name,uid in refs.items():
        pool=json.loads((root/'pools'/f'{pool_name}.json').read_text())
        if pool['metadata']['uid']!=uid or pool.get('status',{}).get('phase')!='Ready':
            raise SystemExit(f'{name}: referenced DiskPool is not Ready or has a different UID')
        vgs.add(pool['spec']['vgName'])
        hostname=pool['status'].get('topologyHostname')
        if not hostname: raise SystemExit(f'{pool_name}: missing verified topology hostname')
        node_name=pool['spec'].get('nodeName')
        if facts['shared'] and (node_name not in targets or targets[node_name]!=hostname or node_name in actual_nodes):
            raise SystemExit(f'{pool_name}: require one pool per selected live node with matching hostname')
        actual_nodes.add(node_name)
        actual_hosts.add(hostname)
    if facts['shared'] and (len(refs)!=2 or actual_nodes!=set(targets)):
        raise SystemExit(f'{name}: shared-disk roles must cover both selected nodes with exactly two pools')
    if sc['parameters'].get('vgpattern')!='^('+'|'.join(sorted(vgs))+')$':
        raise SystemExit(f'{name}: VG pattern differs from the exact selected pools')
    if sc.get('allowedTopologies')!=[{'matchLabelExpressions':[{'key':'kubernetes.io/hostname','values':sorted(actual_hosts)}]}]:
        raise SystemExit(f'{name}: allowedTopologies differs from the exact selected pools')
    hosts[role]={h for term in sc.get('allowedTopologies',[]) for expr in term.get('matchLabelExpressions',[]) if expr.get('key')=='kubernetes.io/hostname' for h in expr.get('values',[])}
if facts['shared']:
    if not pools['zookeeper']==pools['journal']==pools['ledgers']:
        raise SystemExit('Shared-disk lab requires all three roles to reference the same two DiskPools')
else:
    if pools['journal'] & pools['ledgers']:
        raise SystemExit('Journal and ledger classes must reference disjoint physical DiskPools')
    if len(hosts['journal'] & hosts['ledgers']) < facts['bookies'] or len(hosts['zookeeper']) < facts['zookeepers']:
        raise SystemExit('StorageClass topology cannot satisfy Required anti-affinity and co-located journal/ledger mounts')
PY

# Wait for each declared workload PVC; monitoring PVCs are deliberately outside this profile.
while IFS= read -r claim; do
  k wait -n "$NS" "pvc/$claim" '--for=jsonpath={.status.phase}=Bound' "--timeout=${TIMEOUT}s"
done < "$WORK_DIR/required-pvcs.txt"
k get pvc -n "$NS" -o json > "$WORK_DIR/pvcs-before.json"
k get pods -n "$NS" -o json > "$WORK_DIR/pods-before.json"
python3 - "$WORK_DIR" <<'PY' > "$WORK_DIR/pv-names.txt"
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); required=set((r/'required-pvcs.txt').read_text().splitlines())
pvcs={p['metadata']['name']:p for p in json.loads((r/'pvcs-before.json').read_text())['items']}
for name in sorted(required):
    print(pvcs[name]['spec']['volumeName'])
PY
while IFS= read -r pv; do k get pv "$pv" -o json > "$WORK_DIR/pvs/$pv.json"; done < "$WORK_DIR/pv-names.txt"
python3 - "$WORK_DIR" <<'PY' > "$WORK_DIR/node-names.txt"
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); f=json.loads((r/'facts.json').read_text())
pods=json.loads((r/'pods-before.json').read_text())['items']
print('\n'.join(sorted({p['spec']['nodeName'] for p in pods if p['metadata'].get('labels',{}).get('app.kubernetes.io/instance')==f['name']})))
PY
while IFS= read -r node; do k get node "$node" -o json > "$WORK_DIR/nodes/$node.json"; done < "$WORK_DIR/node-names.txt"
python3 - "$WORK_DIR" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); f=json.loads((r/'facts.json').read_text()); name=f['name']
pvcs={p['metadata']['name']:p for p in json.loads((r/'pvcs-before.json').read_text())['items']}
pods={p['metadata']['name']:p for p in json.loads((r/'pods-before.json').read_text())['items']}
nodes={p.stem:json.loads(p.read_text()) for p in (r/'nodes').glob('*.json')}
def matches(expr,values):
    key,op=expr['key'],expr['operator']; options=expr.get('values',[])
    if op=='In': return key in values and values[key] in options
    if op=='NotIn': return key not in values or values[key] not in options
    if op=='Exists': return key in values
    if op=='DoesNotExist': return key not in values
    if op in ('Gt','Lt'):
        try: return int(values[key])>int(options[0]) if op=='Gt' else int(values[key])<int(options[0])
        except (ValueError,KeyError,IndexError): return False
    raise SystemExit(f'Unsupported node affinity operator: {op}')
for claim_name in (r/'required-pvcs.txt').read_text().splitlines():
    pvc=pvcs[claim_name]; labels=pvc['metadata'].get('labels',{})
    if labels.get('app.kubernetes.io/instance')!=name or labels.get('app.kubernetes.io/managed-by')!='pulsar-operator':
        raise SystemExit(f'{claim_name}: not generated by the selected Pulsar Operator cluster')
    role=claim_name.split('-',1)[0]
    expected_class=f['classes']['zookeeper' if role=='data' else role]
    if pvc['spec'].get('storageClassName')!=expected_class or pvc['spec'].get('accessModes')!=['ReadWriteOnce'] or pvc['spec'].get('volumeMode','Filesystem')!='Filesystem':
        raise SystemExit(f'{claim_name}: PVC does not use the declared role StorageClass and RWO filesystem mode')
    pv=json.loads((r/'pvs'/(pvc['spec']['volumeName']+'.json')).read_text())
    if pv['spec'].get('csi',{}).get('driver')!='local.csi.openebs.io' or pv['spec'].get('persistentVolumeReclaimPolicy')!='Retain':
        raise SystemExit(f'{claim_name}: actual PV must be LVM CSI and Retain')
    if pv['spec'].get('claimRef',{}).get('uid')!=pvc['metadata']['uid']:
        raise SystemExit(f'{claim_name}: PV/PVC UID mismatch')
    pod_name=claim_name.split('-',1)[1]
    pod=pods[pod_name]; node=nodes[pod['spec']['nodeName']]
    if not any(v.get('persistentVolumeClaim',{}).get('claimName')==claim_name for v in pod['spec']['volumes']):
        raise SystemExit(f'{claim_name}: not mounted by expected Pod')
    if not any(c.get('type')=='Ready' and c.get('status')=='True' for c in pod.get('status',{}).get('conditions',[])):
        raise SystemExit(f'{pod_name}: Pod is not Ready')
    terms=pv['spec'].get('nodeAffinity',{}).get('required',{}).get('nodeSelectorTerms',[])
    if not terms or not any((t.get('matchExpressions') or t.get('matchFields')) and all(matches(x,node['metadata'].get('labels',{})) for x in t.get('matchExpressions',[])) and all(matches(x,{'metadata.name':node['metadata']['name']}) for x in t.get('matchFields',[])) for t in terms):
        raise SystemExit(f'{claim_name}: PV node affinity does not admit the scheduled node')
if f['shared']:
    for pod in pods.values():
        if pod['metadata'].get('labels',{}).get('app.kubernetes.io/instance')!=name: continue
        node_name=pod['spec'].get('nodeName'); node=nodes.get(node_name,{})
        labels=node.get('metadata',{}).get('labels',{})
        if node_name not in f['targetNodes'] or labels.get('pulsar.songof.dev/two-node')!='true' or labels.get('kubernetes.io/hostname')!=f['targetNodes'][node_name] or not any(c.get('type')=='Ready' and c.get('status')=='True' for c in node.get('status',{}).get('conditions',[])):
            raise SystemExit('Shared-disk workload is outside the two verified Ready nodes')
    print('PASS: nine operator-created PVCs, LVM CSI PVs, mounts and two-node shared-disk topology; Preferred permits co-location')
else:
    for component,count in [('bookie',f['bookies']),('zookeeper',f['zookeepers'])]:
        if len({pods[f'{name}-{component}-{i}']['spec']['nodeName'] for i in range(count)})!=count:
            raise SystemExit(f'{component}: Required anti-affinity did not spread replicas across nodes')
    print('PASS: actual operator-created PVCs, LVM CSI PVs, mounts, topology, and replica node separation')
PY

# Prepare the independent probe without changing the cluster; retain it as reviewable evidence.
LEDGER_CLASS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["classes"]["ledgers"])' "$WORK_DIR/facts.json")
python3 - "$NS" "$PROBE" "$RUN_ID" "$LEDGER_CLASS" "$TEST_IMAGE" "$WORK_DIR/facts.json" <<'PY' > "$WORK_DIR/probe-manifest.json"
import json,sys
ns,name,run,sc,image,facts_path=sys.argv[1:]
facts=json.load(open(facts_path))
meta={'name':name,'namespace':ns,'labels':{'storage.diskfleet.io/integration':facts['integration'],'storage.diskfleet.io/probe-run':run}}
pvc={'apiVersion':'v1','kind':'PersistentVolumeClaim','metadata':meta,'spec':{'storageClassName':sc,'accessModes':['ReadWriteOnce'],'resources':{'requests':{'storage':'1Gi'}}}}
pod={'apiVersion':'v1','kind':'Pod','metadata':meta,'spec':{'terminationGracePeriodSeconds':5,'containers':[{'name':'probe','image':image,'command':['sh','-c','while :; do sleep 3600; done'],'resources':{'requests':{'cpu':'10m','memory':'16Mi'},'limits':{'cpu':'100m','memory':'64Mi'}},'volumeMounts':[{'name':'data','mountPath':'/data'}]}],'volumes':[{'name':'data','persistentVolumeClaim':{'claimName':name}}]}}
if facts['shared']:
    meta['labels']['storage.diskfleet.io/profile']=facts['profile']
    pod['spec']['nodeSelector']={'pulsar.songof.dev/two-node':'true'}
    pod['spec']['tolerations']=[{'key':key,'operator':'Exists','effect':'NoSchedule'} for key in ('node-role.kubernetes.io/control-plane','node-role.kubernetes.io/master')]
print(json.dumps({'apiVersion':'v1','kind':'List','items':[pvc,pod]}))
PY
if [[ "$PREFLIGHT_ONLY" == 1 ]]; then
  printf '\nPASS: read-only preflight for %s. No persistence, expansion, or Retain lifecycle test was executed.\n' "$PROFILE"
  exit 0
fi

BROKER="$NAME-broker-0"
BOOKIE="$NAME-bookie-0"
k wait -n "$NS" "pod/$BROKER" --for=condition=Ready "--timeout=${TIMEOUT}s"
printf 'Writing unique markers on bookie-0 journal and ledger filesystems with the image user.\n'
k exec -n "$NS" "$BOOKIE" -- sh -c 'set -eu; set -C; umask 077; for path in /pulsar/data/bookkeeper/journal /pulsar/data/bookkeeper/ledgers; do printf "%s\n" "$1" > "$path/.diskfleet-e2e-$2"; done; sync; id' sh "$MARKER" "$RUN_ID" > "$WORK_DIR/bookie-user-before.txt"
printf 'Producing retained Pulsar messages through the actual broker.\n'
k exec -n "$NS" "$BROKER" -- bin/pulsar-admin topics create-subscription "$TOPIC" -s "$SUB" -m earliest
k exec -n "$NS" "$BROKER" -- bin/pulsar-client --url pulsar://localhost:6650 produce "$TOPIC" -m "$MARKER" -n 20 > "$WORK_DIR/produce.log" 2>&1
python3 - "$WORK_DIR/produce.log" <<'PY'
import pathlib,sys
if '20 messages successfully produced' not in pathlib.Path(sys.argv[1]).read_text():
    raise SystemExit('Pulsar client did not report 20 successfully produced messages')
PY
OLD_POD_UID=$(k get pod "$BOOKIE" -n "$NS" -o 'jsonpath={.metadata.uid}')
k delete pod "$BOOKIE" -n "$NS" --wait=true "--timeout=${TIMEOUT}s"
deadline=$((SECONDS+TIMEOUT))
while (( SECONDS < deadline )); do
  new_uid=$(k get pod "$BOOKIE" -n "$NS" --ignore-not-found -o 'jsonpath={.metadata.uid}')
  if [[ -n "$new_uid" && "$new_uid" != "$OLD_POD_UID" ]]; then break; fi
  sleep 3
done
[[ -n "${new_uid:-}" && "$new_uid" != "$OLD_POD_UID" ]] || fail 'bookie Pod was not recreated with a new UID'
k wait -n "$NS" "pod/$BOOKIE" --for=condition=Ready "--timeout=${TIMEOUT}s"
k exec -n "$NS" "$BOOKIE" -- sh -c 'set -eu; for path in /pulsar/data/bookkeeper/journal /pulsar/data/bookkeeper/ledgers; do test "$(cat "$path/.diskfleet-e2e-$2")" = "$1"; done; id' sh "$MARKER" "$RUN_ID" > "$WORK_DIR/bookie-user-after.txt"
printf 'PASS: journal/ledger marker files survived bookie-0 recreation and are readable by the image user.\n'
k exec -n "$NS" "$BROKER" -- bin/pulsar-client --url pulsar://localhost:6650 consume "$TOPIC" -s "$SUB" -n 20 > "$WORK_DIR/consume.log" 2>&1
python3 - "$WORK_DIR/consume.log" "$MARKER" <<'PY'
import pathlib,sys
output=pathlib.Path(sys.argv[1]).read_text()
if '20 messages successfully consumed' not in output or sys.argv[2] not in output:
    raise SystemExit('Pulsar did not return the expected retained messages after bookie recreation')
PY
printf 'PASS: 20 messages survived a bookie Pod recreation.\n'

printf 'Expanding ledger storage through PulsarCluster, not by directly modifying its PVCs.\n'
k exec -n "$NS" "$BOOKIE" -- df -Pk /pulsar/data/bookkeeper/ledgers > "$WORK_DIR/df-before.txt"
BEFORE_KIB=$(awk 'NR==2 {print $2}' "$WORK_DIR/df-before.txt")
[[ "$BEFORE_KIB" =~ ^[0-9]+$ ]] || fail 'Unable to parse original ledger filesystem size'
k patch pulsarclusters.pulsar.songof.dev "$NAME" -n "$NS" --type=json --patch-file "$WORK_DIR/resize-patch.json"
NEW_SIZE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["newSize"])' "$WORK_DIR/facts.json")
BOOKIES=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["bookies"])' "$WORK_DIR/facts.json")
for ((i=0;i<BOOKIES;i++)); do
  k wait -n "$NS" "pvc/ledgers-$NAME-bookie-$i" "--for=jsonpath={.status.capacity.storage}=$NEW_SIZE" "--timeout=${TIMEOUT}s"
done
deadline=$((SECONDS+TIMEOUT)); AFTER_KIB=0
while (( SECONDS < deadline )); do
  k exec -n "$NS" "$BOOKIE" -- df -Pk /pulsar/data/bookkeeper/ledgers > "$WORK_DIR/df-after.txt"
  AFTER_KIB=$(awk 'NR==2 {print $2}' "$WORK_DIR/df-after.txt")
  [[ "$AFTER_KIB" =~ ^[0-9]+$ ]] || fail 'Unable to parse expanded ledger filesystem size'
  (( AFTER_KIB > BEFORE_KIB )) && break
  sleep 3
done
(( AFTER_KIB > BEFORE_KIB )) || fail 'Ledger filesystem did not grow online before timeout'
k get pvc -n "$NS" -o json > "$WORK_DIR/pvcs-after.json"
while IFS= read -r pv; do k get pv "$pv" -o json > "$WORK_DIR/pvs/$pv.after.json"; done < "$WORK_DIR/pv-names.txt"
python3 - "$WORK_DIR" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); before={p['metadata']['name']:p for p in json.loads((r/'pvcs-before.json').read_text())['items']}; after={p['metadata']['name']:p for p in json.loads((r/'pvcs-after.json').read_text())['items']}
for name in (r/'required-pvcs.txt').read_text().splitlines():
    old,new=before[name],after[name]
    if old['metadata']['uid']!=new['metadata']['uid'] or old['spec']['volumeName']!=new['spec']['volumeName']:
        raise SystemExit(f'{name}: PVC identity or PV binding changed during Pod recreation/expansion')
    pv=old['spec']['volumeName']
    if json.loads((r/'pvs'/f'{pv}.json').read_text())['metadata']['uid']!=json.loads((r/'pvs'/f'{pv}.after.json').read_text())['metadata']['uid']:
        raise SystemExit(f'{pv}: PV UID changed')
print('PASS: existing PVC/PV identities retained through online expansion')
PY

printf 'Creating a separate 1Gi retention probe; Pulsar-owned PVCs will not be deleted.\n'
k create -f "$WORK_DIR/probe-manifest.json"
k wait -n "$NS" "pod/$PROBE" --for=condition=Ready "--timeout=${TIMEOUT}s"
k exec -n "$NS" "$PROBE" -- sh -c 'printf "%s\n" "$1" > /data/retained-marker; sync; test "$(cat /data/retained-marker)" = "$1"' sh "$MARKER"
k get pvc "$PROBE" -n "$NS" -o json > "$WORK_DIR/probe-pvc.json"
PROBE_PV=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["spec"]["volumeName"])' "$WORK_DIR/probe-pvc.json")
k get pv "$PROBE_PV" -o json > "$WORK_DIR/probe-pv.json"
python3 - "$WORK_DIR" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); pv=json.loads((r/'probe-pv.json').read_text()); pvc=json.loads((r/'probe-pvc.json').read_text())
if pv['spec'].get('persistentVolumeReclaimPolicy')!='Retain' or pv['spec'].get('csi',{}).get('driver')!='local.csi.openebs.io' or pv['spec'].get('claimRef',{}).get('uid')!=pvc['metadata']['uid']:
    raise SystemExit('Retention probe PV identity/driver/policy does not match; leaving resources untouched')
PY
k delete pod "$PROBE" -n "$NS" --wait=true "--timeout=${TIMEOUT}s"
[[ "$(k get pv "$PROBE_PV" -o 'jsonpath={.spec.persistentVolumeReclaimPolicy}')" == Retain ]] || fail 'Probe PV policy changed; refusing PVC deletion'
k delete pvc "$PROBE" -n "$NS" --wait=true "--timeout=${TIMEOUT}s"
k wait "pv/$PROBE_PV" '--for=jsonpath={.status.phase}=Released' "--timeout=${TIMEOUT}s"
k get pv "$PROBE_PV" -o json > "$WORK_DIR/probe-retained.json"
python3 - "$WORK_DIR" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1]); old=json.loads((r/'probe-pv.json').read_text()); new=json.loads((r/'probe-retained.json').read_text())
if old['metadata']['uid']!=new['metadata']['uid'] or new['metadata'].get('deletionTimestamp') or new['status']['phase']!='Released' or new['spec']['persistentVolumeReclaimPolicy']!='Retain':
    raise SystemExit('Retention probe did not preserve the original PV')
PY

printf '\nPASS [%s]: Pulsar PVC topology, message persistence, ledger expansion, and independent Retain lifecycle.\n' "$PROFILE"
if [[ "$PROFILE" == two-node-shared-disk-lab ]]; then
  printf 'Shared-disk lab only: journal/ledger have no physical I/O isolation; host-failure HA is not certified.\n'
fi
printf 'Ledger filesystem grew from %s KiB to %s KiB.\n' "$BEFORE_KIB" "$AFTER_KIB"
printf 'The PulsarCluster remains running with ledger.size=%s; no shrink/rollback is attempted.\n' "$NEW_SIZE"
printf 'Retained probe PV: %s (1Gi remains allocated; do not automatically wipe it).\n' "$PROBE_PV"
printf 'Test topic/subscription remain: %s / %s\n' "$TOPIC" "$SUB"
printf 'Bookie-0 journal and ledger each retain marker file: .diskfleet-e2e-%s\n' "$RUN_ID"
printf 'Not tested: bookie decommission, node/disk failure, multi-cluster failover, or production scale.\n'
