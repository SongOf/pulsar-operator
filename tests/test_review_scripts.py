"""Run real shell entrypoints with isolated HTTP/Kubernetes stand-ins."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
TOKEN = 'fake-review-token-never-a-real-secret'


class ConsoleClientTests(unittest.TestCase):
    def run_client(self, mode='normal', token=TOKEN):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'token').write_text(token)
            (root / 'curl').write_text('#!' + sys.executable + '\n' + r'''
import json,os,sys
from pathlib import Path
p=Path(os.environ['CALL_LOG'])
calls=[json.loads(x) for x in p.read_text().splitlines()] if p.exists() else []
args=sys.argv[1:]; auth=sys.stdin.read()
with p.open('a') as f: f.write(json.dumps({'args':args,'auth':auth})+'\n')
if os.environ['MODE']=='unauthorized': sys.exit(22)
if '--request' not in args:
    uid='replaced' if os.environ['MODE']=='replacement' and calls else 'original'
    print(json.dumps({'metadata':{'uid':uid,'resourceVersion':str(len(calls)+10)}}))
''')
            (root / 'curl').chmod(0o755)
            command = 'set -euo pipefail; source "$CLIENT"; console_init; console_patch \'{"broker":{"replicas":2}}\''
            result = subprocess.run(['bash', '-c', command], capture_output=True, text=True,
                env={**os.environ, 'PATH': tmp + os.pathsep + os.environ['PATH'],
                     'CONSOLE_TOKEN_FILE': str(root / 'token'), 'CALL_LOG': str(root / 'calls'),
                     'MODE': mode, 'CLIENT': str(ROOT / 'test/e2e/console-client.sh'),
                     'NS': 'test-ns', 'NAME': 'test-cluster', 'API': 'http://console.test/api'}, timeout=10)
            calls = [json.loads(x) for x in (root / 'calls').read_text().splitlines()] if (root / 'calls').exists() else []
            self.assertNotIn(TOKEN, result.stdout + result.stderr)
            self.assertTrue(all(TOKEN not in ' '.join(c['args']) for c in calls))
            return result, calls

    def test_authenticated_patch_uses_fresh_version_and_pinned_uid(self):
        result, calls = self.run_client()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertEqual(3, len(calls))
        self.assertTrue(all(c['auth'] == f'Authorization: Bearer {TOKEN}\n' for c in calls))
        self.assertIn('X-Resource-UID: original', calls[-1]['args'])
        self.assertIn('X-Resource-Version: 11', calls[-1]['args'])
        self.assertIn('{"spec":{"broker":{"replicas":2}}}', calls[-1]['args'])

    def test_replacement_is_not_patched(self):
        result, calls = self.run_client('replacement')
        self.assertNotEqual(0, result.returncode)
        self.assertIn('replaced', result.stderr)
        self.assertEqual(2, len(calls))
        self.assertTrue(all('--request' not in c['args'] for c in calls))

    def test_authentication_failure_stops_before_write(self):
        result, calls = self.run_client('unauthorized')
        self.assertNotEqual(0, result.returncode)
        self.assertEqual(1, len(calls))

    def test_missing_token_stops_before_http(self):
        result, calls = self.run_client(token='')
        self.assertNotEqual(0, result.returncode)
        self.assertEqual([], calls)


class StoragePreflightTests(unittest.TestCase):
    def fixture(self):
        return dict(sc=dict(metadata=dict(name='diskfleet-test', labels={'storage.diskfleet.io/pool': 'hashed-name'},
            annotations={'storage.diskfleet.io/pool-uid': 'pool-uid'}), provisioner='local.csi.openebs.io',
            reclaimPolicy='Retain', volumeBindingMode='WaitForFirstConsumer', allowVolumeExpansion=True,
            parameters=dict(thinProvision='no', vgpattern='^df_test$'), allowedTopologies=[dict(
                matchLabelExpressions=[dict(key='kubernetes.io/hostname', values=['host-a'])])]),
            pool=dict(metadata=dict(name='long-pool-'+'a'*70, uid='pool-uid'),
                spec=dict(nodeName='node-a', vgName='df_test', approved=True),
                status=dict(phase='Ready', storageClass='diskfleet-test', topologyHostname='host-a', availableBytes=10*1024**3)),
            node=dict(metadata=dict(name='node-a', labels={'kubernetes.io/hostname':'host-a'}),
                spec=dict(taints=[dict(key='node-role.kubernetes.io/control-plane', effect='NoSchedule')]),
                status=dict(conditions=[dict(type='Ready', status='True')])))

    def run_preflight(self, data):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp); (root/'fixture').write_text(json.dumps(data))
            (root/'kubectl').write_text('#!'+sys.executable+'\n'+r'''
import json,os,sys
a=[x for x in sys.argv[1:] if not x.startswith('--request-timeout=')]
with open(os.environ['CALL_LOG'],'a') as f: f.write(json.dumps(a)+'\n')
d=json.load(open(os.environ['FIXTURE']))
if a==['config','current-context']: print('offline'); sys.exit(0)
if a[0]!='get': sys.exit('Unexpected mutation: '+repr(a))
if a[1]=='storageclass': obj=d['sc']
elif a[1]=='diskpools': obj={'items':[d['pool']]}
elif a[1]=='node': obj=d['node']
else: sys.exit('Unexpected lookup: '+repr(a))
print(json.dumps(obj))
''')
            (root/'kubectl').chmod(0o755)
            result=subprocess.run(['bash', str(ROOT/'modules/storage/scripts/e2e-k8s.sh'), 'diskfleet-test'],
                env={**os.environ, 'PATH':tmp+os.pathsep+os.environ['PATH'], 'TMPDIR':tmp,
                     'FIXTURE':str(root/'fixture'), 'CALL_LOG':str(root/'calls'), 'PREFLIGHT_ONLY':'1'},
                capture_output=True, text=True, timeout=15)
            calls=[json.loads(x) for x in (root/'calls').read_text().splitlines()]
            self.assertTrue(all(c[0] in ('get','config') for c in calls), calls)
            return result

    def test_long_name_resolves_by_uid_and_control_plane_is_schedulable(self):
        result=self.run_preflight(self.fixture())
        self.assertEqual(0, result.returncode, result.stdout+result.stderr)
        scheduling=json.loads(result.stdout.splitlines()[-1])
        self.assertEqual({'kubernetes.io/hostname':'host-a'}, scheduling['nodeSelector'])
        self.assertEqual([dict(key='node-role.kubernetes.io/control-plane', operator='Exists', effect='NoSchedule')], scheduling['tolerations'])

    def test_uid_topology_and_node_guards_fail_before_allocation(self):
        for fault in ('uid','hostname','not-ready','cordon','taint','deleting','vg'):
            with self.subTest(fault=fault):
                data=self.fixture()
                if fault=='uid': data['pool']['metadata']['uid']='replacement'
                elif fault=='hostname': data['node']['metadata']['labels']['kubernetes.io/hostname']='other'
                elif fault=='not-ready': data['node']['status']['conditions'][0]['status']='False'
                elif fault=='cordon': data['node']['spec']['unschedulable']=True
                elif fault=='taint': data['node']['spec']['taints'][0]['key']='dedicated'
                elif fault=='deleting': data['pool']['metadata']['deletionTimestamp']='now'
                else: data['sc']['parameters']['vgpattern']='.*'
                result=self.run_preflight(data)
                self.assertNotEqual(0, result.returncode, result.stdout+result.stderr)


if __name__ == '__main__':
    unittest.main()
