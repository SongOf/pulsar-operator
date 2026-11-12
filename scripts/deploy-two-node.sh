#!/usr/bin/env bash
# Python's standard library handles JSON and credentials; no Python packages required.
set -euo pipefail
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec python3 - "$SCRIPT_DIR/.." "$@" <<'PY'
import argparse
import base64
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import sys
import tempfile

ROOT = Path(sys.argv.pop(1)).resolve()
TARGET_IPS = ("172.23.254.149", "172.23.254.150")
LABEL = "pulsar.songof.dev/two-node"
DRIVER = "local.csi.openebs.io"
NAMESPACES = ("pulsar-operator-system", "diskfleet-system")
CREDENTIALS = ((NAMESPACES[0], "pulsar-operator-console"),
               (NAMESPACES[1], "diskfleet-api-token"))
WORKLOADS = (("Deployment", NAMESPACES[0], "pulsar-operator", "operator", "PULSAR_IMAGE"),
             ("Deployment", NAMESPACES[1], "diskfleet-api", "api", "STORAGE_IMAGE"),
             ("DaemonSet", NAMESPACES[1], "diskfleet-agent", "agent", "STORAGE_IMAGE"))
parser = argparse.ArgumentParser(description="Read-only two-node preflight, or install management/discovery components. Does not prepare disks or install Pulsar/CSI.")
parser.add_argument("command", nargs="?", choices=("preflight", "install"), default="preflight")
parser.add_argument("--kubeconfig", default=os.environ.get("KUBECONFIG", "/etc/kubernetes/admin.conf"),
                    help="single kubeconfig path, or KUBECONFIG environment file list")
parser.add_argument("--timeout", default="300s", help="kubectl rollout/wait timeout (default: 300s)")
args = parser.parse_args()
# Preserve KUBECONFIG's file-list semantics; do not expose credentials with config view.
os.environ["KUBECONFIG"] = args.kubeconfig


def kubectl(*argv, data=None, private=False):
    request_timeout = "0" if argv[0] in ("rollout", "wait") else "30s"
    result = subprocess.run(["kubectl", f"--request-timeout={request_timeout}", *argv], input=data,
                            text=True, capture_output=True, check=False)
    if result.returncode:
        detail = "credential operation failed; command output withheld" if private else result.stderr.strip()
        raise RuntimeError(f"kubectl {argv[0]} failed: {detail}")
    return result.stdout


def get(kind, *extra, private=False):
    output = kubectl("get", kind, *extra, "-o", "json", private=private)
    return json.loads(output) if output.strip() else None


def items(kind, *extra):
    return get(kind, *extra).get("items", [])


def agent_writes(resource):
    containers = resource.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
    agent = next((c for c in containers if c["name"] == "agent"), {})
    return [entry for entry in agent.get("env", []) if entry["name"] == "ENABLE_DISK_WRITES"]


