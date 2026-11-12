"""Management API gateway. CRDs and PVCs remain the authoritative cluster state."""
import hashlib
import json
import re
import threading
import time
from urllib.parse import quote

from .kube import KubeClient, KubeError
from .operator import claims_overlap, conflicting_pool_names, node_label, storage_class
from .store import GiB, Problem, identifier, name, node_name, now, size_gib

BASE = '/apis/storage.diskfleet.io/v1alpha1/'
LABEL = 'storage.diskfleet.io/'
CLASS_FIELDS = ('provisioner', 'parameters', 'reclaimPolicy', 'volumeBindingMode',
                'allowVolumeExpansion', 'allowedTopologies')


def can_manage_pvc(metadata):
    """Observation must never transfer lifecycle control from another operator."""
    labels = metadata.get('labels', {})
    # StatefulSet Retain policies can leave PVCs without ownerReferences; the
    # upstream operator's manager label still reserves lifecycle ownership.
    return (labels.get(LABEL+'managed') == 'true'
            and labels.get('app.kubernetes.io/managed-by', '') in ('', 'diskfleet')
            and not metadata.get('ownerReferences'))


def class_matches(actual, expected):
    return all(actual.get(key) == expected.get(key) for key in CLASS_FIELDS)


def quantity(value):
    match = re.fullmatch(r'([0-9.]+)([KMGTPE]i|[kKMGTPE])?', str(value))
    if not match:
        return 0
    suffix = match[2] or ''
    power = {'Ki':10,'Mi':20,'Gi':30,'Ti':40,'Pi':50,'Ei':60}
    factor = 2 ** power[suffix] if suffix in power else 1000 ** ('kMGTPE'.find(suffix.replace('K','k'))+1) if suffix else 1
    return int(float(match[1]) * factor)


def bound_pool(pvc, pv, members):
    """Resolve a bound volume using its actual claim, driver, VG and topology."""
    if not pv:
        return {}, '绑定的 PV 不存在或尚不可读取'
    spec = pv.get('spec', {})
    claim = spec.get('claimRef', {})
    meta = pvc['metadata']
    if any(claim.get(key) != meta.get(key) for key in ('uid', 'namespace', 'name')):
        return {}, 'PV claimRef 与 PVC 身份不匹配'
    csi = spec.get('csi', {})
    if (csi.get('driver') != 'local.csi.openebs.io'
            or spec.get('storageClassName') != pvc['spec'].get('storageClassName')):
        return {}, 'PV 驱动或 StorageClass 与已审核绑定不匹配'
    terms = spec.get('nodeAffinity', {}).get('required', {}).get('nodeSelectorTerms', [])
    def on_node(pool):
        if not terms:
            return False
        expected = {'openebs.io/nodename': pool['node'],
                    'kubernetes.io/hostname': pool['topologyHostname']}
        # OR terms must all constrain this same node; an unconstrained term or
        # multiple node values cannot prove a local volume's physical location.
        for term in terms:
            expressions = [e for e in term.get('matchExpressions', []) if e.get('key') in expected]
            if not expressions or any(e.get('operator') != 'In' or e.get('values') != [expected[e['key']]]
                                      for e in expressions):
                return False
        return True
    matches = [p for p in members if csi.get('volumeAttributes', {}).get('openebs.io/volgroup') == p['vgName'] and on_node(p)]
    if len(matches) != 1:
        return {}, 'PV 的 VG/节点拓扑不能唯一对应已审核存储池'
    return matches[0], ''


