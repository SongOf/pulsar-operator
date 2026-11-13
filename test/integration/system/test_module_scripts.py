"""Exercise module entrypoints from another cwd with isolated command stand-ins."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]


class ModuleScriptTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='module scripts ')
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.log = self.directory / 'commands.jsonl'
        for name in ('go', 'npm', 'node', 'python-check', 'kubectl', 'docker', 'helm', 'kube-apiserver', 'etcd'):
            executable = self.directory / name
            executable.write_text('#!' + sys.executable + '\n' + '''
import json, os, sys
with open(os.environ['COMMAND_LOG'], 'a') as log:
    log.write(json.dumps(dict(tool=os.path.basename(sys.argv[0]), args=sys.argv[1:],
        cwd=os.getcwd(), existing=os.environ.get('USE_EXISTING_CLUSTER'),
        assets=os.environ.get('KUBEBUILDER_ASSETS'))) + '\\n')
''')
            executable.chmod(0o755)
        self.env = {**os.environ, 'PATH': str(self.directory) + os.pathsep + os.environ['PATH'],
                    'COMMAND_LOG': str(self.log), 'PYTHON': str(self.directory / 'python-check'),
                    'KUBECTL': str(self.directory / 'kubectl'),
                    'KUBEBUILDER_ASSETS': str(self.directory), 'USE_EXISTING_CLUSTER': 'true'}

    def invoke(self, script, *args, **env):
        return subprocess.run(['bash', str(ROOT / script), *args], cwd=self.directory,
                              env={**self.env, **env}, text=True, capture_output=True, timeout=15)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def test_init_only_prepares_local_dependencies(self):
        for module in ('pulsar', 'storage', 'agent'):
            result = self.invoke(f'modules/{module}/scripts/dev.sh', 'init')
            self.assertEqual(result.returncode, 0, result.stderr)
        result = self.invoke('web/scripts/dev.sh', 'init')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual([(c['tool'], c['args']) for c in self.calls()],
                         [('go', ['mod', 'download'])] * 3 + [('npm', ['ci'])])
        self.assertEqual(self.calls()[-1]['cwd'], str(ROOT / 'web'))

    def test_unit_selects_only_own_go_packages_without_integration_tags(self):
        for module in ('pulsar', 'storage', 'agent'):
            result = self.invoke(f'modules/{module}/scripts/dev.sh', 'unit', '-run', 'TestSelected')
            self.assertEqual(result.returncode, 0, result.stderr)
            call = self.calls()[-1]
            self.assertEqual(call['cwd'], str(ROOT))
            self.assertIn(f'./internal/{module}/...', call['args'])
            self.assertEqual(call['args'][-2:], ['-run', 'TestSelected'])
            self.assertFalse(any(a.startswith('-tags') for a in call['args']))

    def test_integration_forces_isolated_envtest_and_preserves_spaced_asset_path(self):
        for module in ('pulsar', 'agent'):
            result = self.invoke(f'modules/{module}/scripts/dev.sh', 'integration')
            self.assertEqual(result.returncode, 0, result.stderr)
        calls = [c for c in self.calls() if c['tool'] == 'go']
        self.assertEqual(len(calls), 2)
        for call in calls:
            self.assertEqual(call['existing'], 'false')
            self.assertEqual(call['assets'], str(self.directory))
            self.assertIn('-tags=integration', call['args'])
            self.assertIn('^TestIntegration', call['args'])

    def test_invalid_assets_fail_before_running_go_tests(self):
        result = self.invoke('modules/agent/scripts/dev.sh', 'integration',
                             KUBEBUILDER_ASSETS=str(self.directory / 'missing assets'))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('envtest needs', result.stderr)
        self.assertEqual(self.calls(), [])

    def test_render_and_explicit_deploy_select_module_base(self):
        for module in ('pulsar', 'storage', 'agent'):
            script = f'modules/{module}/scripts/dev.sh'
            for action in ('render', 'deploy'):
                result = self.invoke(script, action)
                self.assertEqual(result.returncode, 0, result.stderr)
            base = str(ROOT / 'config' / module / 'default')
            self.assertEqual(self.calls()[-2]['args'], ['kustomize', base])
            self.assertEqual(self.calls()[-1]['args'], ['apply', '-k', base])

    def test_repository_render_resolves_bundles_overlays_and_labs(self):
        layouts = {'all': 'bundles/all', 'storage': 'bundles/storage',
                   'two-node': 'overlays/two-node', 'system-disk-storage': 'labs/system-disk/storage'}
        for target, relative in layouts.items():
            result = self.invoke('scripts/dev.sh', 'render', target)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.calls()[-1]['args'], ['kustomize', str(ROOT / 'deploy' / relative)])
        self.assertTrue(all(c['tool'] == 'kubectl' and c['args'][0] == 'kustomize' for c in self.calls()))

    def test_direct_module_scripts_match_dispatcher_and_root_tests_select_all_modules(self):
        for module in ('pulsar', 'storage', 'agent'):
            result = self.invoke(f'modules/{module}/scripts/test.sh', 'unit', '-run', 'TestSelected')
            self.assertEqual(result.returncode, 0, result.stderr)
            direct = self.calls()[-1]
            result = self.invoke(f'modules/{module}/scripts/dev.sh', 'unit', '-run', 'TestSelected')
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(self.calls()[-1], direct)
        self.log.unlink()
        result = self.invoke('scripts/test.sh', 'unit')
        self.assertEqual(result.returncode, 0, result.stderr)
        go_calls = [c for c in self.calls() if c['tool'] == 'go']
        self.assertEqual(len(go_calls), 4)
        for call, module in zip(go_calls, ('pulsar', 'storage', 'agent', 'kubeconfig')):
            self.assertIn(f'./internal/{module}/...', call['args'])
        self.assertEqual([c['args'] for c in self.calls() if c['tool'] == 'npm'],
                         [['test', '--'], ['run', 'build']])

    def test_images_use_root_context_and_explicit_module_dockerfiles(self):
        for module, variable in [('pulsar', 'IMG'), ('storage', 'STORAGE_IMG'), ('agent', 'AGENT_IMG')]:
            result = self.invoke(f'modules/{module}/scripts/dev.sh', 'image', **{variable: 'local/example:test'})
            self.assertEqual(result.returncode, 0, result.stderr)
            call = self.calls()[-1]
            self.assertEqual(call['cwd'], str(ROOT))
            self.assertEqual(call['args'], ['build', '-f', f'modules/{module}/Dockerfile', '-t', 'local/example:test', '.'])

    def test_csi_install_is_explicit_and_forwards_values_from_callers_directory(self):
        result = self.invoke('modules/storage/scripts/dev.sh', 'csi-install',
                             '--values', 'my values.yaml', CSI_CHART_VERSION='1.10.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        self.assertEqual([c['tool'] for c in calls], ['helm', 'helm', 'helm'])
        self.assertEqual(Path(calls[-1]['cwd']).resolve(), self.directory.resolve())
        self.assertEqual(calls[-1]['args'], ['upgrade', '--install', 'openebs-lvm',
                         'openebs-lvm/lvm-localpv', '--namespace', 'openebs', '--create-namespace',
                         '--version', '1.10.1', '--values', 'my values.yaml'])

    def test_help_and_unknown_actions_do_not_invoke_dependencies(self):
        for script in ('scripts/dev.sh', 'web/scripts/dev.sh', *(f'modules/{m}/scripts/dev.sh' for m in ('pulsar', 'storage', 'agent'))):
            self.assertEqual(self.invoke(script, '--help').returncode, 0)
            self.assertNotEqual(self.invoke(script, 'unknown-action').returncode, 0)
        self.assertEqual(self.calls(), [])


if __name__ == '__main__':
    unittest.main()
