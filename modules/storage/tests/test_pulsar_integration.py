"""Compatibility checks for the observed SongOf 3c042c5 source contract.

These are not executions of the upstream Go controller or a K8s scheduler.
"""
import copy
import importlib.util
import io
import json
import re
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

from diskfleet.fleet import fleet_storage_class, pulsar_plan, validate_two_node_targets
from diskfleet.operator import storage_class
from diskfleet.store import GiB, Problem


MODULE = Path(__file__).resolve().parents[1]


def pool_fixture(nodes=3):
    return [dict(id=f'{role}-{i}',uid=f'uid-{role}-{i}',node=f'worker-{i}',vgName=f'df_{role}_{i}',topologyHostname=f'host-{i}',state='Ready',availableBytes=100*GiB,capacityBytes=100*GiB)
            for role in ('journal','ledger') for i in range(nodes)]


def assignments(nodes=3):
    return dict(zookeeper=[f'journal-{i}' for i in range(nodes)],journal=[f'journal-{i}' for i in range(nodes)],ledger=[f'ledger-{i}' for i in range(nodes)])


def shared_pool_fixture(nodes=2):
    return [p for p in pool_fixture(nodes) if p['id'].startswith('journal-')]


def shared_assignments(nodes=2):
    return {role:[f'journal-{i}' for i in range(nodes)] for role in ('zookeeper','journal','ledger')}


def hosts(sc):
    return set(sc['allowedTopologies'][0]['matchLabelExpressions'][0]['values'])


def live_node_fixture():
    return [dict(metadata=dict(name=f'worker-{i}',labels={'pulsar.songof.dev/two-node':'true','kubernetes.io/hostname':f'host-{i}'}),
                 spec=dict(unschedulable=False),status=dict(conditions=[dict(type='Ready',status='True')])) for i in range(2)]