def preflight():
    nodes = items("nodes")
    selected = []
    for address in TARGET_IPS:
        matches = [n for n in nodes if any(a["type"] == "InternalIP" and a["address"] == address
                   for a in n.get("status", {}).get("addresses", []))]
        if len(matches) != 1:
            raise RuntimeError(f"Expected exactly one node with InternalIP {address}; found {len(matches)}")
        node = matches[0]
        status, spec, meta = node.get("status", {}), node.get("spec", {}), node["metadata"]
        if meta.get("deletionTimestamp"):
            raise RuntimeError(f"Target node {meta['name']} is being deleted")
        if not any(c["type"] == "Ready" and c["status"] == "True" for c in status.get("conditions", [])):
            raise RuntimeError(f"Target node {meta['name']} ({address}) is not Ready")
        if spec.get("unschedulable") or status.get("nodeInfo", {}).get("operatingSystem") != "linux":
            raise RuntimeError(f"Target node {meta['name']} must be schedulable Linux")
        unsupported = [t for t in spec.get("taints", []) if t.get("effect") == "NoExecute" or
                       (t.get("effect") == "NoSchedule" and t.get("key") not in
                        ("node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"))]
        if unsupported:
            raise RuntimeError(f"Target node {meta['name']} has taints not tolerated by this profile: " + json.dumps(unsupported))
        hostname = meta.get("labels", {}).get("kubernetes.io/hostname")
        if not hostname:
            raise RuntimeError(f"Target node {meta['name']} has no kubernetes.io/hostname label")
        print(json.dumps({"node": meta["name"], "internalIP": address, "hostname": hostname,
                          "taints": spec.get("taints", []), "allocatable": status.get("allocatable", {}),
                          "architecture": status.get("nodeInfo", {}).get("architecture")}, ensure_ascii=False))
        selected.append(node)
    names = {n["metadata"]["name"] for n in selected}
    hostnames = {n["metadata"]["labels"]["kubernetes.io/hostname"] for n in selected}
    if len(names) != 2 or len(hostnames) != 2:
        raise RuntimeError("Target IPs must identify two distinct nodes with distinct hostname labels")
    outsiders = [n["metadata"]["name"] for n in nodes if n["metadata"]["name"] not in names
                 and n["metadata"].get("labels", {}).get(LABEL) == "true"]
    if outsiders:
        raise RuntimeError(f"Two-node selector also matches other nodes: {', '.join(outsiders)}")
    classes = items("storageclasses")
    drivers = {d["metadata"]["name"] for d in items("csidrivers")}
    registered = {n["metadata"]["name"] for n in items("csinodes")
                  if any(d["name"] == DRIVER for d in (n.get("spec", {}).get("drivers") or []))}
    crds = {c["metadata"]["name"] for c in items("crds")}
    print("StorageClasses: " + json.dumps([{"name": c["metadata"]["name"], "provisioner": c.get("provisioner"),
                                          "bindingMode": c.get("volumeBindingMode")} for c in classes]))
    print("CSI drivers: " + json.dumps(sorted(drivers)))
    csi_registered = DRIVER in drivers and names.issubset(registered)
    print(f"{DRIVER} registered on target nodes: " + json.dumps(sorted(names & registered)))
    print("Project CRDs: " + json.dumps(sorted(c for c in crds if c.endswith(("pulsar.songof.dev", "storage.diskfleet.io")))))
    existing = []
    for namespace in NAMESPACES:
        if get("namespace", namespace, "--ignore-not-found"):
            existing.extend(items("deployments,daemonsets,statefulsets,services,pods,persistentvolumeclaims", "-n", namespace))
    for crd in ("physicaldisks.storage.diskfleet.io", "diskpools.storage.diskfleet.io",
                "pulsarclusters.pulsar.songof.dev", "bookkeeperclusters.pulsar.songof.dev"):
        if crd in crds:
            existing.extend(items(crd, "--all-namespaces"))
    print("Existing project resources: " + json.dumps([
        {"kind": r["kind"], "namespace": r["metadata"].get("namespace", ""),
         "name": r["metadata"]["name"], "phase": r.get("status", {}).get("phase", ""),
         "readyReplicas": r.get("status", {}).get("readyReplicas")} for r in existing]))
    agents = [r for r in existing if r["kind"] == "DaemonSet" and r["metadata"]["name"] == "diskfleet-agent"]
    unsafe_agent = any(agent_writes(r) != [{"name": "ENABLE_DISK_WRITES", "value": "false"}] for r in agents)
    if unsafe_agent:
        print("WARNING: existing Agent does not explicitly disable disk writes; install will refuse to overwrite it.")
    print("CSI registration check: " + ("present; verify controller/node plugin health separately" if csi_registered else
          "INCOMPLETE; install CSI using docs/two-node-guide.md before physical PV/Pulsar validation"))
    print("Preflight completed without changing cluster resources.")
    return names, csi_registered, unsafe_agent


