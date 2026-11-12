"""Installer contract tests with a fake kubectl; no Kubernetes/network access."""

import base64
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]
LABEL = "pulsar.songof.dev/two-node"
TOKEN = "existing-console-token-must-not-appear-in-output"
FAKE_KUBECTL = r'''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
state = json.loads(Path(os.environ["FAKE_STATE"]).read_text())
argv = [a for a in sys.argv[1:] if not a.startswith("--request-timeout=")]
payload = sys.stdin.read() if "-" in argv else ""
record = {"argv": argv, "kubeconfig": os.environ.get("KUBECONFIG"),
          "requestTimeout": sys.argv[1]}
if payload and "--dry-run=client" not in argv:
    document = json.loads(payload)
    record["kind"] = document["kind"]
    if document["kind"] == "Secret":
        record["tokenLength"] = len(document["stringData"]["token"])
with open(os.environ["FAKE_LOG"], "a") as stream:
    stream.write(json.dumps(record) + "\n")
if state.get("fail") and argv[:len(state["fail"])] == state["fail"]:
    sys.stderr.write(state.get("error", "synthetic API permission failure"))
    sys.exit(1)
def output(items):
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
if argv[0] == "get":
    kind = argv[1]
    if kind == "nodes": output(state["nodes"])
    elif kind == "storageclasses": output([])
    elif kind == "csidrivers": output(state.get("drivers", []))
    elif kind == "csinodes": output(state.get("csinodes", []))
    elif kind == "crds": output([])
    elif kind == "namespace": print(json.dumps({"metadata": {"name": argv[2]}}))
    elif kind.startswith("deployments,"):
        namespace = argv[argv.index("-n") + 1]
        output([r for r in state.get("existing", []) if r["metadata"]["namespace"] == namespace])
    elif kind == "secret":
        secret = state.get("secret")
        if secret is not None: print(json.dumps(secret))
    elif kind == "pods": output(state["pods"])
    else: sys.exit("Unexpected get " + kind)
elif argv[0] == "kustomize":
    config = json.loads((Path(argv[1]) / "kustomization.yaml").read_text())
    resources = state["rendered"]
    for patch in config["patches"]:
        patch = json.loads(patch["patch"])
        for resource in resources:
            if resource["kind"] == patch["kind"] and resource["metadata"]["name"] == patch["metadata"]["name"]:
                resource["spec"]["template"]["spec"]["containers"][0]["image"] = patch["spec"]["template"]["spec"]["containers"][0]["image"]
    output(resources)
elif argv[0] == "create" and "--dry-run=client" in argv:
    if state.get("stream_rendered"):
        for resource in json.loads(payload)["items"]: print(json.dumps(resource))
        if state.get("rendered_garbage"): print("invalid trailing output")
    else: print(payload)
elif argv[0] in ("create", "apply", "label", "rollout", "wait"):
    # Even an unexpectedly verbose kubectl must not leak created Secret data.
    print(payload or "ok")
else: sys.exit("Unexpected command " + str(argv))
'''


