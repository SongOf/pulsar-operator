"""Explicit, immutable StorageClass snapshots spanning reviewed DiskFleet pools.

Per-disk classes remain useful for an explicitly placed PVC. StatefulSet
templates need a shared class so each replica can select a different node.
"""
import hashlib
import json
import re

from .store import GiB, Problem, name

LABEL = 'storage.diskfleet.io/'
SOURCE_REVISION = '3c042c5'
SOURCE_REPOSITORY = 'https://github.com/SongOf/pulsar-operator'
TWO_NODE_PROFILES = ('two-node-lab', 'two-node-shared-disk-lab')
PULSAR_PROFILES = ('three-node-e2e', *TWO_NODE_PROFILES)
TWO_NODE_LABEL = 'pulsar.songof.dev/two-node'


def _pool_identity(pool):
    if pool.get('state') != 'Ready' or not pool.get('uid') or not pool.get('node'):
        raise Problem('共享存储类只允许引用具有 UID 的 Ready 池', 409)
    if not re.fullmatch(r'df_[a-z0-9][a-z0-9_-]{0,51}', pool.get('vgName','')):
        raise Problem('不合法的 DiskFleet VG 名称')
    if not pool.get('topologyHostname'):
        raise Problem('缺少节点的真实 hostname 拓扑')


def validate_two_node_targets(pools, assignments, nodes, profile='two-node-lab'):
    """Check a fresh Node list against the pools selected by a live lab plan.

The installer resolves IPs to node names and labels the two selected hosts.
This check does not infer identity from IPs or silently add/remove labels.
An offline pool snapshot cannot establish these live scheduling facts.
"""
    selected=[n for n in nodes if n.get('metadata',{}).get('labels',{}).get(TWO_NODE_LABEL)=='true']
    if len(selected)!=2:
        raise Problem(profile+' 要求恰好 2 个带 '+TWO_NODE_LABEL+'=true 标签的节点',409)
    by_node={n.get('metadata',{}).get('name'):n for n in selected}
    if len(by_node)!=2 or not all(by_node):
        raise Problem(profile+' 节点名称缺失或重复',409)
    for node_name,node in by_node.items():
        ready=any(c.get('type')=='Ready' and c.get('status')=='True' for c in node.get('status',{}).get('conditions',[]))
        if not ready or node.get('spec',{}).get('unschedulable') or node['metadata'].get('deletionTimestamp'):
            raise Problem(node_name+' 必须 Ready、未封锁且未删除，才能生成 '+profile+' 计划',409)
        if not node['metadata'].get('labels',{}).get('kubernetes.io/hostname'):
            raise Problem(node_name+' 缺少真实 kubernetes.io/hostname 标签',409)
    by_id={p['id']:p for p in pools}
    for role in ('zookeeper','journal','ledger'):
        ids=assignments.get(role,[])
        if not ids or any(pid not in by_id for pid in ids):
            raise Problem(role+' 缺少已审核的存储池')
        role_pools=[by_id[pid] for pid in ids]
        if {p.get('node') for p in role_pools}!=set(by_node):
            raise Problem(role+' 池所在节点必须与已标记的两个 Ready 节点完全相同',409)
        for pool in role_pools:
            hostname=by_node[pool['node']]['metadata']['labels']['kubernetes.io/hostname']
            if pool.get('topologyHostname')!=hostname:
                raise Problem(pool['id']+' 的 hostname 拓扑与实时节点标签不一致',409)


def fleet_storage_class(class_name, pools):
    name(class_name)
    if not pools:
        raise Problem('共享存储类至少需要一个已审核的存储池')
    if len({p['id'] for p in pools}) != len(pools):
        raise Problem('共享存储类不能重复引用同一个池')
    for pool in pools:
        _pool_identity(pool)
    owners = {p['id']:p['uid'] for p in pools}
    return dict(
        apiVersion='storage.k8s.io/v1', kind='StorageClass',
        metadata=dict(name=class_name, labels={LABEL+'fleet':'true'},
                      annotations={LABEL+'pool-uids':json.dumps(owners,sort_keys=True,separators=(',',':'))}),
        provisioner='local.csi.openebs.io',
        parameters=dict(storage='lvm',vgpattern='^('+'|'.join(sorted({p['vgName'] for p in pools}))+')$',
                        fsType='ext4',thinProvision='no'),
        reclaimPolicy='Retain',volumeBindingMode='WaitForFirstConsumer',allowVolumeExpansion=True,
        allowedTopologies=[dict(matchLabelExpressions=[dict(key='kubernetes.io/hostname',values=sorted({p['topologyHostname'] for p in pools}))])])