class PulsarIntegrationTests(unittest.TestCase):
    def plan(self,pools=None,roles=None):
        return pulsar_plan(pools or pool_fixture(),roles or assignments(),'diskfleet-pulsar-test','pulsar')

    def test_original_single_pool_class_cannot_place_three_required_replicas(self):
        p=pool_fixture()[0]
        old=storage_class(dict(metadata=dict(name=p['id'],uid=p['uid']),spec=dict(nodeName=p['node'],vgName=p['vgName'])),p['topologyHostname'])
        self.assertLess(len(hosts(old)),3)

    def test_actual_cr_fields_and_separate_bookkeeper_classes(self):
        plan=self.plan(); psc=plan['items'][-1]
        self.assertEqual('pulsar.songof.dev/v1alpha1',psc['apiVersion'])
        self.assertEqual('PulsarCluster',psc['kind'])
        spec=psc['spec']; scs={x['metadata']['name']:x for x in plan['items'] if x['kind']=='StorageClass'}
        journal=scs[spec['bookkeeper']['storage']['journal']['storageClassName']]
        ledger=scs[spec['bookkeeper']['storage']['ledger']['storageClassName']]
        self.assertEqual(3,len(hosts(journal)&hosts(ledger)))
        self.assertNotEqual(journal['parameters']['vgpattern'],ledger['parameters']['vgpattern'])
        self.assertEqual('Required',spec['bookkeeper']['pod']['antiAffinity'])
        self.assertEqual('Retain',spec['bookkeeper']['storage']['reclaimPolicy'])
        self.assertEqual('20Gi',spec['bookkeeper']['storage']['ledger']['size'])
        self.assertIn(spec['zookeeper']['storage']['storageClassName'],scs)
        self.assertFalse(spec['monitoring']['enabled'])

    def test_class_selects_exact_reviewed_vgs_only(self):
        pools=pool_fixture()[::2]
        sc=fleet_storage_class('fleet-test',pools)
        pattern=sc['parameters']['vgpattern']
        for p in pools: self.assertIsNotNone(re.fullmatch(pattern,p['vgName']))
        for alien in ('df_external','prefix'+pools[0]['vgName'],pools[0]['vgName']+'suffix'):
            self.assertIsNone(re.fullmatch(pattern,alien))
        self.assertEqual({p['id']:p['uid'] for p in pools},json.loads(sc['metadata']['annotations']['storage.diskfleet.io/pool-uids']))
        self.assertEqual('WaitForFirstConsumer',sc['volumeBindingMode'])
        self.assertEqual('Retain',sc['reclaimPolicy'])
        self.assertEqual('no',sc['parameters']['thinProvision'])

    def test_fewer_nodes_and_empty_multi_pvc_intersection_are_rejected(self):
        roles=assignments(); roles['journal']=roles['journal'][:2]
        with self.assertRaises(Problem): self.plan(roles=roles)
        pools=pool_fixture(); pools[-1]['node']='worker-9'; pools[-1]['topologyHostname']='host-9'
        with self.assertRaises(Problem): self.plan(pools=pools)

    def test_journal_and_ledger_cannot_share_physical_pools(self):
        roles=assignments(); roles['ledger']=roles['journal']
        with self.assertRaises(Problem): self.plan(roles=roles)

    def test_shared_zookeeper_capacity_and_e2e_headroom_are_accounted(self):
        pools=pool_fixture(); pools[0]['availableBytes']=9*GiB
        with self.assertRaises(Problem): self.plan(pools=pools)
        pools=pool_fixture(); pools[3]['availableBytes']=21*GiB
        with self.assertRaises(Problem): self.plan(pools=pools)

    def test_nonready_pool_and_missing_identity_cannot_enter_class(self):
        for field,value in [('state','Error'),('uid',''),('topologyHostname',''),('vgName','df_foo|.*')]:
            pools=pool_fixture(); pools[0][field]=value
            with self.subTest(field=field),self.assertRaises(Problem):
                self.plan(pools=pools)

    def test_cross_namespace_class_names_do_not_collide(self):
        a=self.plan()['items'][1]['metadata']['name']
        b=pulsar_plan(pool_fixture(),assignments(),'diskfleet-pulsar-other','pulsar')['items'][1]['metadata']['name']
        self.assertNotEqual(a,b)

    def test_cross_node_vg_name_collision_cannot_cross_storage_roles(self):
        pools=pool_fixture()
        pools[4]['vgName']=pools[0]['vgName']
        with self.assertRaises(Problem): self.plan(pools=pools)

    def test_namespace_scope_and_replica_guards(self):
        with self.assertRaises(Problem): pulsar_plan(pool_fixture(),assignments(),'production','pulsar')
        with self.assertRaises(Problem): pulsar_plan(pool_fixture(),assignments(),'diskfleet-pulsar-test','pulsar',replicas=1)
        with self.assertRaises(Problem): pulsar_plan(pool_fixture(),assignments(),'diskfleet-pulsar-test','p'*41)

    def test_default_profile_preserves_published_three_node_manifest(self):
        pools=json.loads((MODULE/'integrations/pulsar/pools.example.json').read_text())
        expected=json.loads((MODULE/'integrations/pulsar/manifest.example.json').read_text())
        psc=expected['items'][-1]
        actual=pulsar_plan(pools,assignments(),psc['metadata']['namespace'],psc['metadata']['name'])
        self.assertEqual(expected,actual)
        self.assertEqual(actual,pulsar_plan(pools,assignments(),psc['metadata']['namespace'],psc['metadata']['name'],profile='three-node-e2e'))


