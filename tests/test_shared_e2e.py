"""Exercise the real shell preflight with a read-only, isolated fake kubectl.

These tests verify acceptance guards and probe manifests, not a running cluster.
Every mutating command or container exec is rejected by the fake client.
"""
import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'modules/storage'))
from diskfleet.fleet import pulsar_plan


SHARED = 'two-node-shared-disk-lab'
NS = 'diskfleet-pulsar-data-lab'
NAME = 'pulsar-lab'
NODE_LABEL = 'pulsar.songof.dev/two-node'


def fixture(shared=True):
    count = 2 if shared else 3
    pool_data = [dict(id=f'{role}-{i}', uid=f'uid-{role}-{i}', node=f'node-{i}',
                      vgName=f'df_{role}_{i}', topologyHostname=f'host-{i}', state='Ready',
                      availableBytes=100 * 1024**3, capacityBytes=100 * 1024**3)
                 for role in (('data',) if shared else ('journal', 'ledger')) for i in range(count)]
    assignments = {role: [f'{"data" if shared else "ledger" if role == "ledger" else "journal"}-{i}'
                          for i in range(count)] for role in ('zookeeper', 'journal', 'ledger')}
    plan = pulsar_plan(pool_data, assignments, NS, NAME, profile=SHARED if shared else 'three-node-e2e')
    namespace, *classes, psc = plan['items']
    psc['metadata'].update(uid='psc-uid', generation=1)
    psc['status'] = dict(phase='Running', observedGeneration=1)
    nodes = {f'node-{i}': dict(metadata=dict(name=f'node-{i}', labels={
        'kubernetes.io/hostname': f'host-{i}', **({NODE_LABEL: 'true'} if shared else {})}),
        spec={}, status=dict(conditions=[dict(type='Ready', status='True')])) for i in range(count)}
    data = dict(namespace=namespace, psc=psc, classes={s['metadata']['name']: s for s in classes},
                pools={p['id']: dict(metadata=dict(name=p['id'], uid=p['uid']),
                    spec=dict(nodeName=p['node'], vgName=p['vgName']),
                    status=dict(phase='Ready', topologyHostname=p['topologyHostname'])) for p in pool_data},
                nodes=nodes, pods=[], pvcs=[], pvs={})
    roles = dict(zookeeper=psc['spec']['zookeeper']['storage']['storageClassName'],
                 journal=psc['spec']['bookkeeper']['storage']['journal']['storageClassName'],
                 ledgers=psc['spec']['bookkeeper']['storage']['ledger']['storageClassName'])
    for component in ('zookeeper', 'bookie'):
        for i in range(3):
            pod_name = f'{NAME}-{component}-{i}'
            pod = dict(metadata=dict(name=pod_name, labels={'app.kubernetes.io/instance': NAME}),
                       spec=dict(nodeName=f'node-{i % count}', volumes=[]),
                       status=dict(conditions=[dict(type='Ready', status='True')]))
            for role in (('data',) if component == 'zookeeper' else ('journal', 'ledgers')):
                claim, pv = f'{role}-{pod_name}', f'pv-{role}-{component}-{i}'
                pvc = dict(metadata=dict(name=claim, uid='uid-' + claim, labels={
                    'app.kubernetes.io/instance': NAME, 'app.kubernetes.io/managed-by': 'pulsar-operator'}),
                    spec=dict(storageClassName=roles['zookeeper' if role == 'data' else role],
                              accessModes=['ReadWriteOnce'], volumeName=pv), status=dict(phase='Bound'))
                data['pvcs'].append(pvc)
                pod['spec']['volumes'].append(dict(name=role, persistentVolumeClaim=dict(claimName=claim)))
                data['pvs'][pv] = dict(metadata=dict(name=pv, uid='uid-' + pv), spec=dict(
                    csi=dict(driver='local.csi.openebs.io'), persistentVolumeReclaimPolicy='Retain',
                    claimRef=dict(uid=pvc['metadata']['uid']), nodeAffinity=dict(required=dict(nodeSelectorTerms=[
                        dict(matchExpressions=[dict(key='kubernetes.io/hostname', operator='In', values=[f'host-{i % count}'])])]))))
            data['pods'].append(pod)
    return data


FAKE_KUBECTL = r'''
import json, os, sys
args=[a for a in sys.argv[1:] if not a.startswith('--request-timeout=')]
with open(os.environ['FAKE_LOG'], 'a') as log: log.write(json.dumps(args)+'\n')
d=json.load(open(os.environ['FAKE_FIXTURE']))
if args[:2]==['config','current-context']: print('offline-test'); sys.exit(0)
if args[0]=='wait': sys.exit(0)
if args[0]!='get': sys.exit('Forbidden mutation/container exec: '+repr(args))
kind=args[1]
name=args[2] if len(args)>2 and not args[2].startswith('-') else None
if kind=='namespace': obj=d['namespace']
elif kind=='pulsarclusters.pulsar.songof.dev': obj=d['psc']
elif kind=='storageclass': obj=d['classes'][name]
elif kind=='diskpool': obj=d['pools'][name]
elif kind=='nodes': obj={'items':[n for n in d['nodes'].values() if n['metadata']['labels'].get('pulsar.songof.dev/two-node')=='true']}
elif kind=='node': obj=d['nodes'][name]
elif kind=='pvc': obj={'items':d['pvcs']}
elif kind=='pods': obj={'items':d['pods']}
elif kind=='pv': obj=d['pvs'][name]
elif kind in ('psc,bkc,pod,pvc','events'): obj={}
else: sys.exit('Unsupported read: '+repr(args))
print(json.dumps(obj))
'''