def pulsar_plan(pools, assignments, namespace, cluster_name, replicas=3, profile='three-node-e2e'):
    """Render the actual SongOf CR fields, with topology/capacity preflight.

No API writes or block IO. By default, one journal and one ledger pool on every
eligible node keep their physical devices separate. ZooKeeper may share a journal VG;
the aggregate requested capacity is checked, including 2 GiB per ledger VG
for the expansion and independent Retain probes.

The explicit two-node-lab profile keeps three ZooKeepers and three bookies,
but relaxes their anti-affinity to Preferred. The SongOf PodPolicy has no
hard topology-spread constraint, so it cannot promise a 2+1 placement. Each
eligible pool must fit all three replicas (and ledger expansion headroom).
This is a functional lab, not host-failure HA or the three-node E2E target.

Only two-node-shared-disk-lab permits journal and ledger to share a disk. All
three roles must select the same two Ready pools, one on each node. Each pool
must have 94 GiB free: 15 GiB ZooKeeper + 15 GiB journal + 64 GiB ledger with
headroom. Logical role-specific StorageClasses do not provide disk isolation.
"""
    name(namespace); name(cluster_name)
    if profile not in PULSAR_PROFILES:
        raise Problem('不支持的 Pulsar profile：'+str(profile))
    two_node = profile in TWO_NODE_PROFILES
    shared_disk = profile == 'two-node-shared-disk-lab'
    if len(cluster_name)>40:
        raise Problem('验收用 PulsarCluster 名称不能超过 40 个字符')
    if not namespace.startswith('diskfleet-pulsar-'):
        raise Problem('测试命名空间必须以 diskfleet-pulsar- 开头')
    if isinstance(replicas,bool) or not isinstance(replicas,int) or replicas<3:
        raise Problem('多节点验证至少需要 3 个副本')
    if two_node and replicas != 3:
        raise Problem(profile+' 固定为 3 个 ZooKeeper 和 3 个 Bookie 副本')
    if not isinstance(assignments,dict) or set(assignments)!={'zookeeper','journal','ledger'}:
        raise Problem('必须分别指定 zookeeper、journal、ledger 池集合')
    by_id={p['id']:p for p in pools}
    if len(by_id)!=len(pools):
        raise Problem('存储池清单中存在重复 ID')
    selected={}
    role_hosts={}
    for role, ids in assignments.items():
        if not isinstance(ids,list) or not ids or any(not isinstance(x,str) for x in ids) or len(set(ids))!=len(ids):
            raise Problem(role+' 需要不重复的池 ID 列表')
        if any(i not in by_id for i in ids):
            raise Problem(role+' 引用了不存在的池')
        selected[role]=[by_id[i] for i in ids]
        for pool in selected[role]: _pool_identity(pool)
        if len({p['node'] for p in selected[role]}) != len(ids):
            raise Problem(role+' 在每个节点只能选择一个池')
        role_hosts[role]={p['topologyHostname'] for p in selected[role]}
        if len(role_hosts[role])!=len(ids):
            raise Problem('多个节点使用了相同 hostname；无法验证故障域分离')
        if two_node and len(ids)!=2:
            raise Problem(role+' 的 '+profile+' 配置必须恰好覆盖 2 个不同节点')
        if not two_node and len(ids)<replicas:
            raise Problem(role+f' 需要至少 {replicas} 个不同节点，不能将单节点类用于 Required 反亲和')
    if not (role_hosts['zookeeper']==role_hosts['journal']==role_hosts['ledger']):
        raise Problem('三个存储等级必须覆盖相同节点，防止多 PVC 拓扑交集为空')
    if shared_disk and not (set(assignments['zookeeper'])==set(assignments['journal'])==set(assignments['ledger'])):
        raise Problem('two-node-shared-disk-lab 的三个存储角色必须选择同一组 2 个 Ready 池；每节点一个共享数据盘')
    if not shared_disk and set(assignments['journal']) & set(assignments['ledger']):
        raise Problem('journal 与 ledger 必须使用不同物理盘/存储池')
    for role, role_pools in selected.items():
        allowed_nodes={p['node'] for p in role_pools}
        allowed_vgs={p['vgName'] for p in role_pools}
        allowed_ids=set(assignments[role])
        if any(p['node'] in allowed_nodes and p['vgName'] in allowed_vgs and p['id'] not in allowed_ids for p in pools):
            raise Problem(role+' 的 VG 名称跨节点冲突，会选中未审核的其他池；请使用唯一 VG 名称')
    # Group-role mapping must refer to the same node behind a hostname.
    host_nodes={}
    for role_pools in selected.values():
        for p in role_pools:
            previous=host_nodes.setdefault(p['topologyHostname'],p['node'])
            if previous!=p['node']:
                raise Problem('hostname 映射到不同节点')
    required={}
    # Preferred anti-affinity is only a score. In lab mode all three pods
    # could share one host, so do not budget for an assumed 2+1 split.
    # Ledger: 20 GiB per replica + 1 GiB expansion per replica + one 1 GiB
    # independent Retain probe on a VG. Sharing roles sums their requirements:
    # a shared-data-disk pool therefore needs 15 + 15 + 64 = 94 GiB free.
    role_sizes = [('zookeeper',15),('journal',15),('ledger',64)] if two_node else [('zookeeper',5),('journal',5),('ledger',22)]
    for role,size in role_sizes:
        for p in selected[role]: required[p['id']]=required.get(p['id'],0)+size*GiB
    for pid,size in required.items():
        if by_id[pid].get('availableBytes',0)<size:
            raise Problem(pid+' 容量不足：需要 '+str(size//GiB)+' GiB（含验收余量）',409)
    identity=namespace+':'+cluster_name+(':'+profile if two_node else '')
    suffix=hashlib.sha256(identity.encode()).hexdigest()[:10]
    classes={role:f'diskfleet-pulsar-{suffix}-{role}' for role in selected}
    labels={LABEL+'integration':('pulsar-two-node-shared-disk-lab' if shared_disk else 'pulsar-two-node-lab') if two_node else 'pulsar'}
    annotations={LABEL+'source-revision':SOURCE_REVISION}
    if two_node:
        labels[LABEL+'profile']=profile
        annotations.update({
            LABEL+'availability':'lab-only-not-host-failure-ha',
            LABEL+'validation':'three-node-e2e-not-applicable',
            LABEL+'placement':'Preferred anti-affinity; a 2+1 placement is not guaranteed',
            LABEL+'capacity-preflight':'Each selected pool fits all 3 replicas; shared ZK/journal >=30Gi, ledger >=64Gi',
        })
        if shared_disk:
            annotations.update({
                LABEL+'storage-layout':'ZooKeeper, journal and ledger share one physical data disk per node; no disk or IO isolation',
                LABEL+'capacity-preflight':'Each selected shared pool fits all 3 replicas: ZK 15Gi + journal 15Gi + ledger 64Gi = 94Gi available',
            })
    scs=[]
    for role in ('zookeeper','journal','ledger'):
        sc=fleet_storage_class(classes[role],selected[role])
        sc['metadata']['labels'].update({**labels,LABEL+'role':role})
        scs.append(sc)
    pod=dict(antiAffinity='Preferred' if two_node else 'Required')
    psc=dict(apiVersion='pulsar.songof.dev/v1alpha1',kind='PulsarCluster',
        metadata=dict(name=cluster_name,namespace=namespace,labels=labels,
                      annotations=annotations),
        spec=dict(image='apachepulsar/pulsar:4.0.14',
            zookeeper=dict(replicas=replicas,pod=pod,storage=dict(size='5Gi',storageClassName=classes['zookeeper']),
                           jvm=dict(heap='512m'),resources=dict(requests=dict(cpu='200m',memory='1Gi'),limits=dict(memory='2Gi'))),
            bookkeeper=dict(replicas=replicas,minReplicas=3,pod=pod,
                            storage=dict(journal=dict(size='5Gi',storageClassName=classes['journal']),ledger=dict(size='20Gi',storageClassName=classes['ledger']),reclaimPolicy='Retain'),
                            jvm=dict(heap='1g',directMemory='1g'),resources=dict(requests=dict(cpu='500m',memory='3Gi'),limits=dict(memory='4Gi')),
                            config=dict(diskUsageThreshold='0.90',diskUsageLwmThreshold='0.85')),
            broker=dict(replicas=2,pod=dict(antiAffinity='Preferred'),jvm=dict(heap='1g',directMemory='1g'),
                        resources=dict(requests=dict(cpu='500m',memory='3Gi'),limits=dict(memory='4Gi')),
                        config=dict(managedLedgerDefaultEnsembleSize='2',managedLedgerDefaultWriteQuorum='2',managedLedgerDefaultAckQuorum='2')),
            proxy=dict(enabled=True,replicas=1,serviceType='ClusterIP',jvm=dict(heap='512m',directMemory='512m')),
            monitoring=dict(enabled=False)))
    if two_node:
        # The deployment workflow labels exactly the two reviewed Ready nodes.
        # Keep metadata-init jobs on those nodes too: the SongOf controller
        # inherits their nodeSelector/tolerations from Broker/Bookie PodPolicy.
        spec=psc['spec']
        spec['bookkeeper']['autoRecovery']=dict(enabled=True,replicas=1,
            jvm=dict(heap='256m',directMemory='256m'),
            resources=dict(requests=dict(cpu='100m',memory='1Gi'),limits=dict(memory='2Gi')))
        spec['proxy']['resources']=dict(requests=dict(cpu='100m',memory='1536Mi'),limits=dict(memory='2Gi'))
        for component in [spec['zookeeper'],spec['bookkeeper'],spec['broker'],spec['proxy'],spec['bookkeeper']['autoRecovery']]:
            policy=component.setdefault('pod',dict(antiAffinity='Preferred'))
            policy.update(nodeSelector={TWO_NODE_LABEL:'true'},tolerations=[
                dict(key='node-role.kubernetes.io/control-plane',operator='Exists',effect='NoSchedule'),
                dict(key='node-role.kubernetes.io/master',operator='Exists',effect='NoSchedule')])
    ns=dict(apiVersion='v1',kind='Namespace',metadata=dict(name=namespace,labels=labels))
    return dict(apiVersion='v1',kind='List',items=[ns,*scs,psc])