class TwoNodePulsarLabTests(unittest.TestCase):
    def plan(self,pools=None,roles=None,**options):
        return pulsar_plan(pool_fixture(2) if pools is None else pools,assignments(2) if roles is None else roles,
                           'diskfleet-pulsar-lab','pulsar',profile='two-node-lab',**options)

    def test_lab_preserves_quorum_and_marks_limits(self):
        plan=self.plan(); psc=plan['items'][-1]; spec=psc['spec']
        self.assertEqual(3,spec['zookeeper']['replicas'])
        self.assertEqual(3,spec['bookkeeper']['replicas'])
        self.assertEqual(3,spec['bookkeeper']['minReplicas'])
        self.assertEqual(2,spec['broker']['replicas'])
        for component in ('zookeeper','bookkeeper'):
            self.assertEqual('Preferred',spec[component]['pod']['antiAffinity'])
        for key in ('managedLedgerDefaultEnsembleSize','managedLedgerDefaultWriteQuorum','managedLedgerDefaultAckQuorum'):
            self.assertEqual('2',spec['broker']['config'][key])
        for item in plan['items']:
            self.assertEqual('two-node-lab',item['metadata']['labels']['storage.diskfleet.io/profile'])
            self.assertNotEqual('pulsar',item['metadata']['labels']['storage.diskfleet.io/integration'])
        self.assertEqual('three-node-e2e-not-applicable',psc['metadata']['annotations']['storage.diskfleet.io/validation'])
        self.assertEqual('lab-only-not-host-failure-ha',psc['metadata']['annotations']['storage.diskfleet.io/availability'])

    def test_every_component_uses_reviewed_nodes_and_control_plane_tolerations(self):
        spec=self.plan()['items'][-1]['spec']
        for component in [spec['zookeeper'],spec['bookkeeper'],spec['broker'],spec['proxy'],spec['bookkeeper']['autoRecovery']]:
            self.assertEqual({'pulsar.songof.dev/two-node':'true'},component['pod']['nodeSelector'])
            self.assertEqual({'node-role.kubernetes.io/control-plane','node-role.kubernetes.io/master'},
                             {t['key'] for t in component['pod']['tolerations']})
            self.assertTrue(all(t['effect']=='NoSchedule' and t['operator']=='Exists' for t in component['pod']['tolerations']))
            self.assertTrue(component['resources']['requests']['memory'])
            self.assertTrue(component['resources']['requests']['cpu'])

    def test_conservative_capacity_accepts_all_replicas_on_either_host(self):
        pools=pool_fixture(2)
        for p in pools:
            p['availableBytes']=(30 if p['id'].startswith('journal-') else 64)*GiB
        self.plan(pools=pools)
        # 2+1 is not enforced by Preferred: all three may land on one host.
        # A pool just one byte below the full skew budget must be rejected.
        for i,p in enumerate(pools):
            insufficient=copy.deepcopy(pools); insufficient[i]['availableBytes']-=1
            with self.subTest(pool=p['id']),self.assertRaisesRegex(Problem,'容量不足'):
                self.plan(pools=insufficient)

    def test_exactly_two_matching_hosts_and_three_replicas_are_required(self):
        for nodes in (1,3):
            with self.subTest(nodes=nodes),self.assertRaisesRegex(Problem,'恰好覆盖 2'):
                self.plan(pools=pool_fixture(nodes),roles=assignments(nodes))
        pools=pool_fixture(2); pools[-1].update(node='worker-9',topologyHostname='host-9')
        with self.assertRaisesRegex(Problem,'覆盖相同节点'): self.plan(pools=pools)
        for replicas in (1,2,4):
            with self.subTest(replicas=replicas),self.assertRaises(Problem): self.plan(replicas=replicas)
        with self.assertRaisesRegex(Problem,'不支持的 Pulsar profile'):
            pulsar_plan(pool_fixture(),assignments(),'diskfleet-pulsar-test','pulsar',profile='two-node')

    def test_lab_cannot_weaken_pool_identity_or_vg_allowlist(self):
        for field,value in [('state','Pending'),('uid',''),('node',''),('topologyHostname',''),('vgName','df_foo|.*')]:
            pools=pool_fixture(2); pools[0][field]=value
            with self.subTest(field=field),self.assertRaises(Problem): self.plan(pools=pools)
        pools=pool_fixture(2); pools[-1]['vgName']=pools[0]['vgName']
        with self.assertRaisesRegex(Problem,'VG 名称跨节点冲突'): self.plan(pools=pools)
        roles=assignments(2); roles['ledger']=roles['journal']
        with self.assertRaisesRegex(Problem,'不同物理盘'): self.plan(roles=roles)
        plan=self.plan(); pools={p['id']:p for p in pool_fixture(2)}
        for sc in plan['items'][1:-1]:
            self.assertEqual({'host-0','host-1'},hosts(sc))
            owners=json.loads(sc['metadata']['annotations']['storage.diskfleet.io/pool-uids'])
            self.assertEqual({pid:pools[pid]['uid'] for pid in owners},owners)
            for pid in owners: self.assertIsNotNone(re.fullmatch(sc['parameters']['vgpattern'],pools[pid]['vgName']))
            self.assertIsNone(re.fullmatch(sc['parameters']['vgpattern'],'df_unreviewed'))
            self.assertEqual('Retain',sc['reclaimPolicy'])
            self.assertEqual('WaitForFirstConsumer',sc['volumeBindingMode'])
            self.assertEqual('no',sc['parameters']['thinProvision'])

    def test_lab_class_names_cannot_replace_default_profile_classes(self):
        default=pulsar_plan(pool_fixture(),assignments(),'diskfleet-pulsar-lab','pulsar')
        self.assertNotEqual(default['items'][1]['metadata']['name'],self.plan()['items'][1]['metadata']['name'])

    def test_live_targets_require_exactly_two_ready_schedulable_labeled_nodes(self):
        validate_two_node_targets(pool_fixture(2),assignments(2),live_node_fixture())
        for state in ('unlabeled','third-labeled-node','not-ready','unknown','cordoned','terminating','missing-hostname'):
            nodes=live_node_fixture()
            if state=='unlabeled': nodes[0]['metadata']['labels'].pop('pulsar.songof.dev/two-node')
            elif state=='third-labeled-node':
                third=copy.deepcopy(nodes[0]); third['metadata']['name']='worker-3'; nodes.append(third)
            elif state=='not-ready': nodes[0]['status']['conditions'][0]['status']='False'
            elif state=='unknown': nodes[0]['status']['conditions']=[]
            elif state=='cordoned': nodes[0]['spec']['unschedulable']=True
            elif state=='terminating': nodes[0]['metadata']['deletionTimestamp']='2026-11-13T00:00:00Z'
            elif state=='missing-hostname': nodes[0]['metadata']['labels'].pop('kubernetes.io/hostname')
            with self.subTest(state=state),self.assertRaises(Problem):
                validate_two_node_targets(pool_fixture(2),assignments(2),nodes)

    def test_live_targets_reject_another_node_pair_and_stale_hostnames(self):
        pools=pool_fixture(2)
        for pool in pools: pool['node']='other-'+pool['node']
        # These pools are internally consistent and Ready; only a live Node
        # check detects that the deployment selected a different host pair.
        self.plan(pools=pools)
        with self.assertRaisesRegex(Problem,'已标记的两个 Ready 节点完全相同'):
            validate_two_node_targets(pools,assignments(2),live_node_fixture())
        pools=pool_fixture(2); pools[0]['topologyHostname']='stale-hostname'
        with self.assertRaisesRegex(Problem,'实时节点标签不一致'):
            validate_two_node_targets(pools,assignments(2),live_node_fixture())

    def test_live_cli_checks_nodes_before_writing_output(self):
        module_spec=importlib.util.spec_from_file_location('prepare_pulsar_under_test',MODULE/'scripts/prepare-pulsar.py')
        cli=importlib.util.module_from_spec(module_spec); module_spec.loader.exec_module(cli)
        for matching in (False,True):
            with self.subTest(matching=matching),tempfile.TemporaryDirectory() as tmp:
                target=Path(tmp)/'live.json'
                args=['prepare-pulsar','--zookeeper-pools','journal-0,journal-1','--journal-pools','journal-0,journal-1',
                      '--ledger-pools','ledger-0,ledger-1','--namespace','diskfleet-pulsar-lab',
                      '--profile','two-node-lab','--output',str(target)]
                nodes=live_node_fixture()
                if not matching: nodes[0]['metadata']['name']='another-worker'
                stdout=io.StringIO(); stderr=io.StringIO()
                with patch.object(cli,'KubeClient') as client,patch.object(cli,'Cluster') as cluster,patch.object(sys,'argv',args),redirect_stdout(stdout),redirect_stderr(stderr):
                    cluster.return_value.read.return_value={'pools':pool_fixture(2)}
                    client.return_value.list.return_value=nodes
                    if matching: cli.main()
                    else:
                        with self.assertRaises(SystemExit): cli.main()
                    client.return_value.list.assert_called_once_with('/api/v1/nodes',label_selector='pulsar.songof.dev/two-node=true')
                self.assertEqual(matching,target.exists())
                if matching:
                    self.assertEqual('live-read-only',json.loads(stdout.getvalue())['mode'])
                else: self.assertIn('已标记的两个 Ready 节点完全相同',stderr.getvalue())

    def test_offline_cli_is_explicit_and_does_not_overwrite_reviewed_output(self):
        fixture=MODULE/'integrations/pulsar/two-node-pools.example.json'
        data=json.loads(fixture.read_text())
        self.assertTrue(data['exampleOnly'])
        self.assertTrue(all(p['uid'].startswith('EXAMPLE-ONLY-UID-') for p in data['items']))
        with tempfile.TemporaryDirectory() as tmp:
            target=Path(tmp)/'lab.json'
            args=[sys.executable,str(MODULE/'scripts/prepare-pulsar.py'),
                  '--pools-json',str(fixture),'--zookeeper-pools','journal-lab-a,journal-lab-b',
                  '--journal-pools','journal-lab-a,journal-lab-b','--ledger-pools','ledger-lab-a,ledger-lab-b',
                  '--namespace','diskfleet-pulsar-lab','--profile','two-node-lab','--output',str(target)]
            run=subprocess.run(args,text=True,capture_output=True)
            self.assertEqual(0,run.returncode,run.stderr)
            result=json.loads(run.stdout)
            self.assertEqual('offline-input',result['mode'])
            self.assertEqual('two-node-lab',result['profile'])
            self.assertFalse(result['applied'])
            self.assertIn('不适用原三节点 E2E',run.stderr)
            self.assertIn('离线输入仅供参考',run.stderr)
            original=target.read_bytes()
            rerun=subprocess.run(args,text=True,capture_output=True)
            self.assertNotEqual(0,rerun.returncode)
            self.assertIn('输出文件已存在',rerun.stderr)
            self.assertEqual(original,target.read_bytes())


