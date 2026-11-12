"""Offline repository entrypoints and captured upstream source integrity.

No test invokes kubectl or runs an E2E workload. Source hashes refer to the
captured LF-normalised snapshot and declared integration changes, not Git blobs.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
MODULE = ROOT / "modules/storage"
# Frozen inventory of the imported 3c042c5 source snapshot. Removing a file from
# both the checkout and manifest must still fail the completeness check.
EXPECTED_UPSTREAM_PATHS = set(""".github/workflows/ci.yml
.gitignore
Dockerfile
Makefile
README.md
api/v1alpha1/bookkeepercluster_types.go
api/v1alpha1/common_types.go
api/v1alpha1/groupversion_info.go
api/v1alpha1/pulsarcluster_types.go
api/v1alpha1/zz_generated.deepcopy.go
cmd/operator/main.go
config/crd/kustomization.yaml
config/crd/pulsar.songof.dev_bookkeeperclusters.yaml
config/crd/pulsar.songof.dev_pulsarclusters.yaml
config/default/kustomization.yaml
config/manager/kustomization.yaml
config/manager/manager.yaml
config/monitoring/podmonitor.yaml
config/rbac/kustomization.yaml
config/rbac/role.yaml
config/samples/bookkeepercluster.yaml
config/samples/pulsarcluster.yaml
docs/operator-scope.md
go.mod
go.sum
internal/console/server.go
internal/console/server_test.go
internal/controller/apply.go
internal/controller/bookkeeper_controller.go
internal/controller/bookkeeper_controller_test.go
internal/controller/monitoring.go
internal/controller/pulsar_controller.go
internal/controller/pulsar_controller_test.go
internal/controller/suite_test.go
internal/metrics/dashboard.go
internal/metrics/metrics_test.go
internal/metrics/prometheus.go
internal/resources/bookkeeper.go
internal/resources/common.go
internal/resources/monitoring.go
internal/resources/pulsar.go
internal/resources/resources_test.go
test/e2e/e2e.sh
web/index.html
web/package-lock.json
web/package.json
web/src/App.jsx
web/src/ClusterView.jsx
web/src/ConfigEditor.jsx
web/src/CreateDialog.jsx
web/src/Monitoring.jsx
web/src/api.js
web/src/charts.jsx
web/src/main.jsx
web/src/model.js
web/src/model.test.js
web/src/styles.css
web/src/ui.jsx
web/vite.config.js""".splitlines())


class EntrypointTests(unittest.TestCase):
    def run_cli(self, argv, cwd):
        return subprocess.run(
            argv, cwd=cwd, capture_output=True, text=True, timeout=20,
            env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
        )

    def test_help_entrypoints_work_from_repository_and_unrelated_directory(self):
        entrypoints = [
            ([sys.executable, str(ROOT / "scripts/prepare-pulsar.py")], "--pools-json"),
            (["bash", str(ROOT / "test/e2e/storage.sh")], "EXISTING_DISKFLEET_STORAGECLASS"),
            (["bash", str(ROOT / "test/e2e/pulsar-storage.sh")], "TEST_NAMESPACE PULSAR_CLUSTER_NAME"),
        ]
        with tempfile.TemporaryDirectory(prefix="pulsar cli help ") as temp:
            for cwd in (ROOT, Path(temp)):
                for argv, expected in entrypoints:
                    with self.subTest(cwd=str(cwd), entrypoint=argv[-1]):
                        result = self.run_cli([*argv, "--help"], cwd)
                        self.assertEqual(0, result.returncode, result.stderr)
                        self.assertIn(expected, result.stdout)
                        self.assertEqual("", result.stderr)
            self.assertEqual([], list(Path(temp).iterdir()))

    def prepare_args(self, output):
        return [
            sys.executable, str(ROOT / "scripts/prepare-pulsar.py"),
            "--pools-json", str(MODULE / "integrations/pulsar/pools.example.json"),
            "--zookeeper-pools", "journal-0,journal-1,journal-2",
            "--journal-pools", "journal-0,journal-1,journal-2",
            "--ledger-pools", "ledger-0,ledger-1,ledger-2",
            "--namespace", "diskfleet-pulsar-offline", "--name", "storage-test",
            "--output", str(output),
        ]

    def test_offline_wrapper_generates_identical_manifests_across_working_directories(self):
        manifests = []
        with tempfile.TemporaryDirectory(prefix="pulsar offline plan ") as temp:
            for index, cwd in enumerate((ROOT, Path(temp))):
                output = Path(temp) / f"plan-{index}.json"
                with self.subTest(cwd=str(cwd)):
                    result = self.run_cli(self.prepare_args(output), cwd)
                    self.assertEqual(0, result.returncode, result.stderr)
                    summary = json.loads(result.stdout)
                    self.assertEqual("offline-input", summary["mode"])
                    self.assertFalse(summary["applied"])
                    self.assertEqual(5, summary["resources"])
                    manifests.append(json.loads(output.read_text()))
            self.assertEqual(manifests[0], manifests[1])
            resources = manifests[0]["items"]
            self.assertEqual(["Namespace", "StorageClass", "StorageClass", "StorageClass", "PulsarCluster"],
                             [item["kind"] for item in resources])
            pulsar = resources[-1]
            self.assertEqual("pulsar.songof.dev/v1alpha1", pulsar["apiVersion"])
            self.assertEqual("diskfleet-pulsar-offline", pulsar["metadata"]["namespace"])
            self.assertEqual("storage-test", pulsar["metadata"]["name"])

    def test_offline_wrapper_preserves_existing_reviewed_output(self):
        with tempfile.TemporaryDirectory(prefix="pulsar protected plan ") as temp:
            output = Path(temp) / "reviewed.json"
            original = '{"reviewed": true}\n'
            output.write_text(original)
            result = self.run_cli(self.prepare_args(output), Path(temp))
            self.assertNotEqual(0, result.returncode)
            self.assertIn("输出文件已存在", result.stderr)
            self.assertEqual(original, output.read_text())


class SourceManifestTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.manifest = json.loads((ROOT / "docs/upstream-source.json").read_text())

    def test_captured_source_inventory_is_complete_unique_and_local(self):
        manifest = self.manifest
        self.assertEqual("https://github.com/SongOf/pulsar-operator", manifest["repository"])
        self.assertEqual("3c042c5", manifest["revision"])
        entries = manifest["files"]
        self.assertEqual(EXPECTED_UPSTREAM_PATHS, {item["path"] for item in entries})
        self.assertEqual(len(entries), len({item["path"] for item in entries}))
        self.assertEqual(len(entries), len({item["localPath"] for item in entries}))
        for item in entries:
            with self.subTest(path=item["path"]):
                relative = Path(item["localPath"])
                self.assertFalse(relative.is_absolute())
                self.assertNotIn("..", relative.parts)
                local = (ROOT / relative).resolve()
                self.assertTrue(local.is_relative_to(ROOT))
                self.assertTrue(local.is_file())
                self.assertEqual(
                    f"https://raw.githubusercontent.com/SongOf/pulsar-operator/3c042c5/{item['path']}",
                    item["url"],
                )
                self.assertGreater(item["lineCount"], 0)
                self.assertRegex(item["importedSha256"], r"^[0-9a-f]{64}$")
        readme = next(item for item in entries if item["path"] == "README.md")
        self.assertEqual("docs/upstream-readme.md", readme["localPath"])

    def test_source_content_matches_import_or_documented_integration_change(self):
        entries = self.manifest["files"]
        declared = self.manifest.get("localModifications", [])
        changes = {item["path"]: item for item in declared}
        self.assertEqual(len(changes), len(declared))
        self.assertTrue(changes.keys() <= {item["path"] for item in entries})
        for item in entries:
            with self.subTest(path=item["path"]):
                data = (ROOT / item["localPath"]).read_bytes()
                expected = item["importedSha256"]
                if item["path"] in changes:
                    change = changes[item["path"]]
                    self.assertTrue(change["reason"].strip())
                    self.assertRegex(change["currentSha256"], r"^[0-9a-f]{64}$")
                    expected = change["currentSha256"]
                else:
                    self.assertEqual(item["lineCount"], len(data.splitlines()))
                self.assertEqual(expected, hashlib.sha256(data).hexdigest())


if __name__ == "__main__":
    unittest.main()