def image_refs():
    refs = {}
    for variable in ("PULSAR_IMAGE", "STORAGE_IMAGE"):
        value = os.environ.get(variable, "")
        tail = value.rsplit("/", 1)[-1]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/@-]*", value) or (
                "@" not in tail and (":" not in tail or tail.endswith(":latest"))):
            raise RuntimeError(f"Set {variable} to an already-built, reachable image with an explicit version tag or digest (not latest)")
        refs[variable] = value
    return refs


def render(refs):
    with tempfile.TemporaryDirectory(prefix=".two-node-install-", dir=ROOT / "deploy") as temporary:
        patches = []
        for kind, namespace, name, container, variable in WORKLOADS:
            patch = {"apiVersion": "apps/v1", "kind": kind, "metadata": {"name": name, "namespace": namespace},
                     "spec": {"template": {"spec": {"containers": [{"name": container, "image": refs[variable]}]}}}}
            patches.append({"patch": json.dumps(patch)})
        config = {"apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization",
                  "resources": ["../two-node"], "patches": patches}
        (Path(temporary) / "kustomization.yaml").write_text(json.dumps(config))
        yaml = kubectl("kustomize", temporary)
        output = kubectl("create", "--dry-run=client", "--validate=false", "-f", "-", "-o", "json", data=yaml)
    # kubectl may print one JSON object per YAML document instead of a List.
    # Decode the complete stream before any mutations; reject trailing garbage.
    resources, cursor = [], 0
    decoder = json.JSONDecoder()
    while cursor < len(output):
        if output[cursor].isspace():
            cursor += 1
            continue
        resource, cursor = decoder.raw_decode(output, cursor)
        if not isinstance(resource, dict):
            raise RuntimeError("Rendered resource must be a JSON object")
        if resource.get("kind") == "List":
            resources.extend(resource.get("items") or [])
        else:
            resources.append(resource)
    allowed = {"Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding", "ServiceAccount",
               "ConfigMap", "Service", "Deployment", "DaemonSet"}
    for resource in resources:
        if resource["kind"] not in allowed or resource["metadata"].get("namespace", NAMESPACES[0]) not in NAMESPACES:
            raise RuntimeError("Rendered overlay contains unexpected resource kinds/namespaces; refusing install")
    workloads = [r for r in resources if r["kind"] in ("Deployment", "DaemonSet")]
    expected = {(kind, namespace, name) for kind, namespace, name, _, _ in WORKLOADS}
    expected.add(("Deployment", NAMESPACES[0], "console-gateway"))
    actual = {(r["kind"], r["metadata"]["namespace"], r["metadata"]["name"]) for r in workloads}
    if actual != expected or len(workloads) != 4:
        raise RuntimeError("Rendered overlay must contain exactly the four expected management workloads")
    for resource in workloads:
        spec = resource["spec"]["template"]["spec"]
        if spec.get("nodeSelector", {}).get(LABEL) != "true":
            raise RuntimeError("Every workload must use the verified two-node selector")
    for kind, namespace, name, container, variable in WORKLOADS:
        resource = next(r for r in workloads if r["kind"] == kind and r["metadata"]["name"] == name)
        containers = resource["spec"]["template"]["spec"]["containers"]
        if next(c for c in containers if c["name"] == container)["image"] != refs[variable]:
            raise RuntimeError(f"Rendered image mismatch for {name}")
        if kind == "DaemonSet" and agent_writes(resource) != [{"name": "ENABLE_DISK_WRITES", "value": "false"}]:
            raise RuntimeError("Rendered ENABLE_DISK_WRITES must be explicitly false; refusing install")
    return resources


def credentials():
    missing = []
    for namespace, name in CREDENTIALS:
        secret = get("secret", name, "-n", namespace, "--ignore-not-found", private=True)
        if secret:
            try:
                token = base64.b64decode(secret.get("data", {}).get("token", ""), validate=True).decode()
                valid = len(token) >= 24 and not any(c.isspace() for c in token)
            except (ValueError, UnicodeError):
                valid = False
            if not valid:
                raise RuntimeError(f"Secret {namespace}/{name} must have a valid token of at least 24 characters; value withheld")
        else:
            missing.append((namespace, name))
    return missing