class TwoNodeSharedDiskLabTests(unittest.TestCase):
    def plan(self,pools=None,roles=None,**options):
        return pulsar_plan(shared_pool_fixture() if pools is None else pools,
                           shared_assignments() if roles is None else roles,
                           'diskfleet-pulsar-data-lab','pulsar-lab',
                           profile='two-node-shared-disk-lab',**options)

    def test_100gib_disks_with_metadata_overhead_fit_all_three_roles(self):
        pools=shared_pool_fixture()
        for p in pools: p['availableBytes']=100*GiB-4*1024*1024
        plan=self.plan(pools=pools)
        scs=plan['items'][1:-1]
        self.assertEqual(3,len(scs))
        self.assertEqual(3,len({sc['metadata']['name'] for sc in scs}))
        self.assertEqual(1,len({sc['parameters']['vgpattern'] for sc in scs}))
        owners={p['id']:p['uid'] for p in pools}
        for sc in scs:
            self.assertEqual(owners,json.loads(sc['metadata']['annotations']['storage.diskfleet.io/pool-uids']))
            self.assertEqual({'host-0','host-1'},hosts(sc))
            self.assertEqual('local.csi.openebs.io',sc['provisioner'])
            self.assertEqual('Retain',sc['reclaimPolicy'])
            self.assertEqual('WaitForFirstConsumer',sc['volumeBindingMode'])
            self.assertTrue(sc['allowVolumeExpansion'])
            self.assertEqual('no',sc['parameters']['thinProvision'])

    def test_each_pool_needs_94gib_actual_free_not_total_capacity(self):
        pools=shared_pool_fixture()
        for p in pools: p['availableBytes']=94*GiB
        self.plan(pools=pools)
        for free in (93*GiB,94*GiB-1):
            for index in range(2):
                insufficient=copy.deepcopy(pools)
                insufficient[index]['availableBytes']=free
                with self.subTest(free=free,node=index),self.assertRaisesRegex(Problem,'需要 94 GiB'):
                    self.plan(pools=insufficient)

    def test_shared_roles_must_select_the_same_two_pools(self):
        reversed_order=shared_assignments(); reversed_order['ledger'].reverse()
        self.plan(roles=reversed_order)
        for role in ('zookeeper','journal','ledger'):
            roles=shared_assignments(); roles[role]=['ledger-0','ledger-1']
            with self.subTest(role=role),self.assertRaisesRegex(Problem,'必须选择同一组 2 个 Ready 池'):
                self.plan(pools=pool_fixture(2),roles=roles)
        for count in (1,3):
            with self.subTest(count=count),self.assertRaisesRegex(Problem,'恰好覆盖 2'):
                self.plan(pools=shared_pool_fixture(count),roles=shared_assignments(count))
        with self.assertRaisesRegex(Problem,'每个节点只能选择一个池'):
            pools=shared_pool_fixture(); pools[1]['node']=pools[0]['node']; self.plan(pools=pools)
        with self.assertRaises(Problem): self.plan(replicas=4)

    def test_other_profiles_still_reject_shared_journal_and_ledger(self):
        for profile,count in [('three-node-e2e',3),('two-node-lab',2)]:
            with self.subTest(profile=profile),self.assertRaisesRegex(Problem,'不同物理盘'):
                pulsar_plan(shared_pool_fixture(count),shared_assignments(count),
                            'diskfleet-pulsar-data-lab','pulsar-lab',profile=profile)

    def test_shared_lab_retains_components_resources_quorum_and_explicit_limits(self):
        plan=self.plan(); psc=plan['items'][-1]; spec=psc['spec']
        reference=pulsar_plan(pool_fixture(2),assignments(2),'diskfleet-pulsar-data-lab','pulsar-lab',profile='two-node-lab')['items'][-1]['spec']
        # Only role StorageClass references differ from the original two-node
        # workload spec; resource requests and quorum must remain unchanged.
        expected=copy.deepcopy(reference)
        expected['zookeeper']['storage']['storageClassName']=spec['zookeeper']['storage']['storageClassName']
        for role in ('journal','ledger'):
            expected['bookkeeper']['storage'][role]['storageClassName']=spec['bookkeeper']['storage'][role]['storageClassName']
        self.assertEqual(expected,spec)
        for item in plan['items']:
            self.assertEqual('two-node-shared-disk-lab',item['metadata']['labels']['storage.diskfleet.io/profile'])
            self.assertEqual('pulsar-two-node-shared-disk-lab',item['metadata']['labels']['storage.diskfleet.io/integration'])
        notes=psc['metadata']['annotations']
        self.assertEqual('three-node-e2e-not-applicable',notes['storage.diskfleet.io/validation'])
        self.assertEqual('lab-only-not-host-failure-ha',notes['storage.diskfleet.io/availability'])
        self.assertIn('no disk or IO isolation',notes['storage.diskfleet.io/storage-layout'])
        self.assertIn('94Gi',notes['storage.diskfleet.io/capacity-preflight'])

    def test_shared_profile_cannot_replace_separate_disk_classes(self):
        old=pulsar_plan(pool_fixture(2),assignments(2),'diskfleet-pulsar-data-lab','pulsar-lab',profile='two-node-lab')
        old_names={sc['metadata']['name'] for sc in old['items'][1:-1]}
        new_names={sc['metadata']['name'] for sc in self.plan()['items'][1:-1]}
        self.assertFalse(old_names&new_names)

    def test_shared_profile_preserves_identity_and_vg_membership_guards(self):
        for field,value in [('state','Pending'),('uid',''),('node',''),('topologyHostname',''),('vgName','df_bad|.*')]:
            pools=shared_pool_fixture(); pools[0][field]=value
            with self.subTest(field=field),self.assertRaises(Problem): self.plan(pools=pools)
        pools=shared_pool_fixture()
        unreviewed=copy.deepcopy(pools[0]); unreviewed['id']='unreviewed'; unreviewed['uid']='alien-uid'
        with self.assertRaisesRegex(Problem,'VG 名称跨节点冲突'): self.plan(pools=pools+[unreviewed])
        validate_two_node_targets(shared_pool_fixture(),shared_assignments(),live_node_fixture(),profile='two-node-shared-disk-lab')
        nodes=live_node_fixture(); nodes[0]['metadata']['name']='unselected-node'
        with self.assertRaisesRegex(Problem,'已标记的两个 Ready 节点完全相同'):
            validate_two_node_targets(shared_pool_fixture(),shared_assignments(),nodes,profile='two-node-shared-disk-lab')

    def test_shared_live_cli_checks_nodes_before_writing_output(self):
        module_spec=importlib.util.spec_from_file_location('prepare_shared_pulsar_under_test',MODULE/'scripts/prepare-pulsar.py')
        cli=importlib.util.module_from_spec(module_spec); module_spec.loader.exec_module(cli)
        for matching in (False,True):
            with self.subTest(matching=matching),tempfile.TemporaryDirectory() as tmp:
                target=Path(tmp)/'live.json'
                args=['prepare-pulsar','--zookeeper-pools','journal-0,journal-1','--journal-pools','journal-0,journal-1',
                      '--ledger-pools','journal-0,journal-1','--namespace','diskfleet-pulsar-data-lab',
                      '--profile','two-node-shared-disk-lab','--output',str(target)]
                nodes=live_node_fixture()
                if not matching: nodes[0]['spec']['unschedulable']=True
                stdout=io.StringIO(); stderr=io.StringIO()
                with patch.object(cli,'KubeClient') as client,patch.object(cli,'Cluster') as cluster,patch.object(sys,'argv',args),redirect_stdout(stdout),redirect_stderr(stderr):
                    cluster.return_value.read.return_value={'pools':shared_pool_fixture()}
                    client.return_value.list.return_value=nodes
                    if matching: cli.main()
                    else:
                        with self.assertRaises(SystemExit): cli.main()
                    client.return_value.list.assert_called_once_with('/api/v1/nodes',label_selector='pulsar.songof.dev/two-node=true')
                self.assertEqual(matching,target.exists())
                if matching:
                    self.assertEqual('live-read-only',json.loads(stdout.getvalue())['mode'])
                    self.assertIn('94Gi',stderr.getvalue())
                else: self.assertIn('必须 Ready、未封锁且未删除',stderr.getvalue())

    def test_shared_offline_fixture_is_explicit_and_output_is_not_overwritten(self):
        fixture=MODULE/'integrations/pulsar/shared-disk-pools.example.json'
        data=json.loads(fixture.read_text())
        self.assertTrue(data['exampleOnly'])
        self.assertTrue(all(p['uid'].startswith('EXAMPLE-ONLY-UID-') for p in data['items']))
        with tempfile.TemporaryDirectory() as tmp:
            target=Path(tmp)/'shared.json'
            args=[sys.executable,str(MODULE/'scripts/prepare-pulsar.py'),'--pools-json',str(fixture),
                  '--zookeeper-pools','shared-lab-a,shared-lab-b','--journal-pools','shared-lab-a,shared-lab-b',
                  '--ledger-pools','shared-lab-a,shared-lab-b','--namespace','diskfleet-pulsar-data-lab',
                  '--profile','two-node-shared-disk-lab','--output',str(target)]
            run=subprocess.run(args,text=True,capture_output=True)
            self.assertEqual(0,run.returncode,run.stderr)
            report=json.loads(run.stdout)
            self.assertEqual('two-node-shared-disk-lab',report['profile'])
            self.assertEqual('offline-input',report['mode'])
            self.assertFalse(report['applied'])
            for warning in ('94Gi','共用每节点同一块数据盘','不适用原三节点 E2E','离线输入仅供参考'):
                self.assertIn(warning,run.stderr)
            original=target.read_bytes()
            rerun=subprocess.run(args,text=True,capture_output=True)
            self.assertNotEqual(0,rerun.returncode)
            self.assertIn('输出文件已存在',rerun.stderr)
            self.assertEqual(original,target.read_bytes())


if __name__=='__main__': unittest.main()
