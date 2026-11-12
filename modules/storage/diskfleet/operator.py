"""Per-node DiskFleet reconciler. CSI owns logical volumes and mounts.

The agent is intentionally not a generic remote command executor. Every device
mutation goes through the separately gated, identity-checking LVMBackend.
"""
from __future__ import annotations

import hashlib
import logging
import os
import random
import signal
import threading
from typing import Any
from urllib.parse import quote

GROUP = "storage.diskfleet.io"
VERSION = "v1alpha1"
BASE = f"/apis/{GROUP}/{VERSION}"
NODE_LABEL = f"{GROUP}/node"
FINALIZER = f"{GROUP}/retain-media"
LOG = logging.getLogger("diskfleet.operator")


def node_label(value: str) -> str:
    """Kubernetes names can be 253 characters, label values only 63."""
    if len(value) <= 63:
        return value
    return "sha256-" + hashlib.sha256(value.encode()).hexdigest()[:40]


def storage_class_name(pool_name: str) -> str:
    if len(pool_name) <= 53:
        return f"diskfleet-{pool_name}"
    digest = hashlib.sha256(pool_name.encode()).hexdigest()[:8]
    return f"diskfleet-{pool_name[:44]}-{digest}"


def disk_name(node: str, disk: dict) -> str:
    identity = f"{node}\0{disk.get('serial', '')}\0{disk['path']}"
    return "disk-" + hashlib.sha256(identity.encode()).hexdigest()[:32]


def claims_overlap(first: dict, second: dict) -> bool:
    """A VG or either physical identity component reserves media on one node."""
    if first.get("nodeName") != second.get("nodeName"):
        return False
    return (first.get("vgName") == second.get("vgName") or any(
        a.get(key) and a.get(key) == b.get(key)
        for a in first.get("devices", []) for b in second.get("devices", [])
        for key in ("path", "serial")))


def conflicting_pool_names(pools: list[dict]) -> set[str]:
    """Index immutable claim components without a quadratic all-pairs scan."""
    seen, conflicts = {}, set()
    for pool in pools:
        spec, name = pool["spec"], pool["metadata"]["name"]
        identities = [("vg", spec.get("vgName"))]
        identities.extend((key, disk.get(key)) for disk in spec.get("devices", []) for key in ("path", "serial"))
        for kind, value in identities:
            if not value:
                continue
            previous = seen.setdefault((spec.get("nodeName"), kind, value), name)
            if previous != name:
                conflicts.update((previous, name))
    return conflicts


def storage_class(pool: dict, hostname: str | None = None) -> dict:
    """A class per node/pool makes placement and failure domains explicit."""
    spec = pool["spec"]
    name = pool["metadata"]["name"]
    # vgName is restricted by CRD and the backend: these anchors are literal.
    return {
        "apiVersion": "storage.k8s.io/v1",
        "kind": "StorageClass",
        "metadata": {
            "name": storage_class_name(name),
            "labels": {f"{GROUP}/pool": node_label(name), NODE_LABEL: node_label(spec["nodeName"])},
            "annotations": {f"{GROUP}/pool-uid": pool["metadata"].get("uid", "")},
        },
        "provisioner": "local.csi.openebs.io",
        "parameters": {"storage": "lvm", "vgpattern": f"^{spec['vgName']}$",
                       "fsType": "ext4", "thinProvision": "no"},
        "reclaimPolicy": "Retain",
        "volumeBindingMode": "WaitForFirstConsumer",
        "allowVolumeExpansion": True,
        "allowedTopologies": [{"matchLabelExpressions": [{
            "key": "kubernetes.io/hostname", "values": [hostname or spec["nodeName"]]
        }]}],
    }