def install(names, csi_registered, unsafe_agent):
    if unsafe_agent:
        raise RuntimeError("Existing Agent may have disk writes enabled. This discovery installer will not overwrite it; review its active DiskPools and deployment separately")
    resources = render(image_refs())
    missing = credentials()  # Validate all existing credentials before any mutations.
    namespaces = [r for r in resources if r["kind"] == "Namespace"]
    kubectl("apply", "-f", "-", data=json.dumps({"apiVersion": "v1", "kind": "List", "items": namespaces}))
    for namespace, name in missing:
        secret = {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": name, "namespace": namespace},
                  "type": "Opaque", "stringData": {"token": secrets.token_urlsafe(32)}}
        kubectl("create", "-f", "-", data=json.dumps(secret), private=True)
    # Apply discovery-only Agent before adding selectors, so an older write-enabled template cannot target new nodes.
    kubectl("apply", "-f", "-", data=json.dumps({"apiVersion": "v1", "kind": "List", "items": resources}))
    kubectl("label", "nodes", *sorted(names), f"{LABEL}=true", "storage.diskfleet.io/enabled=true", "--overwrite")
    for namespace, name in ((NAMESPACES[0], "pulsar-operator"), (NAMESPACES[1], "diskfleet-api"), (NAMESPACES[0], "console-gateway")):
        print(f"Waiting for Deployment {namespace}/{name} ...", flush=True)
        kubectl("rollout", "status", f"deployment/{name}", "-n", namespace, f"--timeout={args.timeout}")
    print("Waiting for discovery Agent on both verified nodes ...", flush=True)
    kubectl("wait", "--for=jsonpath={.status.desiredNumberScheduled}=2", "daemonset/diskfleet-agent",
            "-n", NAMESPACES[1], f"--timeout={args.timeout}")
    kubectl("rollout", "status", "daemonset/diskfleet-agent", "-n", NAMESPACES[1], f"--timeout={args.timeout}")
    pods = items("pods", "-n", NAMESPACES[1], "-l", "app=diskfleet-agent")
    ready = [p for p in pods if not p["metadata"].get("deletionTimestamp") and
             any(c["type"] == "Ready" and c["status"] == "True" for c in p.get("status", {}).get("conditions", []))]
    if len(ready) != 2 or {p.get("spec", {}).get("nodeName") for p in ready} != names:
        raise RuntimeError("Agent rollout did not produce one Ready pod on each verified node")
    print("Management components and two discovery Agents are Ready. Disk writes remain DISABLED.")
    print("Physical PV provisioning and Pulsar/BookKeeper deployment are NOT completed by this command.")
    if not csi_registered:
        print("CSI is not registered on both nodes; install/verify it before enabling any disk or generating a physical-storage Pulsar profile.")
    print("Credentials are retained in Kubernetes Secrets; values were not printed.")
    print("Console: KUBECONFIG=" + shlex.quote(args.kubeconfig) +
          " kubectl -n pulsar-operator-system port-forward --address=127.0.0.1 svc/console-gateway 8080:8080")
    print("Open http://127.0.0.1:8080 (storage management: /storage/). See docs/two-node-guide.md for subsequent steps.")


try:
    if not re.fullmatch(r"[1-9][0-9]*(s|m|h)", args.timeout):
        raise RuntimeError("--timeout must be a positive duration such as 300s or 5m")
    selected, csi_ready, writes_active = preflight()
    if args.command == "install":
        install(selected, csi_ready, writes_active)
except (OSError, RuntimeError, ValueError, KeyError, StopIteration) as error:
    print(f"ERROR: {error}", file=sys.stderr)
    sys.exit(1)
PY