class SharedE2EPreflightTests(unittest.TestCase):
    def run_preflight(self, data, shared=True, extra_env=None, args=None):
        with tempfile.TemporaryDirectory() as tmp:
            tmp = Path(tmp)
            binary = tmp / 'kubectl'
            binary.write_text('#!' + sys.executable + '\n' + FAKE_KUBECTL)
            binary.chmod(0o755)
            source, log = tmp / 'fixture.json', tmp / 'calls.jsonl'
            source.write_text(json.dumps(data))
            wrapper = ROOT / 'test/e2e' / ('pulsar-shared-disk.sh' if shared else 'pulsar-storage.sh')
            env = {**os.environ, 'PATH': str(tmp) + os.pathsep + os.environ['PATH'],
                   'TMPDIR': str(tmp), 'FAKE_LOG': str(log), 'FAKE_FIXTURE': str(source),
                   'PREFLIGHT_ONLY': '1', 'TIMEOUT': '60', **(extra_env or {})}
            result = subprocess.run(['bash', str(wrapper), *(args if args is not None else [NS, NAME])],
                                    env=env, cwd='/tmp', capture_output=True, text=True, timeout=30)
            calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            self.assertTrue(all(c[0] in ('config', 'get', 'wait') for c in calls), calls)
            evidence = next(tmp.glob('diskfleet-pulsar-e2e.*'), None)
            probe = json.loads((evidence / 'probe-manifest.json').read_text()) if evidence and (evidence / 'probe-manifest.json').exists() else None
            return result, probe

    def assert_rejected(self, data, message, shared=True):
        result, probe = self.run_preflight(data, shared)
        self.assertNotEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn(message, result.stderr)
        self.assertIsNone(probe)

    def test_shared_profile_passes_nine_pvcs_and_prepares_control_plane_probe(self):
        result, probe = self.run_preflight(fixture())
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn('nine operator-created PVCs', result.stdout)
        self.assertIn('No persistence, expansion, or Retain lifecycle test was executed', result.stdout)
        pvc, pod = probe['items']
        self.assertEqual('1Gi', pvc['spec']['resources']['requests']['storage'])
        self.assertEqual({NODE_LABEL: 'true'}, pod['spec']['nodeSelector'])
        self.assertEqual([dict(key=key, operator='Exists', effect='NoSchedule') for key in (
            'node-role.kubernetes.io/control-plane', 'node-role.kubernetes.io/master')], pod['spec']['tolerations'])
        for obj in probe['items']:
            self.assertEqual(SHARED, obj['metadata']['labels']['storage.diskfleet.io/profile'])
            self.assertEqual('pulsar-two-node-shared-disk-lab', obj['metadata']['labels']['storage.diskfleet.io/integration'])

    def test_default_three_node_mode_still_passes_without_lab_probe_tolerations(self):
        result, probe = self.run_preflight(fixture(False), shared=False, extra_env={'PROFILE': SHARED})
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertIn('replica node separation', result.stdout)
        self.assertNotIn('tolerations', probe['items'][1]['spec'])
        self.assertEqual('pulsar', probe['items'][0]['metadata']['labels']['storage.diskfleet.io/integration'])

    def test_entrypoints_cannot_cross_profile_labels(self):
        self.assert_rejected(fixture(), 'labeled dedicated', shared=False)
        self.assert_rejected(fixture(False), 'labeled dedicated')
        data = fixture(); data['namespace']['metadata']['labels']['storage.diskfleet.io/profile'] = 'two-node-lab'
        self.assert_rejected(data, 'labeled dedicated')

    def test_shared_preferred_allows_all_replicas_on_either_reviewed_node(self):
        data = fixture()
        for pod in data['pods']: pod['spec']['nodeName'] = 'node-0'
        for pv in data['pvs'].values():
            pv['spec']['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values'] = ['host-0']
        result, _ = self.run_preflight(data)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)

    def test_shared_requires_exactly_two_ready_schedulable_nodes(self):
        data = fixture(); data['nodes']['node-2'] = copy.deepcopy(data['nodes']['node-1'])
        data['nodes']['node-2']['metadata'].update(name='node-2', labels={NODE_LABEL: 'true', 'kubernetes.io/hostname': 'host-2'})
        self.assert_rejected(data, 'exactly two')
        for mode in ('not-ready', 'cordoned', 'missing-label'):
            data = fixture(); node = data['nodes']['node-1']
            if mode == 'not-ready': node['status']['conditions'][0]['status'] = 'False'
            elif mode == 'cordoned': node['spec']['unschedulable'] = True
            else: del node['metadata']['labels'][NODE_LABEL]
            with self.subTest(mode=mode): self.assert_rejected(data, 'Shared-disk')

    def test_shared_replica_quorum_placement_and_toleration_guards(self):
        for key in ('replicas', 'minReplicas', 'quorum', 'affinity', 'toleration'):
            data = fixture(); spec = data['psc']['spec']
            if key in ('replicas', 'minReplicas'): spec['bookkeeper'][key] = 2
            elif key == 'quorum': spec['broker']['config']['managedLedgerDefaultAckQuorum'] = '1'
            elif key == 'affinity': spec['zookeeper']['pod']['antiAffinity'] = 'Required'
            else: spec['proxy']['pod']['tolerations'] = []
            with self.subTest(key=key): self.assert_rejected(data, 'Shared-disk lab requires')

    def test_pool_uid_readiness_node_and_exact_vg_guards(self):
        for key in ('uid', 'phase', 'hostname', 'node', 'vg', 'label'):
            data = fixture(); pool = data['pools']['data-0']
            if key == 'uid': pool['metadata']['uid'] = 'recreated'
            elif key == 'phase': pool['status']['phase'] = 'Pending'
            elif key == 'hostname': pool['status']['topologyHostname'] = 'stale'
            elif key == 'node': pool['spec']['nodeName'] = 'node-1'
            elif key == 'vg': next(iter(data['classes'].values()))['parameters']['vgpattern'] = '.*'
            else: next(iter(data['classes'].values()))['metadata']['labels']['storage.diskfleet.io/profile'] = 'three-node-e2e'
            with self.subTest(key=key): self.assert_rejected(data, ':')

    def test_different_role_pool_sets_are_rejected_even_if_each_has_two_nodes(self):
        data = fixture()
        pool = copy.deepcopy(data['pools']['data-0'])
        pool['metadata'].update(name='alternate-0', uid='alternate-uid')
        pool['spec']['vgName'] = 'df_alternate_0'; data['pools']['alternate-0'] = pool
        sc = data['classes'][data['psc']['spec']['bookkeeper']['storage']['ledger']['storageClassName']]
        sc['metadata']['annotations']['storage.diskfleet.io/pool-uids'] = json.dumps({'alternate-0': 'alternate-uid', 'data-1': 'uid-data-1'})
        sc['parameters']['vgpattern'] = '^(df_alternate_0|df_data_1)$'
        self.assert_rejected(data, 'same two DiskPools')

    def test_default_keeps_disjoint_pools_required_affinity_and_spread(self):
        data = fixture(False)
        spec = data['psc']['spec']
        ledger_name = spec['bookkeeper']['storage']['ledger']['storageClassName']
        journal = data['classes'][spec['bookkeeper']['storage']['journal']['storageClassName']]
        data['classes'][ledger_name].update(parameters=journal['parameters'], allowedTopologies=journal['allowedTopologies'])
        data['classes'][ledger_name]['metadata']['annotations'] = journal['metadata']['annotations']
        self.assert_rejected(data, 'disjoint physical', shared=False)
        data = fixture(False); data['psc']['spec']['bookkeeper']['pod']['antiAffinity'] = 'Preferred'
        self.assert_rejected(data, 'antiAffinity=Required', shared=False)
        data = fixture(False)
        for pod in data['pods']: pod['spec']['nodeName'] = 'node-0'
        for pv in data['pvs'].values():
            pv['spec']['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values'] = ['host-0']
        self.assert_rejected(data, 'did not spread', shared=False)

    def test_actual_volume_driver_uid_mount_and_topology_guards_are_retained(self):
        for key in ('driver', 'uid', 'mount', 'topology'):
            data = fixture(); pv = next(iter(data['pvs'].values()))
            if key == 'driver': pv['spec']['csi']['driver'] = 'rancher.io/local-path'
            elif key == 'uid': pv['spec']['claimRef']['uid'] = 'recreated-pvc'
            elif key == 'mount': data['pods'][0]['spec']['volumes'] = []
            else: pv['spec']['nodeAffinity']['required']['nodeSelectorTerms'][0]['matchExpressions'][0]['values'] = ['other-host']
            with self.subTest(key=key): self.assert_rejected(data, ':')

    def test_wrapper_help_and_invalid_profile_do_not_call_kubectl(self):
        result, probe = self.run_preflight(fixture(), args=['--help'])
        self.assertEqual(0, result.returncode)
        self.assertIn('PREFLIGHT_ONLY=1', result.stdout)
        self.assertIsNone(probe)
        result, _ = self.run_preflight(fixture(), shared=False, args=['--profile', 'two-node-lab', NS, NAME])
        self.assertEqual(2, result.returncode)
        self.assertIn('Unsupported E2E profile', result.stderr)


if __name__ == '__main__':
    unittest.main()
