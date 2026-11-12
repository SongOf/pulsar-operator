import copy
import json
import tempfile
import unittest
from unittest.mock import patch

from diskfleet.cluster import BASE, LABEL, Cluster
from diskfleet.kube import KubeError
from diskfleet.operator import storage_class
from diskfleet.store import GiB, Problem, Store


class FakeKube:
    def __init__(self):
        self.objects = {}
        self.calls = []
        self.delete_conflict = False

    def request(self, method, path, body=None):
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == 'GET':
            if path not in self.objects:
                raise KubeError(404, 'not found')
            return copy.deepcopy(self.objects[path])
        if method == 'POST':
            target = path + '/' + body['metadata']['name']
            if target in self.objects:
                raise KubeError(409, 'already exists')
            obj = copy.deepcopy(body)
            obj['metadata'].update(uid='uid-created',resourceVersion='1')
            self.objects[target] = obj
            return copy.deepcopy(obj)
        if method == 'DELETE':
            if self.delete_conflict:
                raise KubeError(409, 'resourceVersion changed')
            old = self.objects[path]
            if any(old['metadata'].get(k) != v for k,v in body['preconditions'].items()):
                raise KubeError(409, 'precondition failed')
            for key, obj in list(self.objects.items()):
                if '/persistentvolumeclaims/' in key and obj.get('metadata', {}).get('uid') == old['metadata']['uid']:
                    self.objects.pop(key)
            pv = self.objects.get('/api/v1/persistentvolumes/'+old['spec'].get('volumeName',''))
            if pv:
                pv['status'] = {'phase':'Released'}
            return old
        if method == 'PATCH':
            return copy.deepcopy(body)
        raise AssertionError((method,path))

    def list(self, path, label_selector=None, field_selector=None):
        values = [copy.deepcopy(obj) for target,obj in self.objects.items() if target.rsplit('/',1)[0] == path]
        if label_selector:
            if '=' in label_selector:
                key,value=label_selector.split('=',1)
                values=[obj for obj in values if obj['metadata'].get('labels',{}).get(key)==value]
            else:
                values=[obj for obj in values if label_selector in obj['metadata'].get('labels',{})]
        return values


class ClusterTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.store = Store(self.temp.name)
        self.kube = FakeKube()
        self.cluster = Cluster(self.store,self.kube)
        self.disk = dict(id='disk-one',node='worker-01',path='/dev/disk/by-id/wwn-one',serial='SER001',
                         sizeBytes=100*GiB,state='Available',poolId=None)
        self.pool = dict(id='pool-one',uid='pool-uid',name='test',state='Ready',node='worker-01',vgName='df_one',
                         storageClass='diskfleet-pool-one',topologyHostname='worker-01',capacityBytes=100*GiB,availableBytes=90*GiB)
        self.data = dict(disks=[self.disk],pools=[self.pool],volumes=[],nodes=[])
        self.cluster.read = lambda: self.data
        self.physical = dict(metadata=dict(name='disk-one'),spec=dict(nodeName='worker-01',path=self.disk['path'],
                             serial='SER001',sizeBytes=100*GiB),status=dict(phase='Available',poolName=''))
        self.kube.objects[BASE+'physicaldisks/disk-one']=self.physical
        rawpool=dict(metadata=dict(name='pool-one',uid='pool-uid'),
            spec=dict(nodeName='worker-01',vgName='df_one',devices=[{k:self.disk[k] for k in ('path','serial','sizeBytes')}]),
            status=dict(phase='Ready',storageClass='diskfleet-pool-one',topologyHostname='worker-01',capacityBytes=100*GiB,availableBytes=90*GiB))
        self.kube.objects[BASE+'diskpools/pool-one']=rawpool
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-pool-one']=storage_class(rawpool)

    def prepare_volume(self, policy='Retain', managed=True):
        pvc = dict(metadata=dict(name='data',namespace='default',uid='pvc-uid',resourceVersion='17',
                   labels={LABEL+'managed':'true' if managed else 'false'}),
                   spec=dict(volumeName='pv-data',storageClassName=self.pool['storageClass'],
                             resources=dict(requests=dict(storage='1Gi'))),status=dict(phase='Bound'))
        self.kube.objects['/api/v1/namespaces/default/persistentvolumeclaims/data']=pvc
        self.kube.objects['/api/v1/persistentvolumes/pv-data']=dict(
            metadata=dict(name='pv-data',uid='pv-uid'),status=dict(phase='Bound'),
            spec=dict(persistentVolumeReclaimPolicy=policy,storageClassName=self.pool['storageClass'],
                      claimRef=dict(uid='pvc-uid',namespace='default',name='data'),
                      csi=dict(driver='local.csi.openebs.io',volumeHandle='volume-one',volumeAttributes={'openebs.io/volgroup':'df_one'}),
                      nodeAffinity=dict(required=dict(nodeSelectorTerms=[dict(matchExpressions=[dict(
                          key='openebs.io/nodename',operator='In',values=['worker-01'])])]))))
        self.data['volumes']=[dict(id='default~data',pvcUid='pvc-uid',name='data',namespace='default',node='worker-01',status='Bound',sizeBytes=GiB,bindingVerified=True)]
        return pvc

    def plan_new_disk(self, body):
        # Pool fixtures used by volume tests are not an existing claim when
        # exercising enrollment of this new, Available disk.
        self.kube.objects.pop(BASE+'diskpools/pool-one',None)
        return self.cluster.plan(body)

    def test_release_is_retain_only_and_uses_identity_preconditions(self):
        self.prepare_volume()
        result=self.cluster.release('default~data')
        self.assertEqual(result['status'],'ReleaseRequested')
        delete=[body for method,path,body in self.kube.calls if method=='DELETE'][0]
        self.assertEqual(delete['preconditions'],dict(uid='pvc-uid',resourceVersion='17'))
        self.assertEqual(len(self.store.list('retained')['items']),1)

    def test_release_rejects_actual_delete_pv_policy(self):
        self.prepare_volume(policy='Delete')
        with self.assertRaises(Problem) as ctx:
            self.cluster.release('default~data')
        self.assertEqual(ctx.exception.status,409)
        self.assertFalse(any(c[0]=='DELETE' for c in self.kube.calls))

    def test_external_pvc_cannot_be_released_or_expanded(self):
        self.prepare_volume(managed=False)
        with self.assertRaises(Problem) as ctx:
            self.cluster.release('default~data')
        self.assertEqual(ctx.exception.status,403)
        with self.assertRaises(Problem):
            self.cluster.expand('default~data',{'sizeGiB':2})
        self.assertFalse(any(c[0] in ('DELETE','PATCH') for c in self.kube.calls))

    def test_delete_race_does_not_create_false_retained_record(self):
        self.prepare_volume()
        self.kube.delete_conflict=True
        with self.assertRaises(KubeError):
            self.cluster.release('default~data')
        self.assertEqual(self.store.list('retained')['total'],0)

    def test_plan_is_revalidated_against_live_inventory_before_commit(self):
        plan=self.plan_new_disk(dict(name='first',node='worker-01',diskIds=['disk-one']))
        self.physical['spec']['serial']='REPLACED'
        with self.assertRaises(Problem) as ctx:
            self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.assertEqual(ctx.exception.status,409)
        self.assertFalse(any(c[0]=='POST' for c in self.kube.calls))

    def test_plan_rejects_disk_that_became_used(self):
        plan=self.plan_new_disk(dict(name='first',node='worker-01',diskIds=['disk-one']))
        self.physical['status']['phase']='Protected'
        with self.assertRaises(Problem):
            self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.assertFalse(any(c[0]=='POST' for c in self.kube.calls))

    def test_pool_retry_is_idempotent_even_after_disk_became_managed(self):
        plan=self.plan_new_disk(dict(name='first',node='worker-01',diskIds=['disk-one']))
        first=self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.physical['status']['phase']='Managed'
        # Model a successful API write whose local audit transaction was lost.
        with self.store.transaction() as con:
            con.execute('UPDATE plans SET consumed=0 WHERE id=?',(plan['planId'],))
        second=self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.assertEqual(first['id'],second['id'])
        self.assertEqual(len([c for c in self.kube.calls if c[0]=='POST']),1)

    def test_consumed_plan_does_not_recreate_deleted_pool(self):
        plan=self.plan_new_disk(dict(name='first',node='worker-01',diskIds=['disk-one']))
        first=self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.kube.objects.pop(BASE+'diskpools/'+first['id'])
        with self.assertRaises(Problem):
            self.cluster.create_pool(plan['planId'],plan['confirmation'])

    def test_pvc_creation_waits_for_consumer_and_uses_exact_pool_class(self):
        result=self.cluster.create_volume(dict(name='data',namespace='default',poolId='pool-one',sizeGiB=2))
        self.assertEqual(result['status'],'Pending')
        request=[body for method,path,body in self.kube.calls if method=='POST'][0]
        self.assertEqual(request['spec']['storageClassName'],'diskfleet-pool-one')
        self.assertNotIn('volumeName',request['spec'])
        self.assertEqual(request['spec']['resources']['requests']['storage'],'2Gi')

    def test_pvc_creation_rejects_storageclass_drift(self):
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-pool-one']['reclaimPolicy']='Delete'
        with self.assertRaises(Problem):
            self.cluster.create_volume(dict(name='data',namespace='default',poolId='pool-one',sizeGiB=1))
        self.assertFalse(any(c[0]=='POST' for c in self.kube.calls))

    def test_existing_unmanaged_pvc_is_not_adopted(self):
        self.prepare_volume(managed=False)
        with self.assertRaises(Problem):
            self.cluster.create_volume(dict(name='data',namespace='default',poolId='pool-one',sizeGiB=1))

    def test_shrink_is_rejected(self):
        pvc=self.prepare_volume()
        pvc['spec']['resources']['requests']['storage']='2Gi'
        with self.assertRaises(Problem):
            self.cluster.expand('default~data',{'sizeGiB':1})
        self.assertFalse(any(c[0]=='PATCH' for c in self.kube.calls))

    def test_live_terminating_pvc_is_not_duplicated_by_retained_record(self):
        pvc=self.prepare_volume()
        pvc['metadata']['deletionTimestamp']='2026-11-13T01:00:00Z'
        # list-all-namespaces is a different collection from the GET endpoint.
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        with self.store.transaction() as con:
            self.store.put(con,'retained',dict(id='retained-pvc-uid',pvcUid='pvc-uid',pvName='pv-data',name='data (retained)',status='ReleaseRequested'))
        snapshot=Cluster(self.store,self.kube).read()
        self.assertEqual(len(snapshot['volumes']),1)
        self.assertEqual(snapshot['volumes'][0]['status'],'Releasing')

    def test_new_same_name_pvc_does_not_hide_old_retained_uid(self):
        pvc=self.prepare_volume()
        pvc['metadata']['uid']='new-pvc-uid'
        pvc['spec']['volumeName']='pv-new'
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        with self.store.transaction() as con:
            self.store.put(con,'retained',dict(id='retained-old-pvc-uid',pvcUid='old-pvc-uid',pvName='pv-old',name='data (retained)',status='ReleaseRequested'))
        self.kube.objects['/api/v1/persistentvolumes/pv-old']=dict(metadata=dict(name='pv-old'),
            spec=dict(claimRef=dict(uid='old-pvc-uid'),persistentVolumeReclaimPolicy='Retain'),status=dict(phase='Released'))
        snapshot=Cluster(self.store,self.kube).read()
        self.assertEqual(len(snapshot['volumes']),2)
        old=next(v for v in snapshot['volumes'] if v['id']=='retained-old-pvc-uid')
        self.assertEqual(old['status'],'Released')
        self.assertEqual(old['pvName'],'pv-old')

    def test_terminating_pvc_cannot_expand(self):
        pvc=self.prepare_volume()
        pvc['metadata']['deletionTimestamp']='2026-11-13T01:00:00Z'
        with self.assertRaises(Problem):
            self.cluster.expand('default~data',{'sizeGiB':2})

    def test_create_volume_accepts_actual_hostname_distinct_from_node(self):
        self.pool['topologyHostname']='host-in-rack-a'
        sc=self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-pool-one']
        sc['allowedTopologies'][0]['matchLabelExpressions'][0]['values']=['host-in-rack-a']
        result=self.cluster.create_volume(dict(name='data',namespace='default',poolId='pool-one',sizeGiB=1))
        self.assertEqual(result['status'],'Pending')

    def test_long_node_plan_uses_bounded_label_and_original_spec(self):
        long_node='worker.'+'a'*60+'.example.com'
        self.disk['node']=long_node
        self.physical['spec']['nodeName']=long_node
        plan=self.plan_new_disk(dict(name='first',node=long_node,diskIds=['disk-one']))
        self.cluster.create_pool(plan['planId'],plan['confirmation'])
        created=next(body for method,path,body in self.kube.calls if method=='POST')
        self.assertLessEqual(len(created['metadata']['labels'][LABEL+'node']),63)
        self.assertEqual(created['spec']['nodeName'],long_node)

    def test_node_totals_use_inventory_capacity_and_pool_allocation(self):
        self.kube.objects['/api/v1/nodes/worker-01']=dict(metadata=dict(name='worker-01',labels={LABEL+'enabled':'true'}),status=dict(conditions=[dict(type='Ready',status='True')]))
        self.kube.objects[BASE+'diskpools/pool-one']=dict(metadata=dict(name='pool-one',uid='pool-uid'),
            spec=dict(nodeName='worker-01',vgName='df_one',devices=[{k:self.disk[k] for k in ('path','serial','sizeBytes')}]),
            status=dict(phase='Ready',capacityBytes=99*GiB,availableBytes=80*GiB))
        snapshot=Cluster(self.store,self.kube).read()
        node=snapshot['nodes'][0]
        self.assertEqual(node['disks'],1)
        self.assertEqual(node['capacityBytes'],100*GiB)
        self.assertEqual(node['allocatedBytes'],19*GiB)

    def test_ready_pool_cannot_hide_offline_or_protected_disk(self):
        self.kube.objects[BASE+'diskpools/pool-one']=dict(metadata=dict(name='pool-one',uid='pool-uid'),
            spec=dict(nodeName='worker-01',vgName='df_one',devices=[{k:self.disk[k] for k in ('path','serial','sizeBytes')}]),
            status=dict(phase='Ready',capacityBytes=99*GiB,availableBytes=80*GiB))
        for phase in ('Offline','Protected'):
            with self.subTest(phase=phase):
                self.physical['status']=dict(phase=phase,reasons=['Recent node discovery warning'])
                snapshot=Cluster(self.store,self.kube).read()
                disk=snapshot['disks'][0]
                self.assertEqual(disk['state'],phase)
                self.assertEqual(disk['reasons'],['Recent node discovery warning'])
                self.assertEqual(disk['poolId'],'pool-one')
                self.assertEqual(snapshot['pools'][0]['diskIds'],['disk-one'])

    def test_external_operator_pvc_is_visible_but_read_only(self):
        pvc=self.prepare_volume(managed=False)
        pvc['metadata']['ownerReferences']=[dict(apiVersion='pulsar.songof.dev/v1alpha1',kind='PulsarCluster',name='pulsar',uid='pulsar-uid',controller=True)]
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        snapshot=Cluster(self.store,self.kube).read()
        self.assertEqual(len(snapshot['volumes']),1)
        volume=snapshot['volumes'][0]
        self.assertEqual(volume['management'],'external')
        self.assertFalse(volume['canManage'])
        self.assertEqual(volume['poolId'],'pool-one')
        self.assertEqual(volume['owners'][0]['kind'],'PulsarCluster')
        self.assertFalse(any(method in ('PATCH','POST','DELETE') for method,_,_ in self.kube.calls))

    def test_external_controller_blocks_writes_even_with_managed_label(self):
        pvc=self.prepare_volume(managed=True)
        pvc['metadata']['ownerReferences']=[dict(apiVersion='apps/v1',kind='StatefulSet',name='bookkeeper',uid='sts-uid',controller=True)]
        for action in (lambda:self.cluster.expand('default~data',dict(sizeGiB=2)), lambda:self.cluster.release('default~data')):
            with self.assertRaises(Problem) as error:
                action()
            self.assertEqual(error.exception.status,403)
        self.assertFalse(any(method in ('PATCH','POST','DELETE') for method,_,_ in self.kube.calls))

    def test_external_pvc_requires_verified_diskfleet_class(self):
        pvc=self.prepare_volume(managed=False)
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        key='/apis/storage.k8s.io/v1/storageclasses/diskfleet-pool-one'
        original=copy.deepcopy(self.kube.objects[key])
        mutations=[('owner',None),('driver',None),('reclaim',None),('topology',None)]
        for mutation,_ in mutations:
            with self.subTest(mutation=mutation):
                bad=copy.deepcopy(original)
                if mutation=='owner': bad['metadata']['annotations'][LABEL+'pool-uid']='other-pool'
                if mutation=='driver': bad['provisioner']='other.csi.driver'
                if mutation=='reclaim': bad['reclaimPolicy']='Delete'
                if mutation=='topology': bad['allowedTopologies'][0]['matchLabelExpressions'][0]['values']=['other-node']
                self.kube.objects[key]=bad
                self.assertEqual(Cluster(self.store,self.kube).read()['volumes'],[])

    def test_unrelated_operator_pvc_is_not_included(self):
        pvc=self.prepare_volume(managed=False)
        pvc['spec']['storageClassName']='unrelated-fast-disk'
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        self.assertEqual(Cluster(self.store,self.kube).read()['volumes'],[])

    def test_unhealthy_pool_keeps_external_volume_observable(self):
        pvc=self.prepare_volume(managed=False)
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        self.kube.objects[BASE+'diskpools/pool-one']['status']['phase']='Error'
        snapshot=Cluster(self.store,self.kube).read()
        self.assertEqual(len(snapshot['volumes']),1)
        self.assertFalse(snapshot['volumes'][0]['canManage'])

    def test_diskfleet_owned_pvc_remains_manageable(self):
        pvc=self.prepare_volume(managed=True)
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['management'],'diskfleet')
        self.assertTrue(volume['canManage'])

    def test_pulsar_retained_pvc_without_owner_refs_stays_readonly_even_if_mislabeled(self):
        pvc=self.prepare_volume(managed=True)
        pvc['metadata'].pop('ownerReferences',None)
        pvc['metadata']['labels']['app.kubernetes.io/managed-by']='pulsar-operator'
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertFalse(volume['canManage'])
        for operation in (lambda:self.cluster.expand('default~data',{'sizeGiB':3}),lambda:self.cluster.release('default~data')):
            with self.assertRaises(Problem) as raised: operation()
            self.assertEqual(403,raised.exception.status)

    def test_untrusted_class_does_not_falsely_complete_retention(self):
        pvc=self.prepare_volume()
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-pool-one']['reclaimPolicy']='Delete'
        with self.store.transaction() as con:
            self.store.put(con,'retained',dict(id='retained-pvc-uid',pvcUid='pvc-uid',pvName='pv-data',name='data (retained)',status='ReleaseRequested'))
        self.assertEqual(Cluster(self.store,self.kube).read()['volumes'],[])

    def prepare_fleet_volume(self):
        from diskfleet.fleet import fleet_storage_class
        second=dict(self.pool,id='pool-two',uid='pool-two-uid',name='test-two',node='worker-02',vgName='df_two',storageClass='diskfleet-pool-two',topologyHostname='worker-02')
        self.kube.objects[BASE+'diskpools/pool-two']=dict(metadata=dict(name='pool-two',uid='pool-two-uid'),
            spec=dict(nodeName='worker-02',vgName='df_two',devices=[]),
            status=dict(phase='Ready',storageClass='diskfleet-pool-two',topologyHostname='worker-02',capacityBytes=100*GiB,availableBytes=90*GiB))
        sc=fleet_storage_class('diskfleet-pulsar-journal',[self.pool,second])
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/'+sc['metadata']['name']]=sc
        pvc=self.prepare_volume(managed=False)
        pvc['spec']['storageClassName']=sc['metadata']['name']
        pvc['metadata']['annotations']={'volume.kubernetes.io/selected-node':'worker-02'}
        pv=self.kube.objects['/api/v1/persistentvolumes/pv-data']
        pv['spec']['storageClassName']=sc['metadata']['name']
        pv['spec']['csi']['volumeAttributes']['openebs.io/volgroup']='df_two'
        pv['spec']['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values']=['worker-02']
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        return pvc,sc

    def test_fleet_operator_volume_matches_selected_node_without_adoption(self):
        self.prepare_fleet_volume()
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['poolId'],'pool-two')
        self.assertEqual(volume['node'],'worker-02')
        self.assertEqual(volume['management'],'external')
        self.assertFalse(volume['canManage'])

    def test_fleet_pending_volume_does_not_invent_selected_pool(self):
        pvc,_=self.prepare_fleet_volume()
        pvc['metadata'].pop('annotations')
        pvc['spec'].pop('volumeName')
        pvc['status']['phase']='Pending'
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['poolId'],'')
        self.assertEqual(volume['node'],'')
        self.assertEqual(volume['pool'],'多节点存储类')

    def test_fleet_unhealthy_pool_keeps_volume_observable(self):
        self.prepare_fleet_volume()
        self.kube.objects[BASE+'diskpools/pool-two']['status']['phase']='Error'
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['poolId'],'pool-two')
        self.assertFalse(volume['canManage'])

    def test_fleet_forged_pool_uid_and_widened_patterns_are_not_trusted(self):
        _,sc=self.prepare_fleet_volume()
        path='/apis/storage.k8s.io/v1/storageclasses/'+sc['metadata']['name']
        for mutation in ('uid','pattern','topology'):
            with self.subTest(mutation=mutation):
                bad=copy.deepcopy(sc)
                if mutation=='uid': bad['metadata']['annotations'][LABEL+'pool-uids']=json.dumps({'pool-one':'forged','pool-two':'pool-two-uid'})
                if mutation=='pattern': bad['parameters']['vgpattern']='^df_.*$'
                if mutation=='topology': bad['allowedTopologies'][0]['matchLabelExpressions'][0]['values'].append('worker-unknown')
                self.kube.objects[path]=bad
                self.assertEqual(Cluster(self.store,self.kube).read()['volumes'],[])

    def test_duplicate_live_pool_claim_blocks_plan_commit_without_node_label(self):
        plan=self.plan_new_disk(dict(name='first',node='worker-01',diskIds=['disk-one']))
        self.kube.objects[BASE+'diskpools/manual-claim']=dict(metadata=dict(name='manual-claim',uid='manual-uid'),
            spec=dict(nodeName='worker-01',vgName='df_manual',devices=[dict(path='/dev/disk/by-id/alias',serial='SER001',sizeBytes=100*GiB)]))
        with self.assertRaises(Problem) as error:
            self.cluster.create_pool(plan['planId'],plan['confirmation'])
        self.assertEqual(error.exception.status,409)
        self.assertFalse(any(method=='POST' for method,_,_ in self.kube.calls))

    def test_duplicate_previously_ready_pools_do_not_double_count_capacity(self):
        pvc=self.prepare_volume()
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        original=self.kube.objects[BASE+'diskpools/pool-one']
        duplicate=copy.deepcopy(original)
        duplicate['metadata'].update(name='duplicate',uid='duplicate-uid')
        duplicate['status']['storageClass']='diskfleet-duplicate'
        self.kube.objects[BASE+'diskpools/duplicate']=duplicate
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-duplicate']=storage_class(duplicate)
        current=Cluster(self.store,self.kube)
        snapshot=current.read()
        self.assertEqual([p['state'] for p in snapshot['pools']],['Conflict','Conflict'])
        for field in ('capacityBytes','availableBytes','allocatedBytes'):
            self.assertEqual(sum(p[field] for p in snapshot['pools']),0)
            self.assertEqual(current.overview()['totals'][field],0)
        self.assertEqual(snapshot['volumes'],[])
        self.assertIsNone(snapshot['disks'][0]['poolId'])

    def test_bound_fleet_volume_uses_actual_pv_without_selected_node(self):
        pvc,_=self.prepare_fleet_volume()
        pvc['metadata'].pop('annotations')
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['status'],'Bound')
        self.assertEqual((volume['poolId'],volume['node']),('pool-two','worker-02'))
        self.assertTrue(volume['bindingVerified'])
        pvc['metadata']['annotations']={'volume.kubernetes.io/selected-node':'worker-01'}
        self.assertEqual(Cluster(self.store,self.kube).read()['volumes'][0]['poolId'],'pool-two')

    def test_bound_volume_rejects_forged_claim_driver_vg_and_topology(self):
        pvc,_=self.prepare_fleet_volume()
        pvc['metadata']['labels'][LABEL+'managed']='true'
        path='/api/v1/persistentvolumes/pv-data'
        original=copy.deepcopy(self.kube.objects[path])
        for mutation in ('claim','driver','vg','class','node','unconstrained-or','multiple-nodes'):
            with self.subTest(mutation=mutation):
                pv=copy.deepcopy(original); spec=pv['spec']
                if mutation=='claim': spec['claimRef']['uid']='other-pvc'
                if mutation=='driver': spec['csi']['driver']='other.csi'
                if mutation=='vg': spec['csi']['volumeAttributes']['openebs.io/volgroup']='df_external'
                if mutation=='class': spec['storageClassName']='foreign-class'
                if mutation=='node': spec['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values']=['worker-01']
                if mutation=='unconstrained-or': spec['nodeAffinity']['required']['nodeSelectorTerms'].append({})
                if mutation=='multiple-nodes': spec['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values'].append('worker-01')
                self.kube.objects[path]=pv
                volume=Cluster(self.store,self.kube).read()['volumes'][0]
                self.assertEqual((volume['poolId'],volume['node']),('',''))
                self.assertFalse(volume['bindingVerified'])
                self.assertFalse(volume['canManage'])
                self.assertTrue(volume['bindingMessage'])
                with self.assertRaises(Problem):
                    Cluster(self.store,self.kube).expand('default~data',{'sizeGiB':2})
                self.assertFalse(any(method=='PATCH' for method,_,_ in self.kube.calls))

    def test_bound_pv_hostname_topology_can_differ_from_node_name(self):
        from diskfleet.fleet import fleet_storage_class
        pvc,sc=self.prepare_fleet_volume()
        self.kube.objects[BASE+'diskpools/pool-two']['status']['topologyHostname']='rack-host-two'
        second=dict(self.pool,id='pool-two',uid='pool-two-uid',node='worker-02',vgName='df_two',topologyHostname='rack-host-two')
        updated=fleet_storage_class(sc['metadata']['name'],[self.pool,second])
        self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/'+sc['metadata']['name']]=updated
        expression=self.kube.objects['/api/v1/persistentvolumes/pv-data']['spec']['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]
        expression.update(key='kubernetes.io/hostname',values=['rack-host-two'])
        pvc['metadata'].pop('annotations')
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertTrue(volume['bindingVerified'])
        self.assertEqual((volume['poolId'],volume['node']),('pool-two','worker-02'))

    def lose_delete_response(self):
        request=self.kube.request
        def wrapped(method,path,body=None):
            response=request(method,path,body)
            if method=='DELETE':
                raise KubeError(503,'Response lost after committed deletion')
            return response
        self.kube.request=wrapped

    def test_release_lost_response_recovers_exact_uid_on_retry(self):
        self.prepare_volume()
        self.lose_delete_response()
        with self.assertRaises(KubeError):
            self.cluster.release('default~data')
        self.assertEqual(self.store.list('release-intents')['total'],1)
        result=self.cluster.release('default~data')
        self.assertEqual(result['status'],'Released')
        self.assertEqual(result['pvcUid'],'pvc-uid')
        self.assertEqual(result['pvUid'],'pv-uid')
        self.assertEqual(self.store.list('release-intents')['total'],0)
        self.assertEqual(self.store.list('retained')['total'],1)
        self.assertEqual(len([c for c in self.kube.calls if c[0]=='DELETE']),1)

    def test_release_intent_recovers_during_read_after_process_restart(self):
        pvc=self.prepare_volume()
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        with patch.object(self.cluster,'_finish_release',side_effect=OSError('crash before final local commit')):
            with self.assertRaises(OSError): self.cluster.release('default~data')
        restarted=Cluster(Store(self.temp.name),self.kube)
        volume=restarted.read()['volumes'][0]
        self.assertEqual(volume['status'],'Released')
        self.assertEqual(volume['pvcUid'],'pvc-uid')
        self.assertEqual(self.store.list('release-intents')['total'],0)

    def test_release_retry_does_not_delete_new_same_name_pvc(self):
        original=self.prepare_volume()
        self.lose_delete_response()
        with self.assertRaises(KubeError): self.cluster.release('default~data')
        replacement=copy.deepcopy(original)
        replacement['metadata'].update(uid='new-pvc',resourceVersion='100')
        self.kube.objects['/api/v1/namespaces/default/persistentvolumeclaims/data']=replacement
        result=self.cluster.release('default~data')
        self.assertEqual(result['pvcUid'],'pvc-uid')
        self.assertEqual(self.kube.objects['/api/v1/namespaces/default/persistentvolumeclaims/data']['metadata']['uid'],'new-pvc')
        self.assertEqual(len([c for c in self.kube.calls if c[0]=='DELETE']),1)

    def test_recovered_release_history_cannot_delete_reused_name(self):
        original=self.prepare_volume()
        self.lose_delete_response()
        with self.assertRaises(KubeError): self.cluster.release('default~data',expected_uid='pvc-uid')
        Cluster(Store(self.temp.name),self.kube).read()
        self.assertEqual(self.store.list('release-intents')['total'],0)
        replacement=copy.deepcopy(original)
        replacement['metadata'].update(uid='new-pvc',resourceVersion='100')
        path='/api/v1/namespaces/default/persistentvolumeclaims/data'
        self.kube.objects[path]=replacement
        with self.assertRaises(Problem) as error: self.cluster.release('default~data')
        self.assertEqual(error.exception.status,409)
        result=self.cluster.release('default~data',expected_uid='pvc-uid')
        self.assertEqual(result['pvcUid'],'pvc-uid')
        self.assertEqual(self.kube.objects[path]['metadata']['uid'],'new-pvc')
        self.assertEqual(len([c for c in self.kube.calls if c[0]=='DELETE']),1)

    def test_release_rejects_uid_drift_without_previous_intent(self):
        self.prepare_volume()
        with self.assertRaises(Problem) as error:
            self.cluster.release('default~data',expected_uid='stale-uid')
        self.assertEqual(error.exception.status,409)
        self.assertFalse(any(method=='DELETE' for method,_,_ in self.kube.calls))

    def test_explicit_new_uid_can_release_a_recreated_pvc(self):
        original=self.prepare_volume()
        self.cluster.release('default~data',expected_uid='pvc-uid')
        replacement=copy.deepcopy(original)
        replacement['metadata'].update(uid='new-pvc',resourceVersion='100')
        replacement['spec']['volumeName']='pv-new'
        self.kube.objects['/api/v1/namespaces/default/persistentvolumeclaims/data']=replacement
        pv=copy.deepcopy(self.kube.objects['/api/v1/persistentvolumes/pv-data'])
        pv['metadata'].update(name='pv-new',uid='pv-new-uid')
        pv['spec']['claimRef']['uid']='new-pvc'
        pv['status']['phase']='Bound'
        self.kube.objects['/api/v1/persistentvolumes/pv-new']=pv
        self.data['volumes'][0]['pvcUid']='new-pvc'
        result=self.cluster.release('default~data',expected_uid='new-pvc')
        self.assertEqual(result['pvcUid'],'new-pvc')
        self.assertEqual(self.store.list('retained')['total'],2)
        deletes=[body for method,_,body in self.kube.calls if method=='DELETE']
        self.assertEqual([d['preconditions']['uid'] for d in deletes],['pvc-uid','new-pvc'])

    def test_intent_write_failure_prevents_remote_delete(self):
        self.prepare_volume()
        with patch.object(self.store,'put',side_effect=OSError('disk full')):
            with self.assertRaises(OSError): self.cluster.release('default~data')
        self.assertFalse(any(method=='DELETE' for method,_,_ in self.kube.calls))

    def test_delete_conflict_intent_does_not_falsely_report_release(self):
        pvc=self.prepare_volume()
        self.kube.objects['/api/v1/persistentvolumeclaims/data']=pvc
        self.kube.delete_conflict=True
        with self.assertRaises(KubeError): self.cluster.release('default~data')
        volume=Cluster(self.store,self.kube).read()['volumes'][0]
        self.assertEqual(volume['status'],'Bound')
        self.assertEqual(self.store.list('retained')['total'],0)
        self.assertEqual(self.store.list('release-intents')['total'],1)

    def test_release_rejects_replaced_pv_claim(self):
        self.prepare_volume()
        self.kube.objects['/api/v1/persistentvolumes/pv-data']['spec']['claimRef']['uid']='other-pvc'
        with self.assertRaises(Problem): self.cluster.release('default~data')
        self.assertFalse(any(method=='DELETE' for method,_,_ in self.kube.calls))

    def test_create_volume_does_not_accept_terminating_name_as_success(self):
        pvc=self.prepare_volume()
        pvc['metadata']['deletionTimestamp']='2026-11-13T00:00:00Z'
        with self.assertRaises(Problem) as error:
            self.cluster.create_volume(dict(name='data',namespace='default',poolId='pool-one',sizeGiB=1))
        self.assertEqual(error.exception.status,409)
        self.assertEqual(self.store.events()['total'],0)


if __name__ == '__main__':
    unittest.main()
