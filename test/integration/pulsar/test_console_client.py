"""Run real shell entrypoints with isolated HTTP/Kubernetes stand-ins."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
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
                     'MODE': mode, 'CLIENT': str(ROOT / 'test/e2e/lib/console-client.sh'),
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


if __name__ == '__main__':
    unittest.main()
