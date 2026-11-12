"""Repository integration checks; Python's standard library is sufficient.

The small readers below inspect this repository's simple Kustomization resource
lists and top-level resource identities; they are not general YAML parsers.
When kubectl is installed, its real Kustomize renderer also validates the bases.
These checks do not start Kubernetes or prove that backend images run.
"""

import json
import re
import shutil
import subprocess
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
CLUSTER_KINDS = {
    "Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding",
    "StorageClass", "PersistentVolume",
}


def top_value(text, key):
    match = re.search(r"^" + re.escape(key) + r":\s*([^\n#]+)", text, re.MULTILINE)
    return match.group(1).strip().strip("\"'") if match else ""


def resource_paths(directory):
    """Read local resources from the block list used by our Kustomizations."""
    active = False
    for line in (directory / "kustomization.yaml").read_text().splitlines():
        if line == "resources:":
            active = True
            continue
        if active and line and not line[0].isspace() and not line.startswith("#"):
            break
        if active:
            match = re.match(r"\s+-\s+([^#]+)", line)
            if match:
                yield (directory / match.group(1).strip()).resolve()


def documents(text):
    return [part for part in re.split(r"(?m)^---\s*$", text) if part.strip()]


def identity(document, namespace=""):
    kind = top_value(document, "kind")
    version = top_value(document, "apiVersion")
    metadata = re.search(r"^metadata:\s*\n(.*?)(?=^\S|\Z)", document, re.MULTILINE | re.DOTALL)
    if not kind or not version or not metadata:
        raise AssertionError("Resource is missing apiVersion, kind or metadata")
    # Metadata uses two-space indentation in both upstream bases and the gateway.
    fields = dict(re.findall(r"^  (name|namespace):\s*([^\n#]+)", metadata.group(1), re.MULTILINE))
    if "name" not in fields:
        raise AssertionError("Resource is missing metadata.name")
    group = version.split("/", 1)[0] if "/" in version else ""
    ns = "" if kind in CLUSTER_KINDS else namespace or fields.get("namespace", "").strip()
    return group, kind, ns, fields["name"].strip()


class RepositoryTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("kubectl"), "kubectl is needed for actual Kustomize rendering")
    def test_two_node_overlay_keeps_narrow_scheduling_and_discovery(self):
        result = subprocess.run(["kubectl", "kustomize", str(ROOT / "deploy/two-node")],
                                capture_output=True, text=True, check=True)
        rendered = documents(result.stdout)
        workloads = [doc for doc in rendered if top_value(doc, "kind") in ("Deployment", "DaemonSet")]
        self.assertEqual(len(workloads), 4)
        for doc in workloads:
            node_selector = re.search(r"^      nodeSelector:\n((?:        [^\n]+\n)+)", doc, re.MULTILINE)
            self.assertIsNotNone(node_selector)
            self.assertIn('pulsar.songof.dev/two-node: "true"', node_selector.group(1))
            self.assertEqual(doc.count("effect: NoSchedule"), 2)
            for role in ("control-plane", "master"):
                self.assertIn("key: node-role.kubernetes.io/" + role, doc)
        agent = next(doc for doc in workloads if top_value(doc, "kind") == "DaemonSet")
        self.assertIn('storage.diskfleet.io/enabled: "true"', agent)
        self.assertRegex(agent, r'name: ENABLE_DISK_WRITES\n\s+value: "false"')
        self.assertRegex(agent, r'mountPath: /run/udev\n\s+name: udev\n\s+readOnly: true')
        self.assertRegex(agent, r'path: /run/udev\n\s+type: Directory\n\s+name: udev')
        self.assertFalse(any(top_value(doc, "kind") in ("Node", "DiskPool", "PulsarCluster", "Secret")
                             for doc in rendered))

    def collect_resources(self, directory, inherited_namespace="", visited=None):
        visited = set() if visited is None else visited
        self.assertNotIn(directory, visited, "A base is included more than once")
        visited.add(directory)
        config = (directory / "kustomization.yaml").read_text()
        namespace = inherited_namespace or top_value(config, "namespace")
        found = []
        paths = list(resource_paths(directory))
        self.assertTrue(paths, f"No resource paths in {directory}")
        for path in paths:
            self.assertTrue(path.is_relative_to(ROOT), f"Resource escapes repository: {path}")
            self.assertTrue(path.exists(), f"Missing resource: {path}")
            if path.is_dir():
                found.extend(self.collect_resources(path, namespace, visited))
            else:
                found.extend(identity(doc, namespace) for doc in documents(path.read_text()))
        # This repository has one file-backed ConfigMap generator.
        if "configMapGenerator:" in config:
            generator = config.split("configMapGenerator:", 1)[1]
            names = re.findall(r"^  - name:\s*(\S+)", generator, re.MULTILINE)
            files = re.findall(r"^      -\s*(\S+)", generator, re.MULTILINE)
            self.assertTrue(names)
            self.assertTrue(files)
            for filename in files:
                self.assertTrue((directory / filename).is_file(), filename)
            found.extend(("", "ConfigMap", namespace, name) for name in names)
        return found

    def test_all_base_references_and_namespace_ownership(self):
        directory = ROOT / "deploy/all"
        self.assertEqual(set(resource_paths(directory)), {
            ROOT / "config/default", ROOT / "modules/storage/deploy", ROOT / "deploy/console",
        })
        self.assertFalse(top_value((directory / "kustomization.yaml").read_text(), "namespace"))
        resources = self.collect_resources(directory)
        self.assertEqual(len(resources), len(set(resources)), "Duplicate Kubernetes resource identity")
        namespaces = {name for _, kind, _, name in resources if kind == "Namespace"}
        self.assertEqual(namespaces, {"pulsar-operator-system", "diskfleet-system"})
        for _, kind, namespace, name in resources:
            if kind not in CLUSTER_KINDS:
                self.assertIn(namespace, namespaces, f"Missing namespace for {kind}/{name}")
        self.assertIn(("", "Service", "pulsar-operator-system", "console-gateway"), resources)

    def test_gateway_routes_match_backend_services(self):
        config = (ROOT / "deploy/console/nginx.conf").read_text()
        for filename, name, namespace in [
            ("config/manager/manager.yaml", "pulsar-operator-console", "pulsar-operator-system"),
            ("modules/storage/deploy/api.yaml", "diskfleet-api", "diskfleet-system"),
        ]:
            service = next(doc for doc in documents((ROOT / filename).read_text())
                           if identity(doc) == ("", "Service", namespace, name))
            self.assertRegex(service, r"\bport:\s*8080\b")
            self.assertIn(f"http://{name}.{namespace}.svc:8080", config)
        storage = re.search(r"location \^~ /storage/\s*\{([^}]+)\}", config).group(1)
        self.assertIn("proxy_pass http://diskfleet-api.diskfleet-system.svc:8080/;", storage)
        self.assertRegex(config, r"location = /storage\s*\{\s*return 308 /storage/")
        for path in ("/api/", "/"):
            location = re.search(r"location " + re.escape(path) + r"\s*\{([^}]+)\}", config).group(1)
            self.assertIn("proxy_pass http://pulsar-operator-console.pulsar-operator-system.svc:8080;", location)
        self.assertNotRegex(config, r"(?m)^\s*(?:proxy_set_header\s+Authorization|auth_basic)")

    def test_gateway_runs_unprivileged_and_health_is_local(self):
        manifest = (ROOT / "deploy/console/gateway.yaml").read_text()
        config = (ROOT / "deploy/console/nginx.conf").read_text()
        for setting in ("runAsNonRoot: true", "runAsUser: 101", "fsGroup: 101",
                        "readOnlyRootFilesystem: true", "allowPrivilegeEscalation: false",
                        "drop: [ALL]", "automountServiceAccountToken: false",
                        "mountPath: /tmp/nginx", "emptyDir:"):
            self.assertIn(setting, manifest)
        self.assertIn("command: [nginx]", manifest)
        self.assertRegex(config, r"(?m)^pid /tmp/nginx/nginx.pid;")
        for name in ("client_body", "proxy", "fastcgi", "uwsgi", "scgi"):
            self.assertRegex(config, rf"{name}_temp_path /tmp/nginx/\w+;")
        health = re.search(r"location = /healthz\s*\{([^}]+)\}", config).group(1)
        self.assertIn('return 200 "ok\\n";', health)
        self.assertNotIn("proxy_pass", health)
        self.assertNotIn("kind: Secret", manifest)
        self.assertNotIn("TOKEN", manifest)

    def test_disk_writes_are_disabled_by_default(self):
        agent = (ROOT / "modules/storage/deploy/agent.yaml").read_text()
        values = re.findall(r"name:\s*ENABLE_DISK_WRITES,\s*value:\s*['\"]?(\w+)", agent)
        self.assertEqual(values, ["false"])
        # The unified overlay must not turn the safe default into a write action.
        for path in (ROOT / "deploy").rglob("*.yaml"):
            self.assertNotIn("ENABLE_DISK_WRITES", path.read_text())

    def test_upstream_snapshot_has_traceable_records(self):
        path = ROOT / "docs/upstream-source.json"
        if not path.exists():
            self.skipTest("Upstream provenance manifest has not been created")
        manifest = json.loads(path.read_text())
        self.assertEqual(manifest["repository"], "https://github.com/SongOf/pulsar-operator")
        self.assertEqual(manifest["revision"], "3c042c5")
        records = manifest["files"]
        paths = [item["path"] for item in records]
        self.assertEqual(len(paths), len(set(paths)))
        self.assertTrue({"go.mod", "cmd/operator/main.go", "internal/controller/bookkeeper_controller.go"}.issubset(paths))
        for item in records:
            local = (ROOT / item["localPath"]).resolve()
            self.assertTrue(local.is_relative_to(ROOT))
            self.assertTrue(local.is_file(), item["localPath"])
            self.assertRegex(item["importedSha256"], r"^[0-9a-f]{64}$")
            self.assertGreater(item["lineCount"], 0)
            self.assertEqual(item["url"], "https://raw.githubusercontent.com/SongOf/pulsar-operator/3c042c5/" + item["path"])
        # Hashes describe the imported baseline. Integration changes may alter
        # working files, so comparing every current hash would reject valid edits.

    @unittest.skipUnless(shutil.which("kubectl"), "kubectl is not installed; real Kustomize render skipped")
    def test_real_kustomize_render(self):
        result = subprocess.run(["kubectl", "kustomize", "deploy/all"], cwd=ROOT,
                                capture_output=True, text=True, timeout=30, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        rendered = documents(result.stdout)
        resources = [identity(doc) for doc in rendered]
        self.assertEqual(len(resources), len(set(resources)))
        self.assertIn(("", "Service", "diskfleet-system", "diskfleet-api"), resources)
        self.assertIn(("", "Service", "pulsar-operator-system", "console-gateway"), resources)
        generated = [name for _, kind, ns, name in resources
                     if kind == "ConfigMap" and ns == "pulsar-operator-system" and name.startswith("console-gateway-config-")]
        self.assertEqual(len(generated), 1)
        gateway = next(doc for doc in rendered
                       if identity(doc) == ("apps", "Deployment", "pulsar-operator-system", "console-gateway"))
        self.assertIn("name: " + generated[0], gateway)
        for kind, namespace, name, image in [
            ("Deployment", "pulsar-operator-system", "pulsar-operator", "pulsar-operator:dev"),
            ("Deployment", "diskfleet-system", "diskfleet-api", "pulsar-operator-storage:dev"),
            ("DaemonSet", "diskfleet-system", "diskfleet-agent", "pulsar-operator-storage:dev"),
        ]:
            workload = next(doc for doc in rendered
                            if identity(doc) == ("apps", kind, namespace, name))
            self.assertEqual(re.findall(r"^\s+image:\s*(\S+)", workload, re.MULTILINE), [image], name)
        operator = next(doc for doc in rendered
                        if identity(doc) == ("apps", "Deployment", "pulsar-operator-system", "pulsar-operator"))
        token = re.search(r"^[ ]*- name: CONSOLE_TOKEN\n((?:[ ]{10,}[^\n]+\n)+)",
                          operator, re.MULTILINE)
        self.assertIsNotNone(token, "Pulsar console must retain its token environment variable")
        token_source = token.group(1)
        self.assertIn("secretKeyRef:", token_source)
        self.assertRegex(token_source, r"(?m)^\s+name: pulsar-operator-console$")
        self.assertRegex(token_source, r"(?m)^\s+key: token$")
        self.assertRegex(token_source, r"(?m)^\s+optional: false$")
        self.assertNotRegex(token_source, r"(?m)^\s+value:")


if __name__ == "__main__":
    unittest.main()