def node(name, address):
    return {"metadata": {"name": name, "labels": {"kubernetes.io/hostname": name}},
            "spec": {"taints": [{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"}]},
            "status": {"addresses": [{"type": "InternalIP", "address": address}],
                       "conditions": [{"type": "Ready", "status": "True"}],
                       "allocatable": {"cpu": "4", "memory": "8Gi"},
                       "nodeInfo": {"operatingSystem": "linux", "architecture": "amd64"}}}


def workload(kind, namespace, name, container):
    spec = {"nodeSelector": {LABEL: "true"}, "containers": [{"name": container, "image": "placeholder:v1"}]}
    if container == "agent":
        spec["containers"][0]["env"] = [{"name": "ENABLE_DISK_WRITES", "value": "false"}]
    return {"apiVersion": "apps/v1", "kind": kind, "metadata": {"name": name, "namespace": namespace},
            "spec": {"template": {"spec": spec}}}


class TwoNodeInstallerTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        executable = self.directory / "kubectl"
        executable.write_text(FAKE_KUBECTL)
        executable.chmod(0o755)
        self.state = {"nodes": [node("actual-control", "172.23.254.149"), node("actual-worker", "172.23.254.150")],
                      "rendered": [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}}
                                   for ns in ("pulsar-operator-system", "diskfleet-system")],
                      "pods": [{"metadata": {"name": "agent-" + name}, "spec": {"nodeName": name},
                                "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
                               for name in ("actual-control", "actual-worker")]}
        self.state["rendered"].extend([
            workload("Deployment", "pulsar-operator-system", "pulsar-operator", "operator"),
            workload("Deployment", "diskfleet-system", "diskfleet-api", "api"),
            workload("DaemonSet", "diskfleet-system", "diskfleet-agent", "agent"),
            workload("Deployment", "pulsar-operator-system", "console-gateway", "nginx"),
        ])

    def run_script(self, *argv, images=True):
        state_path, log_path = self.directory / "state.json", self.directory / "log.jsonl"
        state_path.write_text(json.dumps(self.state))
        environment = dict(os.environ, PATH=str(self.directory) + os.pathsep + os.environ["PATH"],
                           FAKE_STATE=str(state_path), FAKE_LOG=str(log_path), KUBECONFIG="/test/admin.conf")
        for name in ("PULSAR_IMAGE", "STORAGE_IMAGE"):
            environment.pop(name, None)
        if images:
            environment.update(PULSAR_IMAGE="registry.example/pulsar:v1", STORAGE_IMAGE="registry.example/storage:v1")
        result = subprocess.run(["bash", str(ROOT / "scripts/deploy-two-node.sh"), *argv], env=environment,
                                capture_output=True, text=True, timeout=20, check=False)
        self.records = [json.loads(line) for line in log_path.read_text().splitlines()] if log_path.exists() else []
        return result

    def mutations(self):
        return [r for r in self.records if r["argv"][0] in ("apply", "create", "label") and
                "--dry-run=client" not in r["argv"]]

    def test_default_preflight_is_read_only_and_uses_real_node_names(self):
        result = self.run_script(images=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('"node": "actual-control"', result.stdout)
        self.assertIn("NoSchedule", result.stdout)
        self.assertIn("CSI registration check: INCOMPLETE", result.stdout)
        self.assertTrue(all(r["argv"][0] == "get" for r in self.records))
        self.assertEqual(self.mutations(), [])

    def test_kubeconfig_flag_overrides_environment(self):
        result = self.run_script("--kubeconfig", "/chosen/config")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all(r["kubeconfig"] == "/chosen/config" for r in self.records))

    def test_new_csi_node_with_null_drivers_is_incomplete(self):
        self.state["csinodes"] = [
            {"metadata": {"name": "actual-control"}, "spec": {"drivers": None}},
            {"metadata": {"name": "actual-worker"}, "spec": {}},
        ]
        result = self.run_script("preflight")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("CSI registration check: INCOMPLETE", result.stdout)
        self.assertEqual(self.mutations(), [])

    def test_kubectl_rendered_json_stream_is_accepted(self):
        self.state["stream_rendered"] = True
        result = self.run_script("install")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Management components", result.stdout)

    def test_invalid_rendered_stream_stops_before_mutations(self):
        self.state.update(stream_rendered=True, rendered_garbage=True)
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.mutations(), [])

    def test_not_ready_or_missing_target_fails_before_mutation(self):
        for change in ("not_ready", "missing"):
            with self.subTest(change=change):
                if change == "not_ready":
                    self.state["nodes"][1]["status"]["conditions"][0]["status"] = "False"
                else:
                    self.state["nodes"] = self.state["nodes"][:1]
                result = self.run_script("install")
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.mutations(), [])

    def test_extra_selector_match_fails_closed(self):
        third = node("unrelated-worker", "172.23.254.151")
        third["metadata"]["labels"][LABEL] = "true"
        self.state["nodes"].append(third)
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("other nodes", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_unsupported_taint_fails_without_removing_it(self):
        self.state["nodes"][1]["spec"]["taints"] = [{"key": "dedicated", "value": "other", "effect": "NoSchedule"}]
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("taints not tolerated", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_deleting_target_is_rejected_before_mutation(self):
        self.state["nodes"][1]["metadata"]["deletionTimestamp"] = "2026-11-13T00:00:00Z"
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is being deleted", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_permission_failure_is_not_treated_as_missing_csi(self):
        self.state["fail"] = ["get", "csidrivers"]
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("permission failure", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_install_requires_explicit_images(self):
        result = self.run_script("install", images=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Set PULSAR_IMAGE", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_active_disk_writes_are_not_silently_disabled(self):
        agent = workload("DaemonSet", "diskfleet-system", "diskfleet-agent", "agent")
        agent["spec"]["template"]["spec"]["containers"][0]["env"][0]["value"] = "true"
        self.state["existing"] = [agent]
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("will not overwrite", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_rendered_write_enabled_agent_is_rejected_before_mutation(self):
        agent = next(r for r in self.state["rendered"] if r["kind"] == "DaemonSet")
        agent["spec"]["template"]["spec"]["containers"][0]["env"][0]["value"] = "true"
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ENABLE_DISK_WRITES must be explicitly false", result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_invalid_existing_secret_does_not_mutate_cluster_or_leak_value(self):
        weak = "too-short"
        self.state["secret"] = {"data": {"token": base64.b64encode(weak.encode()).decode()}}
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("at least 24", result.stderr)
        self.assertNotIn(weak, result.stdout + result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_existing_secret_values_and_errors_are_never_printed(self):
        self.state["fail"] = ["get", "secret"]
        self.state["error"] = TOKEN
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(TOKEN, result.stdout + result.stderr)
        self.assertEqual(self.mutations(), [])

    def test_install_creates_credentials_and_labels_only_verified_nodes(self):
        result = self.run_script("install")
        self.assertEqual(result.returncode, 0, result.stderr)
        secrets = [r for r in self.records if r.get("kind") == "Secret"]
        self.assertEqual(len(secrets), 2)
        self.assertTrue(all(r["tokenLength"] >= 24 for r in secrets))
        labels = next(r["argv"] for r in self.records if r["argv"][0] == "label")
        self.assertEqual(labels, ["label", "nodes", "actual-control", "actual-worker", LABEL + "=true",
                                  "storage.diskfleet.io/enabled=true", "--overwrite"])
        self.assertNotIn("stringData", result.stdout + result.stderr)
        self.assertIn("Pulsar/BookKeeper deployment are NOT completed", result.stdout)
        self.assertIn("CSI is not registered", result.stdout)
        self.assertTrue(all(r["requestTimeout"] == "--request-timeout=0" for r in self.records
                            if r["argv"][0] in ("rollout", "wait")))
        temporary = next(r["argv"][1] for r in self.records if r["argv"][0] == "kustomize")
        self.assertFalse(Path(temporary).exists())

    def test_install_preserves_existing_credentials(self):
        self.state["secret"] = {"data": {"token": base64.b64encode(TOKEN.encode()).decode()}}
        result = self.run_script("install")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(r.get("kind") == "Secret" for r in self.records))
        self.assertNotIn(TOKEN, result.stdout + result.stderr)

    def test_rollout_failure_does_not_claim_success(self):
        self.state["fail"] = ["rollout", "status"]
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("two discovery Agents are Ready", result.stdout)

    def test_two_ready_agents_on_same_node_are_not_accepted(self):
        self.state["pods"][1]["spec"]["nodeName"] = "actual-control"
        result = self.run_script("install")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("one Ready pod on each verified node", result.stderr)


if __name__ == "__main__":
    unittest.main()
