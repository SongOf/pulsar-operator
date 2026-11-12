"""Fail-closed, host-local disk discovery and one-disk LVM pool reconciliation.

No disk writes occur during discovery. LVM writes require BOTH a process-wide
ENABLE_DISK_WRITES=true switch and an approved DiskPool. The CSI driver owns
logical volumes, filesystems and mounts; this module never formats or deletes.
"""
from __future__ import annotations

import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import uuid
from typing import Callable


class StorageError(RuntimeError):
    """A command failed or a safety precondition was not met."""


def run_command(argv: list[str]) -> str:
    """No shell, no implicit confirmations, bounded execution."""
    try:
        result = subprocess.run(
            argv, stdin=subprocess.DEVNULL, capture_output=True, text=True,
            timeout=60, check=False, env={**os.environ, "LC_ALL": "C"},
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise StorageError(f"{argv[0]} failed: {exc}") from exc
    if result.returncode:
        raise StorageError(f"{argv[0]} failed ({result.returncode}): {result.stderr.strip()[:1000]}")
    return result.stdout


def _json_command(runner: Callable, argv: list[str]) -> dict:
    try:
        value = json.loads(runner(argv))
        if not isinstance(value, dict):
            raise ValueError("expected JSON object")
        return value
    except (ValueError, TypeError) as exc:
        raise StorageError(f"Invalid {argv[0]} JSON output") from exc


def _walk(items):
    for item in items:
        yield item
        yield from _walk(item.get("children", []))


def _mount_devices(proc_root: Path) -> tuple[set[str], set[str]]:
    mounted, swaps = set(), set()
    # /proc/1/mountinfo is the HOST mount namespace when the node pod has hostPID.
    # The current mount namespace is also checked for local execution.
    for source in (proc_root / "1/mountinfo", proc_root / "self/mountinfo"):
        for line in source.read_text().splitlines():
            fields = line.split()
            if len(fields) >= 6:
                mounted.add(fields[2])
    for line in (proc_root / "swaps").read_text().splitlines()[1:]:
        if line.split():
            swaps.add(os.path.realpath(line.split()[0]))
    return mounted, swaps


def discover_disks(
    *, runner: Callable = run_command, sys_root: Path | str = "/sys",
    by_id: Path | str = "/dev/disk/by-id", proc_root: Path | str = "/proc",
) -> list[dict]:
    """Discover whole disks; inaccessible safety evidence marks them Protected.

    Linux RAID/multipath/LVM nodes are not blank physical disk candidates. Any
    partition, filesystem signature, mount, swap or holder blocks enrollment.
    Disk identity is a by-id symlink plus serial and exact byte capacity.
    """
    report = _json_command(runner, [
        "lsblk", "--json", "--bytes", "--paths", "--output",
        "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,FSTYPE,PTTYPE,MOUNTPOINTS,RO,ROTA",
    ])
    sys_root, by_id, proc_root = Path(sys_root), Path(by_id), Path(proc_root)
    safety_error = None
    try:
        mounted, swaps = _mount_devices(proc_root)
    except (OSError, ValueError) as exc:
        mounted, swaps = set(), set()
        safety_error = f"Cannot read host mount/swap state: {exc}"
    aliases = {}
    try:
        for link in sorted(by_id.iterdir(), key=lambda p: (not p.name.startswith("wwn-"), p.name)):
            # pvcreate adds metadata aliases; they must not replace the approved
            # hardware identity or qualify a disk lacking a hardware by-id.
            if (link.is_symlink() and not link.name.startswith("lvm-pv-uuid-")
                    and not re.search(r"-part\d+$", link.name)):
                aliases.setdefault(os.path.realpath(link), str(link))
    except OSError:
        pass
    output, seen = [], set()
    for disk in _walk(report.get("blockdevices", [])):
        if disk.get("type") != "disk":
            continue
        path = disk.get("path") or disk.get("name", "")
        if path in seen:
            continue
        seen.add(path)
        reasons = []
        if safety_error:
            reasons.append(safety_error)
        if disk.get("ro"):
            reasons.append("Read-only device")
        if disk.get("children"):
            reasons.append("Has partitions or dependent block devices")
        for member in _walk([disk]):
            name = os.path.basename(member.get("kname") or member.get("name", ""))
            if member.get("fstype"):
                reasons.append(f"Filesystem/signature detected: {member['fstype']}")
            if member.get("pttype"):
                reasons.append(f"Partition table detected: {member['pttype']}")
            if any(member.get("mountpoints") or []) or member.get("maj:min") in mounted:
                reasons.append("Mounted device (includes host mounts)")
            if os.path.realpath(member.get("path") or "") in swaps or member.get("fstype") == "swap":
                reasons.append("Swap device")
            try:
                if any((sys_root / "class/block" / name / "holders").iterdir()):
                    reasons.append("Device has active holders (RAID/LVM/multipath)")
            except OSError:
                reasons.append("Cannot verify device holders")
        serial = str(disk.get("serial") or "").strip()
        stable = aliases.get(os.path.realpath(path))
        if not serial:
            reasons.append("Stable serial number is missing")
        if not stable:
            reasons.append("Stable /dev/disk/by-id path is missing")
        size = int(disk.get("size") or 0)
        if size < 64 * 1024 * 1024:
            reasons.append("Device is smaller than 64 MiB")
        identity = str(disk.get("wwn") or serial or path)
        output.append({
            "id": "disk-" + hashlib.sha256(identity.encode()).hexdigest()[:16],
            "name": os.path.basename(path), "path": stable or path, "devicePath": path,
            "serial": serial, "wwn": str(disk.get("wwn") or ""), "sizeBytes": size,
            "model": str(disk.get("model") or "").strip(),
            "media": "NVMe" if os.path.basename(path).startswith("nvme") else ("HDD" if disk.get("rota") else "SSD"),
            "state": "Protected" if reasons else "Available", "reasons": list(dict.fromkeys(reasons)),
            "poolId": None,
        })
    return output


def _validate_device(path: str) -> None:
    # No partitions, arbitrary paths, or symlinks outside the stable-id directory.
    p = Path(path)
    if p.parent != Path("/dev/disk/by-id") or re.search(r"-part\d+$", p.name):
        raise StorageError("Device must be a whole-disk /dev/disk/by-id symlink")
    try:
        if not p.is_symlink() or not stat.S_ISBLK(p.stat().st_mode):
            raise StorageError("Stable device path must resolve to a block device")
    except OSError as exc:
        raise StorageError(f"Cannot resolve block device: {exc}") from exc


def _pv_uuid() -> str:
    value = uuid.uuid4().hex
    return "-".join([value[:6], value[6:10], value[10:14], value[14:18], value[18:22], value[22:26], value[26:]])


class LVMBackend:
    """Reconcile a VG using a durable ownership journal and host-local lock.

    A predetermined LVM PV UUID allows recovery if pvcreate succeeded immediately
    before process termination. Existing externally-created PVs/VGs are NEVER
    adopted. If an owned PV/VG disappears after being recorded, fail closed;
    recreating it automatically could destroy recoverable data.
    """

    def __init__(self, state_dir: str | Path, *, runner: Callable = run_command,
                 inventory: Callable | None = None, device_validator: Callable = _validate_device):
        self.state_dir = Path(state_dir)
        self.runner = runner
        self.inventory = inventory or (lambda: discover_disks(runner=self.runner))
        self.device_validator = device_validator

    def _save(self, path: Path, value: dict) -> None:
        fd, tmp = tempfile.mkstemp(prefix=".journal-", dir=self.state_dir)
        try:
            with os.fdopen(fd, "w") as stream:
                json.dump(value, stream, sort_keys=True)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(tmp, path)
            dir_fd = os.open(self.state_dir, os.O_DIRECTORY)
            try:
                os.fsync(dir_fd)
            finally:
                os.close(dir_fd)
        finally:
            if os.path.exists(tmp):
                os.unlink(tmp)

    def _report(self, tool: str, fields: str, section: str) -> list[dict]:
        report = _json_command(self.runner, [tool, "--reportformat", "json", "--units", "b", "--nosuffix", "--options", fields])
        if "report" not in report:
            raise StorageError(f"Missing {tool} report")
        return [{key: value.strip() if isinstance(value, str) else value for key, value in row.items()}
                for block in report["report"] for row in block.get(section, [])]

    def _state(self, path: str, vg_name: str):
        pvs = self._report("pvs", "pv_name,pv_uuid,vg_name", "pv")
        vgs = self._report("vgs", "vg_name,vg_uuid,vg_tags,vg_size,vg_free,pv_count", "vg")
        matches = [row for row in pvs if os.path.realpath(row["pv_name"]) == os.path.realpath(path)]
        groups = [row for row in vgs if row["vg_name"] == vg_name]
        if len(matches) > 1 or len(groups) > 1:
            raise StorageError("Ambiguous LVM identity")
        return (matches[0] if matches else None, groups[0] if groups else None)

    def _identity(self, device: dict) -> dict:
        path = device.get("path", "")
        self.device_validator(path)
        matches = [disk for disk in self.inventory()
                   if os.path.realpath(disk.get("devicePath") or disk["path"]) == os.path.realpath(path)]
        if len(matches) != 1:
            raise StorageError("Disk missing or device identity is ambiguous")
        disk = matches[0]
        if not device.get("serial") or disk["serial"] != device["serial"] or disk["sizeBytes"] != device.get("sizeBytes"):
            raise StorageError("Disk serial/capacity changed since approval")
        return disk

    def _check_unused(self, disk: dict, path: str, *, owned_pv: bool = False):
        allowed = "Filesystem/signature detected: LVM2_member"
        reasons = [reason for reason in disk.get("reasons", []) if not (owned_pv and reason == allowed)]
        if reasons or (disk.get("state") != "Available" and not owned_pv):
            raise StorageError("Disk is protected: " + "; ".join(reasons or [disk.get("state", "unknown state")]))
        signatures = _json_command(self.runner, ["wipefs", "--no-act", "--json", path]).get("signatures")
        if signatures is None or not isinstance(signatures, list):
            raise StorageError("Cannot verify disk signatures")
        if any(not (owned_pv and signature.get("type") == "LVM2_member") for signature in signatures):
            raise StorageError("Disk contains existing filesystem, RAID or partition-table signatures")

    def ensure_pool(self, pool_name: str, devices: list[dict], approved: bool = False,
                    *, owner_uid: str, allow_legacy_migration: bool = False) -> dict:
        if approved is not True or os.environ.get("ENABLE_DISK_WRITES", "").lower() != "true":
            raise StorageError("Disk writes disabled: explicit approval and ENABLE_DISK_WRITES=true are required")
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.+-]{0,126}", pool_name) or pool_name in {"all", "default"}:
            raise StorageError("Invalid volume group name")
        if not isinstance(devices, list) or len(devices) != 1 or not isinstance(devices[0], dict):
            raise StorageError("Exactly one disk is required per pool; use separate pools for bulk enrollment")
        if not isinstance(owner_uid, str) or not owner_uid.strip():
            raise StorageError("A DiskPool UID is required for durable ownership")
        device = devices[0]
        self.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        with (self.state_dir / ".lvm.lock").open("a+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            return self._ensure_locked(pool_name, device, owner_uid, allow_legacy_migration)

    def _ensure_locked(self, pool_name: str, device: dict, owner_uid: str,
                       allow_legacy_migration: bool) -> dict:
        disk = self._identity(device)
        path = device["path"]
        journal_path = self.state_dir / (pool_name + ".json")
        identity = {key: device[key] for key in ("path", "serial", "sizeBytes")}
        pv, vg = self._state(path, pool_name)
        if journal_path.exists():
            try:
                journal = json.loads(journal_path.read_text())
                if journal["identity"] != identity or journal["vgName"] != pool_name:
                    raise StorageError("Pool identity differs from its durable ownership record")
                if journal["phase"] not in {"prepared", "pv-created", "ready"}:
                    raise StorageError("Invalid pool journal phase")
                for key in ("pvUUID", "tag"):
                    if not journal[key]:
                        raise StorageError("Invalid pool journal ownership record")
                if "poolUID" in journal and journal["poolUID"] != owner_uid:
                    raise StorageError("DiskPool UID does not match the durable ownership record")
                if "poolUID" not in journal and not (
                        allow_legacy_migration and journal["phase"] == "ready" and pv and vg):
                    raise StorageError("Legacy ownership requires a verified existing Ready pool; refusing automatic adoption")
            except (ValueError, KeyError, TypeError) as exc:
                raise StorageError("Cannot read pool ownership journal") from exc
        else:
            if pv or vg:
                raise StorageError("Refusing to adopt an existing PV/VG without its ownership journal")
            for other in self.state_dir.glob("*.json"):
                try:
                    owned = json.loads(other.read_text())["identity"]
                except (ValueError, KeyError, OSError, TypeError) as exc:
                    raise StorageError("Unreadable ownership journal; manual investigation required") from exc
                if owned["serial"] == identity["serial"] or os.path.realpath(owned["path"]) == os.path.realpath(path):
                    raise StorageError("Disk is already reserved by another pool")
            self._check_unused(disk, path)
            journal = {"identity": identity, "vgName": pool_name, "poolUID": owner_uid, "pvUUID": _pv_uuid(),
                       "tag": "diskfleet_" + uuid.uuid4().hex, "phase": "prepared"}
            self._save(journal_path, journal)

        if pv and pv["pv_uuid"] != journal["pvUUID"]:
            raise StorageError("LVM PV UUID does not match the ownership journal")
        if vg:
            if not pv or pv.get("vg_name") != pool_name or int(vg.get("pv_count", 0)) != 1 or journal["tag"] not in vg.get("vg_tags", "").split(","):
                raise StorageError("Existing VG ownership or disk membership does not match")
            # Legacy migration occurs only after all existing media ownership
            # checks pass; this branch never initializes or recreates anything.
            journal["poolUID"] = owner_uid
            journal["phase"] = "ready"
            # Do not rewrite an unchanged record on every reconciliation.
            if not journal_path.exists() or json.loads(journal_path.read_text()) != journal:
                self._save(journal_path, journal)
            return self._result(vg, pool_name)
        if journal["phase"] == "ready":
            raise StorageError("Previously managed VG is missing; refusing automatic recreation")
        if pv and pv.get("vg_name"):
            raise StorageError("Disk belongs to a different VG")
        if not pv:
            if journal["phase"] != "prepared":
                raise StorageError("Previously initialized PV is missing; refusing automatic recreation")
            self._check_unused(self._identity(device), path)
            # Persisted UUID closes the crash window between initialization and
            # journal advancement; no --force/--yes or wipefs writes are used.
            self.runner(["pvcreate", "--uuid", journal["pvUUID"], "--norestorefile", path])
            pv, vg = self._state(path, pool_name)
            if not pv or pv["pv_uuid"] != journal["pvUUID"] or pv.get("vg_name") or vg:
                raise StorageError("PV initialization did not produce the expected isolated PV")
        journal["phase"] = "pv-created"
        self._save(journal_path, journal)
        self._check_unused(self._identity(device), path, owned_pv=True)
        self.runner(["vgcreate", "--addtag", journal["tag"], pool_name, path])
        pv, vg = self._state(path, pool_name)
        if not pv or not vg or pv["pv_uuid"] != journal["pvUUID"] or pv.get("vg_name") != pool_name or int(vg.get("pv_count", 0)) != 1 or journal["tag"] not in vg.get("vg_tags", "").split(","):
            raise StorageError("VG creation did not produce the expected owned group")
        journal["phase"] = "ready"
        self._save(journal_path, journal)
        return self._result(vg, pool_name)

    @staticmethod
    def _result(vg: dict, name: str) -> dict:
        return {"vgName": name, "capacityBytes": int(float(vg["vg_size"])), "availableBytes": int(float(vg["vg_free"]))}