class Cluster:
    def __init__(self, store, kube=None):
        self.store, self.kube = store, kube or KubeClient()
        self.lock = threading.Lock()
        self.snapshot, self.expires = {}, 0

    def invalidate(self):
        self.expires = 0

    @staticmethod
    def _retained_state(record, pvs):
        result = dict(record, canManage=False)
        result.pop('message', None)
        if not record.get('pvName'):
            return dict(result, status='Deleted')
        pv = pvs.get(record['pvName'])
        if (not pv or (record.get('pvUid') and pv.get('metadata', {}).get('uid') != record['pvUid'])
                or pv.get('spec', {}).get('claimRef', {}).get('uid') != record['pvcUid']
                or pv.get('spec', {}).get('persistentVolumeReclaimPolicy') != 'Retain'):
            return dict(result, status='ReleaseRequested', message='PVC 已不存在；保留 PV 身份或策略需要人工核对')
        return dict(result, status='Released' if pv.get('status', {}).get('phase') == 'Released' else 'ReleaseRequested')

    def _finish_release(self, record):
        with self.store.transaction() as con:
            self.store.put(con, 'retained', record)
            con.execute('DELETE FROM objects WHERE kind=? AND id=?', ('release-intents', record['id']))

    def _release_history(self, key, kind):
        return sorted((r for r in self.store.list(kind, limit=1000000)['items']
                       if r.get('pvcKey') == key), key=lambda r: r.get('requestedAt', ''), reverse=True)

    def _recover_release(self, record):
        pvs = {}
        if record.get('pvName'):
            try:
                pvs[record['pvName']] = self.kube.request('GET', '/api/v1/persistentvolumes/' + quote(record['pvName'], safe=''))
            except KubeError as error:
                if error.status != 404:
                    raise
        result = self._retained_state(record, pvs)
        self._finish_release(result)
        self.invalidate()
        return result

    def observed_classes(self, raw_pools, pools):
        """Resolve trusted DiskFleet classes to their verified pool identities.

        Pool health is not an ownership signal: volumes remain visible when a
        pool becomes unhealthy. No labels or owner references are written here.
        """
        by_name = {p['id']: p for p in pools}
        result = {}
        # One paginated collection read avoids one API request per physical disk.
        pool_classes = {sc['metadata']['name']: sc for sc in self.kube.list(
            '/apis/storage.k8s.io/v1/storageclasses', label_selector=LABEL+'pool')}
        for raw in raw_pools:
            meta, status = raw['metadata'], raw.get('status', {})
            if not meta.get('uid') or not status.get('storageClass') or not status.get('topologyHostname'):
                continue
            expected = storage_class(raw, status['topologyHostname'])
            if status['storageClass'] != expected['metadata']['name']:
                continue
            actual = pool_classes.get(status['storageClass'])
            if actual is None:
                continue
            if (actual.get('metadata', {}).get('annotations', {}).get(LABEL+'pool-uid') == meta['uid']
                    and class_matches(actual, expected)):
                result[status['storageClass']] = [by_name[meta['name']]]
        for actual in self.kube.list('/apis/storage.k8s.io/v1/storageclasses', label_selector=LABEL+'fleet=true'):
            meta = actual.get('metadata', {})
            try:
                refs = json.loads(meta.get('annotations', {}).get(LABEL+'pool-uids', 'null'))
            except (ValueError, TypeError):
                continue
            if not isinstance(refs, dict) or not refs:
                continue
            if any(pool_id not in by_name or not uid or by_name[pool_id].get('uid') != uid for pool_id, uid in refs.items()):
                continue
            members = [by_name[pool_id] for pool_id in sorted(refs)]
            # Registration requires Ready pools; observation verifies the same
            # immutable identities even after a failure, without changing health.
            from .fleet import fleet_storage_class
            try:
                expected = fleet_storage_class(meta['name'], [dict(p, state='Ready') for p in members])
            except (ValueError, KeyError, TypeError):
                continue
            if class_matches(actual, expected):
                result[meta['name']] = members
        return result

    def read(self):
        with self.lock:
            if time.monotonic() < self.expires:
                return self.snapshot
            disks, pools, volumes, nodes = [], [], [], []
            rawp = self.kube.list(BASE + 'diskpools')
            conflicts = conflicting_pool_names(rawp)
            by_vg, by_sc, pool_devices = {}, {}, {}
            for p in rawp:
                m,s,t = p['metadata'],p['spec'],p.get('status',{})
                capacity, free = t.get('capacityBytes',0),t.get('availableBytes',0)
                obj = dict(id=m['name'],uid=m.get('uid',''),name=m.get('annotations',{}).get(LABEL+'display-name',m['name']),node=s['nodeName'],vgName=s['vgName'],capacityBytes=capacity,
                           allocatedBytes=max(0,capacity-free),availableBytes=free,state=t.get('phase','Pending'),diskIds=[],storageClass=t.get('storageClass',''),topologyHostname=t.get('topologyHostname',''),createdAt=m.get('creationTimestamp',''),message=t.get('message',''))
                if m['name'] in conflicts:
                    obj.update(state='Conflict',capacityBytes=0,allocatedBytes=0,availableBytes=0,
                               message='多个 DiskPool 认领相同节点的 VG 或设备；无法确认唯一所有者，容量不计入可分配汇总')
                pools.append(obj)
                by_vg[(s['nodeName'],s['vgName'])] = obj
                if obj['storageClass']:
                    by_sc[obj['storageClass']] = obj
                if m['name'] not in conflicts:
                    for device in s['devices']:
                        pool_devices[(s['nodeName'],device['path'],device['serial'],int(device['sizeBytes']))] = obj
            for d in self.kube.list(BASE+'physicaldisks'):
                m,s,t=d['metadata'],d['spec'],d.get('status',{})
                p = pool_devices.get((s['nodeName'],s['path'],s.get('serial',''),int(s.get('sizeBytes',0))))
                obj=dict(id=m['name'],name=s['path'].rsplit('/',1)[-1],node=s['nodeName'],path=s['path'],serial=s.get('serial',''),sizeBytes=s.get('sizeBytes',0),media=s.get('media','Unknown'),model=s.get('model',''),state=t.get('phase','Protected'),reasons=t.get('reasons',[]),poolId=p['id'] if p else None)
                if p:
                    p['diskIds'].append(obj['id'])
                disks.append(obj)
            classes = self.observed_classes([p for p in rawp if p['metadata']['name'] not in conflicts],
                                            [p for p in pools if p['id'] not in conflicts])
            # Third-party operators normally do not add DiskFleet's management
            # label. Observe their claims by verified StorageClass, read-only.
            pvcs = self.kube.list('/api/v1/persistentvolumeclaims')
            pvs = {pv['metadata']['name']: pv for pv in self.kube.list('/api/v1/persistentvolumes')}
            for v in pvcs:
                m,s,t=v['metadata'],v['spec'],v.get('status',{})
                members = classes.get(s.get('storageClassName'))
                if not members:
                    continue
                binding_message = ''
                bound = t.get('phase') == 'Bound' or bool(s.get('volumeName'))
                if bound:
                    p, binding_message = bound_pool(v, pvs.get(s.get('volumeName')), members)
                    actual_node = p.get('node', '')
                else:
                    selected_node = m.get('annotations', {}).get('volume.kubernetes.io/selected-node', '')
                    eligible = [p for p in members if p['node'] == selected_node] if selected_node else members
                    p = eligible[0] if len(eligible) == 1 else {}
                    actual_node = selected_node if any(member['node'] == selected_node for member in members) else p.get('node','')
                managed = can_manage_pvc(m) and not binding_message
                owners = [{'kind': owner.get('kind',''), 'name': owner.get('name',''), 'uid': owner.get('uid','')}
                          for owner in m.get('ownerReferences', [])]
                volumes.append(dict(id=m['namespace']+'~'+m['name'],pvcUid=m['uid'],name=m['name'],namespace=m['namespace'],poolId=p.get('id',''),pool=p.get('name','多节点存储类'),node=actual_node,sizeBytes=quantity(s['resources']['requests']['storage']),status='Releasing' if m.get('deletionTimestamp') else t.get('phase','Pending'),storageClass=s.get('storageClassName',''),pvName=s.get('volumeName',''),mountPath='由 CSI / kubelet 管理',createdAt=m.get('creationTimestamp',''),management='diskfleet' if can_manage_pvc(m) else 'external',canManage=managed,owners=owners,bindingVerified=bool(bound and p),bindingMessage=binding_message))
            node_stats={}
            for disk in disks:
                stats=node_stats.setdefault(disk['node'],dict(disks=0,capacityBytes=0,allocatedBytes=0))
                stats['disks']+=1
                stats['capacityBytes']+=disk['sizeBytes']
            for pool in pools:
                stats=node_stats.setdefault(pool['node'],dict(disks=0,capacityBytes=0,allocatedBytes=0))
                stats['allocatedBytes']+=pool['allocatedBytes']
            for n in self.kube.list('/api/v1/nodes', label_selector=LABEL+'enabled=true'):
                m=n['metadata']; node=m['name']
                ready=next((c.get('status') for c in n.get('status',{}).get('conditions',[]) if c['type']=='Ready'),'Unknown')
                stats=node_stats.get(node,dict(disks=0,capacityBytes=0,allocatedBytes=0))
                nodes.append(dict(name=node,zone=m.get('labels',{}).get('topology.kubernetes.io/zone','—'),state='Ready' if ready=='True' else 'NotReady',**stats))
            # Released records are audit metadata only; real space comes from live VG status.
            # A class losing its trusted identity must not make a still-live PVC
            # appear deleted in our retained history.
            live_uids={v.get('metadata', {}).get('uid') for v in pvcs}
            deleting_uids={v['metadata']['uid'] for v in pvcs if v['metadata'].get('deletionTimestamp')}
            for intent in self.store.list('release-intents', limit=1000000)['items']:
                if intent['pvcUid'] not in live_uids:
                    self._finish_release(self._retained_state(intent, pvs))
                elif intent['pvcUid'] in deleting_uids:
                    self._finish_release(dict(intent, status='ReleaseRequested', canManage=False))
            retained=[]
            for record in self.store.list('retained',limit=1000000)['items']:
                uid=record.get('pvcUid',record['id'].removeprefix('retained-'))
                if uid not in live_uids:
                    # A same-name new PVC has a different UID and must not hide
                    # the old retained volume. Never infer deletion from name.
                    retained.append(self._retained_state(dict(record, pvcUid=uid), pvs))
            self.snapshot=dict(disks=disks,pools=pools,volumes=volumes+retained,nodes=nodes)
            self.expires=time.monotonic()+30
            return self.snapshot

    def list(self, kind, limit=50, offset=0, node='', state='', q=''):
        items=self.read()[kind]
        if node: items=[i for i in items if i.get('node',i.get('name'))==node]
        if state: items=[i for i in items if i.get('state',i.get('status'))==state]
        if q: items=[i for i in items if q.lower() in str(i).lower()]
        items=sorted(items,key=lambda i:(i.get('node',''),i.get('name','')))
        return dict(items=items[offset:offset+limit],total=len(items),limit=limit,offset=offset)

    def events(self, **kwargs):
        return self.store.events(**kwargs)

    def overview(self):
        data=self.read(); pools=data['pools']
        alerts=[dict(level='warning',message=p['name']+': '+p.get('message',p['state'])) for p in pools if p['state'] not in ('Ready','Pending')]
        return dict(mode='cluster',totals=dict(**{k:len(v) for k,v in data.items()},capacityBytes=sum(p['capacityBytes'] for p in pools),allocatedBytes=sum(p['allocatedBytes'] for p in pools),availableBytes=sum(p['availableBytes'] for p in pools)),recentEvents=self.events(limit=8)['items'],alerts=alerts)

    def plan(self, body):
        pool_name,node=name(body.get('name')),node_name(body.get('node'))
        ids=body.get('diskIds')
        if not isinstance(ids,list) or len(ids)!=1:
            raise Problem('每个池只能包含一块盘')
        self.invalidate()
        disk=next((d for d in self.read()['disks'] if d['id']==ids[0]),None)
        if not disk or disk['node']!=node or disk['state']!='Available' or disk['poolId']:
            raise Problem('磁盘不可用或已纳管',409)
        digest=hashlib.sha256((node+':'+disk['serial']).encode()).hexdigest()[:16]
        vg='df_'+digest
        return self.store.save_plan(dict(name=pool_name,node=node,diskIds=ids,disks=[disk],vgName=vg,resourceName='pool-'+digest,confirmation='接管 '+disk['serial'],commands=[['pvcreate',disk['path']],['vgcreate',vg,disk['path']]],warnings=['此计划会初始化所选空盘。节点必须显式开启 ENABLE_DISK_WRITES。','一块盘一个 VG；原始盘不需要挂到 /mnt，业务 LV 由 CSI 挂载。']))

    def create_pool(self, plan_id, confirmation):
        # SQLite serializes commits; deterministic CR name ensures retries cannot double-enroll a disk.
        with self.store.transaction() as con:
            plan,consumed=self.store.read_plan(con,plan_id,confirmation)
            resource=plan['resourceName']
            disk=plan['disks'][0]
            spec=dict(nodeName=plan['node'],devices=[{k:disk[k] for k in ('path','serial','sizeBytes')}],vgName=plan['vgName'],approved=True)
            body=dict(apiVersion='storage.diskfleet.io/v1alpha1',kind='DiskPool',metadata=dict(name=resource,labels={LABEL+'node':node_label(plan['node'])},annotations={LABEL+'display-name':plan['name']}),spec=spec)
            # Fresh, unfiltered claims are required; mutable node labels and
            # the UI cache are not authority for granting device ownership.
            for other in self.kube.list(BASE+'diskpools'):
                if other['metadata']['name'] != resource and claims_overlap(spec, other['spec']):
                    raise Problem('磁盘或 VG 已由其它 DiskPool 认领：'+other['metadata']['name'],409)
            try:
                existing = self.kube.request('GET', BASE+'diskpools/'+resource)
            except KubeError as error:
                if error.status != 404: raise
                existing = None
            if existing is not None:
                if existing.get('spec') != spec or existing.get('metadata', {}).get('deletionTimestamp'):
                    raise Problem('磁盘已有参数不同或正在删除的接管请求',409)
            else:
                if consumed:
                    raise Problem('原接管资源已删除，拒绝自动重建；请调查磁盘状态',409)
                # Fetch the live CR, not the 30-second management snapshot. The
                # node backend independently rechecks the actual device before IO.
                current = self.kube.request('GET', BASE+'physicaldisks/'+quote(disk['id'],safe=''))
                actual, status = current.get('spec', {}), current.get('status', {})
                if (actual.get('nodeName') != disk['node']
                        or any(actual.get(k) != disk.get(k) for k in ('path','serial','sizeBytes'))
                        or status.get('phase') != 'Available' or status.get('poolName')):
                    raise Problem('磁盘身份或使用状态已变化，请重新生成接管计划',409)
                try:
                    self.kube.request('POST',BASE+'diskpools',body)
                except KubeError as error:
                    if error.status != 409: raise
                    existing=self.kube.request('GET',BASE+'diskpools/'+resource)
                    if existing.get('spec') != spec or existing.get('metadata', {}).get('deletionTimestamp'):
                        raise Problem('磁盘已有参数不同或正在删除的接管请求',409)
            con.execute('UPDATE plans SET consumed=1 WHERE id=?',(plan_id,))
            self.store.event(con,'pool.requested','提交磁盘接管请求 '+resource,plan['node'])
        self.invalidate()
        return next((p for p in self.read()['pools'] if p['id']==resource),dict(id=resource,name=plan['name'],state='Pending'))

    def create_volume(self, body):
        vol,ns=name(body.get('name')),name(body.get('namespace','default'))
        size=size_gib(body.get('sizeGiB'))
        p=next((p for p in self.read()['pools'] if p['id']==body.get('poolId')),None)
        if not p or p['state']!='Ready' or not p['storageClass']:
            raise Problem('存储池或 StorageClass 尚未就绪',409)
        actual_class = self.kube.request('GET','/apis/storage.k8s.io/v1/storageclasses/'+quote(p['storageClass'],safe=''))
        params = actual_class.get('parameters', {})
        topology = [{'matchLabelExpressions': [{'key':'kubernetes.io/hostname','values':[p.get('topologyHostname','')]}]}]
        if (actual_class.get('provisioner') != 'local.csi.openebs.io'
                or not p.get('topologyHostname')
                or actual_class.get('reclaimPolicy') != 'Retain'
                or actual_class.get('volumeBindingMode') != 'WaitForFirstConsumer'
                or params.get('vgpattern') != '^'+p['vgName']+'$'
                or params.get('thinProvision') != 'no'
                or actual_class.get('metadata',{}).get('annotations',{}).get(LABEL+'pool-uid') != p.get('uid')
                or actual_class.get('allowedTopologies') != topology):
            raise Problem('StorageClass 安全配置发生漂移，拒绝创建 PVC',409)
        request=dict(apiVersion='v1',kind='PersistentVolumeClaim',metadata=dict(name=vol,namespace=ns,labels={LABEL+'managed':'true',LABEL+'pool':p['id']}),spec=dict(accessModes=['ReadWriteOnce'],volumeMode='Filesystem',storageClassName=p['storageClass'],resources=dict(requests=dict(storage=str(size//GiB)+'Gi'))))
        path='/api/v1/namespaces/'+ns+'/persistentvolumeclaims'
        try:
            self.kube.request('POST',path,request)
        except KubeError as error:
            if error.status!=409: raise
            old=self.kube.request('GET',path+'/'+vol)
            if old['metadata'].get('deletionTimestamp'):
                raise Problem('同名 PVC 正在删除；等待删除完成后重新申请',409)
            if not can_manage_pvc(old['metadata']) or old['spec'].get('storageClassName')!=p['storageClass'] or quantity(old['spec']['resources']['requests']['storage'])!=size:
                raise Problem('同名 PVC 已存在且参数不同',409)
        self.store.audit('pvc.created',f'{ns}/{vol} 等待消费 Pod 后由 CSI 分配',p['node'])
        self.invalidate()
        return dict(id=ns+'~'+vol,name=vol,namespace=ns,status='Pending',sizeBytes=size,poolId=p['id'],node=p['node'])

    def volume(self,key):
        parts=key.split('~')
        if len(parts)!=2: raise Problem('无效 PVC 标识')
        ns,vol=map(name,parts)
        path='/api/v1/namespaces/'+ns+'/persistentvolumeclaims/'+vol
        pvc=self.kube.request('GET',path)
        if not can_manage_pvc(pvc['metadata']):
            raise Problem('该 PVC 由外部工作负载管理，DiskFleet 仅提供只读观察',403)
        if pvc['spec'].get('volumeName') or pvc.get('status',{}).get('phase') == 'Bound':
            self.invalidate()
            observed=next((v for v in self.read()['volumes'] if v['id']==key and v.get('pvcUid')==pvc['metadata']['uid']),None)
            if not observed or not observed.get('bindingVerified'):
                raise Problem('PVC/PV 绑定身份无法核验，拒绝修改',409)
        return path,pvc

    def expand(self,key,body):
        size=size_gib(body.get('sizeGiB'))
        path,pvc=self.volume(key)
        if pvc['metadata'].get('deletionTimestamp'):
            raise Problem('PVC 正在释放，不能扩容',409)
        if size<quantity(pvc['spec']['resources']['requests']['storage']):
            raise Problem('不支持缩容',409)
        self.kube.request('PATCH',path,dict(metadata=dict(resourceVersion=pvc['metadata']['resourceVersion']),spec=dict(resources=dict(requests=dict(storage=str(size//GiB)+'Gi')))))
        self.store.audit('pvc.expanded',key+' 扩容请求已提交')
        self.invalidate()
        return dict(id=key,sizeBytes=size,status='Pending')

    def release(self,key,expected_uid=None):
        if expected_uid is not None:
            identifier(expected_uid)
        parts=key.split('~')
        if len(parts)!=2: raise Problem('无效 PVC 标识')
        ns,vol=map(name,parts)
        path='/api/v1/namespaces/'+ns+'/persistentvolumeclaims/'+vol
        intents=self._release_history(key,'release-intents')
        completed=self._release_history(key,'retained')
        try:
            pvc=self.kube.request('GET',path)
        except KubeError as error:
            if error.status != 404: raise
            history=intents or completed
            if expected_uid is not None:
                history=[r for r in (*intents,*completed) if r['pvcUid']==expected_uid]
            if not history: raise
            return self._recover_release(history[0])
        # Bind requests to the UID displayed by the client. Name-only retries
        # are refused after a name is reused, including after a read recovered
        # the old intent into retained history.
        if expected_uid is not None and expected_uid != pvc['metadata']['uid']:
            history=[r for r in (*intents,*completed) if r['pvcUid']==expected_uid]
            if history:
                return self._recover_release(history[0])
            raise Problem('PVC UID 已变化，请刷新后重新确认释放',409)
        if expected_uid is None and not intents and completed and completed[0]['pvcUid'] != pvc['metadata']['uid']:
            raise Problem('同名 PVC 已重建，释放时必须提供当前 PVC UID',409)
        if intents and intents[0]['pvcUid'] != pvc['metadata']['uid']:
            if expected_uid is None:
                return self._recover_release(intents[0])
            # The caller explicitly confirmed the new object; finish older
            # intent bookkeeping before processing that independent request.
            self._recover_release(intents[0])
            intents=[]
        if not can_manage_pvc(pvc['metadata']):
            raise Problem('该 PVC 由外部工作负载管理，DiskFleet 仅提供只读观察',403)
        if intents and pvc['metadata'].get('deletionTimestamp'):
            result=dict(intents[0],status='ReleaseRequested',canManage=False)
            self._finish_release(result)
            self.invalidate()
            return result
        pv_name=pvc['spec'].get('volumeName')
        pv={}
        if pv_name:
            pv=self.kube.request('GET','/api/v1/persistentvolumes/'+quote(pv_name,safe=''))
            if pv['spec'].get('persistentVolumeReclaimPolicy')!='Retain':
                raise Problem('实际 PV 回收策略不是 Retain，拒绝释放',409)
            if any(pv['spec'].get('claimRef',{}).get(k) != pvc['metadata'].get(k) for k in ('uid','namespace','name')):
                raise Problem('实际 PV claimRef 与 PVC 身份不匹配，拒绝释放',409)
        self.invalidate()
        obj=next((v for v in self.read()['volumes'] if v['id']==key),None)
        if not obj: raise Problem('卷快照不可用，请重试',409)
        if obj.get('pvcUid') != pvc['metadata']['uid'] or obj.get('bindingMessage'):
            raise Problem('PVC/PV 绑定身份无法核验，拒绝释放',409)
        obj=dict(obj,id='retained-'+pvc['metadata']['uid'],pvcUid=pvc['metadata']['uid'],
                 pvcKey=key,pvcResourceVersion=pvc['metadata']['resourceVersion'],
                 pvName=pv_name or '',pvUid=pv.get('metadata',{}).get('uid',''),
                 requestedAt=intents[0]['requestedAt'] if intents else now(),
                 status='ReleaseRequested',name=pvc['metadata']['name']+' (retained)',canManage=False,
                 sizeBytes=quantity(pvc['spec']['resources']['requests']['storage']))
        # Commit intent before the remote mutation. It survives both a lost
        # response and a crash before the retained record can be committed.
        with self.store.transaction() as con:
            self.store.put(con,'release-intents',obj)
            self.store.event(con,'pvc.release-requested',key+' 已请求删除；若仍被 Pod 使用，Kubernetes 会等待卸载。已绑定 PV 保留。')
        self.invalidate()
        self.kube.request('DELETE',path,dict(apiVersion='v1',kind='DeleteOptions',preconditions=dict(uid=pvc['metadata']['uid'],resourceVersion=pvc['metadata']['resourceVersion'])))
        self._finish_release(obj)
        return obj