class NodeOperator:
    def __init__(self, kube: Any, backend: Any, node_name: str,
                 discover=None, writes_enabled: bool = False):
        if not node_name:
            raise ValueError("NODE_NAME is required")
        self.kube = kube
        self.backend = backend
        self.node = node_name
        self.writes_enabled = writes_enabled
        self._node_hostname = None
        self._claims_snapshot = None
        if discover is None:
            from .storage import discover_disks
            discover = discover_disks
        self.discover = discover

    def _list(self, plural: str) -> list[dict]:
        return self.kube.list(f"{BASE}/{plural}", label_selector=f"{NODE_LABEL}={node_label(self.node)}")

    def _status(self, plural: str, obj: dict, status: dict) -> None:
        if obj.get("status", {}) == status:
            return
        name = obj["metadata"]["name"]
        self.kube.request("PATCH", f"{BASE}/{plural}/{name}/status", {
            "metadata": {"resourceVersion": obj["metadata"].get("resourceVersion")},
            "status": status,
        })
        obj["status"] = status

    def _ensure_finalizer(self, pool: dict) -> None:
        metadata = pool["metadata"]
        finalizers = metadata.get("finalizers", [])
        if FINALIZER not in finalizers:
            updated = self.kube.request("PATCH", f"{BASE}/diskpools/{metadata['name']}", {
                "metadata": {"resourceVersion": metadata.get("resourceVersion"),
                             "finalizers": [*finalizers, FINALIZER]}})
            pool["metadata"] = updated["metadata"]

    def _hostname(self) -> str:
        if self._node_hostname is None:
            node = self.kube.request("GET", "/api/v1/nodes/" + quote(self.node, safe=""))
            hostname = node.get("metadata", {}).get("labels", {}).get("kubernetes.io/hostname")
            if not hostname or len(hostname) > 63:
                raise ValueError("Node requires a valid kubernetes.io/hostname topology label")
            self._node_hostname = hostname
        return self._node_hostname

    def _ensure_class(self, pool: dict) -> str:
        desired = storage_class(pool, self._hostname())
        name = desired["metadata"]["name"]
        endpoint = "/apis/storage.k8s.io/v1/storageclasses"
        try:
            existing = self.kube.request("GET", f"{endpoint}/{name}")
        except Exception as error:
            if getattr(error, "status", None) != 404:
                raise
            self.kube.request("POST", endpoint, desired)
            return name
        owner = existing.get("metadata", {}).get("annotations", {}).get(f"{GROUP}/pool-uid")
        if owner != pool["metadata"].get("uid", ""):
            raise ValueError(f"StorageClass {name} already belongs to another pool")
        for key in ("provisioner", "parameters", "reclaimPolicy", "volumeBindingMode",
                    "allowVolumeExpansion", "allowedTopologies"):
            if existing.get(key) != desired[key]:
                raise ValueError(f"StorageClass {name} differs in {key}; refusing to overwrite")
        return name

    def _check_claim_owner(self, pool: dict) -> None:
        uid = pool["metadata"].get("uid")
        if not uid:
            raise ValueError("DiskPool UID is required")
        # Read all claims: an absent or incorrect node label must not hide a
        # competing owner. The backend's file lock and UID journal close races.
        claims = self._claims_snapshot
        if claims is None:
            claims = self.kube.list(f"{BASE}/diskpools")
        for other in claims:
            if (other["metadata"].get("uid") == uid
                    and other["metadata"]["name"] == pool["metadata"]["name"]):
                continue
            if claims_overlap(pool["spec"], other["spec"]):
                raise ValueError("Media is also claimed by DiskPool " + other["metadata"]["name"])

    def _legacy_owner_proven(self, pool: dict, hostname: str) -> bool:
        desired = storage_class(pool, hostname)
        status = pool.get("status", {})
        if (status.get("storageClass") != desired["metadata"]["name"]
                or status.get("topologyHostname") != hostname):
            return False
        try:
            actual = self.kube.request("GET", "/apis/storage.k8s.io/v1/storageclasses/" + desired["metadata"]["name"])
        except Exception as error:
            if getattr(error, "status", None) == 404:
                return False
            raise
        return (actual.get("metadata", {}).get("annotations", {}).get(f"{GROUP}/pool-uid") == pool["metadata"]["uid"]
                and all(actual.get(key) == desired[key] for key in (
                    "provisioner", "parameters", "reclaimPolicy", "volumeBindingMode",
                    "allowVolumeExpansion", "allowedTopologies")))

    def reconcile_pool(self, pool: dict) -> None:
        spec = pool["spec"]
        # Never trust the label without checking the actual spec as well.
        if spec.get("nodeName") != self.node:
            return
        status = dict(pool.get("status", {}))
        status["observedGeneration"] = pool["metadata"].get("generation", 1)
        if pool["metadata"].get("deletionTimestamp"):
            status.update(phase="Retained", message="Media and existing volumes are retained. "
                          "Administrator must assess data and explicitly remove the retain-media finalizer.")
            self._status("diskpools", pool, status)
            return
        self._ensure_finalizer(pool)
        if not spec.get("approved", False):
            status.update(phase="PendingApproval", message="Device initialization has not been approved.")
        elif not self.writes_enabled:
            status.update(phase="WritesDisabled", message="Agent ENABLE_DISK_WRITES is false; no disk commands executed.")
        else:
            try:
                self._check_claim_owner(pool)
                hostname = self._hostname()
                result = self.backend.ensure_pool(spec["vgName"], spec["devices"], approved=True,
                    owner_uid=pool["metadata"]["uid"], allow_legacy_migration=self._legacy_owner_proven(pool, hostname))
                class_name = self._ensure_class(pool)
                status.update(phase="Ready", message="Volume group ready; OpenEBS CSI provisions PVCs.",
                              storageClass=class_name,
                              topologyHostname=hostname,
                              capacityBytes=int(result["capacityBytes"]),
                              availableBytes=int(result["availableBytes"]))
            except Exception as error:
                LOG.exception("Pool reconciliation failed for %s", pool["metadata"]["name"])
                status.update(phase="Error", message=str(error)[:1500])
        self._status("diskpools", pool, status)

    def reconcile_disks(self, pools: list[dict]) -> None:
        discovered = self.discover()
        existing = {d["metadata"]["name"]: d for d in self._list("physicaldisks")}
        seen = set()
        managed = {}
        for pool in pools:
            if pool["spec"].get("nodeName") != self.node:
                continue
            if pool.get("status", {}).get("phase") not in ("Ready", "Retained"):
                continue
            for device in pool["spec"]["devices"]:
                managed[(device["path"], device["serial"], int(device["sizeBytes"]))] = pool["metadata"]["name"]
        for disk in discovered:
            name = disk_name(self.node, disk)
            seen.add(name)
            spec = {"nodeName": self.node, "path": disk["path"],
                    "serial": str(disk.get("serial") or ""), "sizeBytes": int(disk["sizeBytes"]),
                    "model": disk.get("model") or "unknown", "media": disk.get("media") or "unknown"}
            phase = disk.get("state", "Protected")
            reasons = list(disk.get("reasons", []))
            pool_name = managed.get((spec["path"], spec["serial"], spec["sizeBytes"]))
            if pool_name:
                phase, reasons = "Managed", []
            current = existing.get(name)
            if current is None:
                body = {"apiVersion": f"{GROUP}/{VERSION}", "kind": "PhysicalDisk",
                        "metadata": {"name": name, "labels": {NODE_LABEL: node_label(self.node)}}, "spec": spec}
                try:
                    current = self.kube.request("POST", f"{BASE}/physicaldisks", body)
                except Exception as error:
                    if getattr(error, "status", None) == 409:
                        # Another iteration/process has already created it; next poll reads it.
                        continue
                    raise
            elif current["spec"] != spec:
                phase, reasons, pool_name = "Protected", ["Device identity or capacity changed; inventory spec is immutable."], None
            self._status("physicaldisks", current, {"phase": phase, "reasons": reasons,
                         "poolName": pool_name or ""})
        for name, disk in existing.items():
            if name not in seen and disk["spec"].get("nodeName") == self.node:
                status = dict(disk.get("status", {}))
                status.update(phase="Offline", reasons=["Device absent from current node discovery."])
                self._status("physicaldisks", disk, status)

    def reconcile_once(self) -> None:
        self._node_hostname = None
        # One fresh, paginated collection per cycle, rather than a cluster-wide
        # scan per local pool. All claims participate in uniqueness checks even
        # when their editable node label is absent or incorrect.
        self._claims_snapshot = self.kube.list(f"{BASE}/diskpools")
        try:
            pools = [p for p in self._claims_snapshot if p["metadata"].get("labels", {}).get(NODE_LABEL) == node_label(self.node)]
            for pool in pools:
                try:
                    self.reconcile_pool(pool)
                except Exception:
                    LOG.exception("Unable to reconcile/report pool %s", pool.get("metadata", {}).get("name"))
            self.reconcile_disks(pools)
        finally:
            self._claims_snapshot = None


def main() -> None:
    from .kube import KubeClient
    from .storage import LVMBackend
    logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO"),
                        format="%(asctime)s %(levelname)s %(name)s %(message)s")
    state_dir = os.getenv("STATE_DIR", "/var/lib/diskfleet")
    operator = NodeOperator(KubeClient(), LVMBackend(state_dir), os.environ.get("NODE_NAME", ""),
                            writes_enabled=os.getenv("ENABLE_DISK_WRITES", "false").lower() == "true")
    stopped = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stopped.set())
    interval = max(10, int(os.getenv("RECONCILE_INTERVAL", "30")))
    stopped.wait(random.uniform(0, min(10, interval)))
    while not stopped.is_set():
        try:
            operator.reconcile_once()
        except Exception:
            LOG.exception("Node reconciliation failed; will retry")
        stopped.wait(interval + random.uniform(0, interval * 0.2))


if __name__ == "__main__":
    main()
