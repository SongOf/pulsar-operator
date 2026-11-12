import copy
import json
import multiprocessing
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from diskfleet.storage import LVMBackend, StorageError, discover_disks


DISK = {"path": "/dev/disk/by-id/wwn-test", "devicePath": "/dev/disk/by-id/wwn-test",
        "serial": "serial-test", "sizeBytes": 4 * 1024**3, "state": "Available", "reasons": []}


class FakeLVM:
    """An in-memory LVM double: never invokes a command or accesses a real disk."""

    def __init__(self):
        self.disk = copy.deepcopy(DISK)
        self.pv = None
        self.vg = None
        self.signatures = []
        self.calls = []
        self.crash_after = None

    def inventory(self):
        disk = copy.deepcopy(self.disk)
        if self.pv:
            disk.update(state="Protected", reasons=["Filesystem/signature detected: LVM2_member"] + disk["reasons"])
        return [disk]

    def __call__(self, argv):
        self.calls.append(argv)
        command = argv[0]
        if command == "pvs":
            return json.dumps({"report": [{"pv": [self.pv] if self.pv else []}]})
        if command == "vgs":
            return json.dumps({"report": [{"vg": [self.vg] if self.vg else []}]})
        if command == "wipefs":
            return json.dumps({"signatures": self.signatures or ([{"type": "LVM2_member"}] if self.pv else [])})
        if command == "pvcreate":
            self.pv = {"pv_name": argv[-1], "pv_uuid": argv[2], "vg_name": ""}
        elif command == "vgcreate":
            self.pv["vg_name"] = argv[-2]
            self.vg = {"vg_name": argv[-2], "vg_uuid": "vg-test", "vg_tags": argv[2],
                       "pv_count": "1", "vg_size": str(4 * 1024**3 - 4 * 1024**2),
                       "vg_free": str(4 * 1024**3 - 4 * 1024**2)}
        else:
            raise AssertionError(f"Unexpected command {argv}")
        if self.crash_after == command:
            self.crash_after = None
            raise StorageError(f"Simulated process crash after {command}")
        return ""

    @property
    def writes(self):
        return [argv for argv in self.calls if argv[0] in {"pvcreate", "vgcreate"}]


class StorageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.env = patch.dict(os.environ, {"ENABLE_DISK_WRITES": "true"})
        self.env.start()
        self.addCleanup(self.env.stop)
        self.fake = FakeLVM()
        self.backend = LVMBackend(self.temp.name, runner=self.fake,
                                  inventory=self.fake.inventory, device_validator=lambda path: None)

    def ensure(self, **kwargs):
        return self.backend.ensure_pool("df-test", [copy.deepcopy(DISK)], approved=True, owner_uid=kwargs.pop("owner_uid", "uid-pool"), **kwargs)

    def test_process_gate_and_explicit_approval_both_required(self):
        with patch.dict(os.environ, {"ENABLE_DISK_WRITES": "false"}):
            with self.assertRaises(StorageError):
                self.ensure()
        with self.assertRaises(StorageError):
            self.backend.ensure_pool("df-test", [DISK], approved=False, owner_uid="uid-pool")
        with self.assertRaises(StorageError):
            self.backend.ensure_pool("df-test", [DISK], approved="true", owner_uid="uid-pool")
        self.assertEqual(self.fake.calls, [])

    def test_system_or_mounted_disk_is_rejected(self):
        self.fake.disk.update(state="Protected", reasons=["Mounted device (includes host mounts)"])
        with self.assertRaisesRegex(StorageError, "protected"):
            self.ensure()
        self.assertEqual(self.fake.writes, [])

    def test_identity_changed_after_approval_is_rejected(self):
        self.fake.disk["serial"] = "replacement-disk"
        with self.assertRaisesRegex(StorageError, "serial/capacity changed"):
            self.ensure()
        self.assertEqual(self.fake.writes, [])

    def test_unreported_raid_signature_is_rejected(self):
        self.fake.signatures = [{"type": "linux_raid_member"}]
        with self.assertRaisesRegex(StorageError, "signatures"):
            self.ensure()
        self.assertEqual(self.fake.writes, [])

    def test_existing_external_pv_is_not_adopted(self):
        self.fake.pv = {"pv_name": DISK["path"], "pv_uuid": "external-uuid", "vg_name": ""}
        with self.assertRaisesRegex(StorageError, "Refusing to adopt"):
            self.ensure()
        self.assertEqual(self.fake.writes, [])

    def test_existing_external_vg_name_is_not_adopted(self):
        self.fake.vg = {"vg_name": "df-test", "vg_uuid": "external", "vg_tags": "", "pv_count": "1"}
        with self.assertRaisesRegex(StorageError, "Refusing to adopt"):
            self.ensure()
        self.assertEqual(self.fake.writes, [])

    def test_create_and_idempotent_reconcile(self):
        first = self.ensure()
        self.assertEqual(first["vgName"], "df-test")
        self.assertEqual(first["capacityBytes"], 4 * 1024**3 - 4 * 1024**2)
        self.assertEqual(len(self.fake.writes), 2)
        self.fake.vg["vg_free"] = "1024"
        second = self.ensure()
        self.assertEqual(second["availableBytes"], 1024)
        self.assertEqual(len(self.fake.writes), 2)
        self.assertTrue(all("--force" not in cmd and "--yes" not in cmd for cmd in self.fake.writes))

    def test_resume_after_pvcreate_crash_uses_same_uuid(self):
        self.fake.crash_after = "pvcreate"
        with self.assertRaises(StorageError):
            self.ensure()
        pv_uuid = self.fake.pv["pv_uuid"]
        self.ensure()
        self.assertEqual(self.fake.pv["pv_uuid"], pv_uuid)
        self.assertEqual([cmd[0] for cmd in self.fake.writes], ["pvcreate", "vgcreate"])

    def test_resume_after_vgcreate_crash_does_not_recreate(self):
        self.fake.crash_after = "vgcreate"
        with self.assertRaises(StorageError):
            self.ensure()
        self.ensure()
        self.assertEqual([cmd[0] for cmd in self.fake.writes], ["pvcreate", "vgcreate"])

    def test_replaced_pv_uuid_is_rejected(self):
        self.ensure()
        self.fake.pv["pv_uuid"] = "replacement-uuid"
        with self.assertRaisesRegex(StorageError, "UUID"):
            self.ensure()
        self.assertEqual(len(self.fake.writes), 2)

    def test_removed_ready_vg_is_not_recreated(self):
        self.ensure()
        self.fake.vg = None
        self.fake.pv["vg_name"] = ""
        with self.assertRaisesRegex(StorageError, "missing"):
            self.ensure()
        self.assertEqual(len(self.fake.writes), 2)

    def test_replacement_vg_tag_is_rejected(self):
        self.ensure()
        self.fake.vg["vg_tags"] = "another-owner"
        with self.assertRaisesRegex(StorageError, "ownership"):
            self.ensure()

    def test_same_disk_cannot_be_reserved_twice(self):
        self.fake.crash_after = "pvcreate"
        with self.assertRaises(StorageError):
            self.ensure()
        self.fake.pv = None  # A prepared journal still reserves the identity.
        with self.assertRaisesRegex(StorageError, "reserved"):
            self.backend.ensure_pool("df-second", [DISK], approved=True, owner_uid="uid-second")

    def test_one_disk_per_vg_and_safe_name(self):
        with self.assertRaises(StorageError):
            self.backend.ensure_pool("df-test", [DISK, DISK], approved=True, owner_uid="uid-pool")
        with self.assertRaises(StorageError):
            self.backend.ensure_pool("../escape", [DISK], approved=True, owner_uid="uid-pool")
        self.assertEqual(self.fake.writes, [])

    def test_same_vg_cannot_be_claimed_by_another_pool_uid(self):
        self.ensure()
        with self.assertRaisesRegex(StorageError, "DiskPool UID"):
            self.ensure(owner_uid="uid-duplicate")
        self.assertEqual(len(self.fake.writes), 2)
        journal = json.loads((Path(self.temp.name) / "df-test.json").read_text())
        self.assertEqual(journal["poolUID"], "uid-pool")

    def test_existing_journal_binds_uid_before_initialization_retry(self):
        self.fake.crash_after = "pvcreate"
        with self.assertRaises(StorageError):
            self.ensure()
        with self.assertRaisesRegex(StorageError, "DiskPool UID"):
            self.ensure(owner_uid="uid-replacement")
        self.assertEqual(len(self.fake.writes), 1)
        self.ensure()
        self.assertEqual(len(self.fake.writes), 2)

    def test_verified_legacy_ready_journal_migrates_without_disk_writes(self):
        self.ensure()
        path = Path(self.temp.name) / "df-test.json"
        legacy = json.loads(path.read_text())
        legacy.pop("poolUID")
        path.write_text(json.dumps(legacy))
        with self.assertRaisesRegex(StorageError, "Legacy ownership"):
            self.ensure()
        self.assertEqual(json.loads(path.read_text()), legacy)
        self.ensure(allow_legacy_migration=True)
        self.assertEqual(json.loads(path.read_text()), dict(legacy, poolUID="uid-pool"))
        self.assertEqual(len(self.fake.writes), 2)

    def test_legacy_migration_requires_ready_phase_and_matching_media(self):
        self.ensure()
        path = Path(self.temp.name) / "df-test.json"
        legacy = json.loads(path.read_text())
        legacy.pop("poolUID")
        for mutation in ("phase", "pv", "tag", "missing-vg"):
            with self.subTest(mutation=mutation):
                journal = dict(legacy)
                original_pv, original_vg = copy.deepcopy(self.fake.pv), copy.deepcopy(self.fake.vg)
                if mutation == "phase": journal["phase"] = "pv-created"
                if mutation == "pv": self.fake.pv["pv_uuid"] = "external-uuid"
                if mutation == "tag": self.fake.vg["vg_tags"] = "external-owner"
                if mutation == "missing-vg": self.fake.vg = None
                path.write_text(json.dumps(journal))
                with self.assertRaises(StorageError):
                    self.ensure(allow_legacy_migration=True)
                self.assertEqual(json.loads(path.read_text()), journal)
                self.fake.pv, self.fake.vg = original_pv, original_vg
        self.assertEqual(len(self.fake.writes), 2)

    def test_empty_pool_uid_is_rejected_before_disk_commands(self):
        with self.assertRaisesRegex(StorageError, "UID"):
            self.ensure(owner_uid="")
        self.assertEqual(self.fake.calls, [])

    def test_two_processes_cannot_claim_one_vg_for_different_uids(self):
        # Exercise the actual flock across processes with file-backed fake LVM
        # state. No child invokes a real command or touches a block device.
        state = Path(self.temp.name) / "fake-lvm-state"
        state.write_text(json.dumps({"pv": None, "vg": None, "writes": []}))
        context = multiprocessing.get_context("fork")
        started, results = context.Event(), context.Queue()
        def claim(uid):
            def load_fake():
                data = json.loads(state.read_text())
                fake = FakeLVM()
                fake.pv, fake.vg = data["pv"], data["vg"]
                return fake, data
            def runner(argv):
                fake, data = load_fake()
                result = fake(argv)
                data.update(pv=fake.pv, vg=fake.vg)
                data["writes"].extend(fake.writes)
                state.write_text(json.dumps(data))
                return result
            backend = LVMBackend(self.temp.name, runner=runner,
                inventory=lambda: load_fake()[0].inventory(), device_validator=lambda p: None)
            started.wait(5)
            try:
                backend.ensure_pool("df-test", [DISK], approved=True, owner_uid=uid)
                results.put((uid, "Ready"))
            except StorageError as error:
                results.put((uid, str(error)))
        children = [context.Process(target=claim, args=(uid,)) for uid in ("first-uid", "second-uid")]
        try:
            for child in children: child.start()
            started.set()
            for child in children:
                child.join(10)
                self.assertEqual(child.exitcode, 0)
            outcomes = [results.get(timeout=2) for _ in children]
        finally:
            for child in children:
                if child.is_alive(): child.terminate(); child.join()
            results.close()
        self.assertEqual(sum(status == "Ready" for _, status in outcomes), 1)
        self.assertTrue(any("DiskPool UID" in status for _, status in outcomes))
        self.assertEqual(len(json.loads(state.read_text())["writes"]), 2)
        journal = json.loads((Path(self.temp.name) / "df-test.json").read_text())
        self.assertEqual(journal["poolUID"], next(uid for uid, status in outcomes if status == "Ready"))


class DiscoveryTests(unittest.TestCase):
    def alias_fixture(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = Path(temp.name)
        proc, sys, aliases = root / "proc", root / "sys", root / "by-id"
        for folder in (proc / "1", proc / "self", aliases,
                       sys / "class/block/vdb/holders"):
            folder.mkdir(parents=True)
        (proc / "1/mountinfo").write_text("")
        (proc / "self/mountinfo").write_text("")
        (proc / "swaps").write_text("Filename Type Size Used Priority\n")
        device = root / "vdb"
        device.touch()
        disk = {"name": "vdb", "kname": "vdb", "path": str(device),
                "type": "disk", "size": 100 * 1024**3, "serial": "disk-serial",
                "maj:min": "252:16"}
        return disk, {"runner": lambda argv: json.dumps({"blockdevices": [disk]}),
                      "sys_root": sys, "by_id": aliases, "proc_root": proc}

    def test_lvm_metadata_alias_does_not_replace_physical_identity(self):
        disk, options = self.alias_fixture()
        virtio = options["by_id"] / "virtio-disk-serial"
        virtio.symlink_to(disk["path"])
        before, = discover_disks(**options)
        self.assertEqual(before["path"], str(virtio))
        self.assertEqual(before["state"], "Available")

        # pvcreate introduces an alias that sorts before the hardware by-id.
        metadata = options["by_id"] / "lvm-pv-uuid-new-pv"
        metadata.symlink_to(disk["path"])
        self.assertLess(metadata.name, virtio.name)
        disk["fstype"] = "LVM2_member"
        (options["sys_root"] / "class/block/vdb/holders/dm-0").touch()
        after, = discover_disks(**options)
        for key in ("id", "path", "devicePath", "serial", "sizeBytes"):
            self.assertEqual(after[key], before[key], key)
        self.assertEqual(after["state"], "Protected")
        self.assertIn("Filesystem/signature detected: LVM2_member", after["reasons"])
        self.assertIn("Device has active holders (RAID/LVM/multipath)", after["reasons"])

    def test_lvm_metadata_alias_alone_is_not_a_physical_identity(self):
        disk, options = self.alias_fixture()
        (options["by_id"] / "lvm-pv-uuid-existing-pv").symlink_to(disk["path"])
        found, = discover_disks(**options)
        self.assertEqual(found["path"], disk["path"])
        self.assertEqual(found["state"], "Protected")
        self.assertIn("Stable /dev/disk/by-id path is missing", found["reasons"])

    def test_wwn_hardware_alias_retains_priority(self):
        disk, options = self.alias_fixture()
        for name in ("virtio-disk-serial", "wwn-physical-disk", "lvm-pv-uuid-existing-pv"):
            (options["by_id"] / name).symlink_to(disk["path"])
        found, = discover_disks(**options)
        self.assertEqual(found["path"], str(options["by_id"] / "wwn-physical-disk"))
        self.assertEqual(found["state"], "Available")

    def test_protect_root_partitions_holders_swap_and_missing_evidence(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            proc, sys, aliases = root / "proc", root / "sys", root / "by-id"
            for folder in [proc / "1", proc / "self", aliases]:
                folder.mkdir(parents=True)
            (proc / "1/mountinfo").write_text("21 0 8:1 / / rw - ext4 /dev/sda1 rw\n")
            (proc / "self/mountinfo").write_text("")
            (proc / "swaps").write_text("Filename Type Size Used Priority\n/dev/sdc partition 123 0 -2\n")
            devices = []
            for suffix in "abcd":
                name = "sd" + suffix
                holders = sys / "class/block" / name / "holders"
                holders.mkdir(parents=True)
                (aliases / ("wwn-" + name)).symlink_to("/dev/" + name)
                devices.append({"name": name, "kname": name, "path": "/dev/" + name,
                                "type": "disk", "size": 4 * 1024**3, "serial": name,
                                "maj:min": "8:" + str("abcd".index(suffix) * 16), "rota": True})
            devices[0]["children"] = [{"name": "sda1", "type": "part", "path": "/dev/sda1", "maj:min": "8:1"}]
            (sys / "class/block/sda1/holders").mkdir(parents=True)
            (sys / "class/block/sdb/holders/md0").touch()
            runner = lambda argv: json.dumps({"blockdevices": devices})
            disks = discover_disks(runner=runner, sys_root=sys, by_id=aliases, proc_root=proc)
            self.assertEqual([d["state"] for d in disks], ["Protected", "Protected", "Protected", "Available"])
            self.assertIn("Mounted device (includes host mounts)", disks[0]["reasons"])
            self.assertIn("Swap device", disks[2]["reasons"])
            (proc / "swaps").unlink()
            disks = discover_disks(runner=runner, sys_root=sys, by_id=aliases, proc_root=proc)
            self.assertTrue(all(d["state"] == "Protected" for d in disks))

    def test_no_shell_interpolation_in_command_runner(self):
        from diskfleet.storage import run_command
        with patch("diskfleet.storage.subprocess.run") as run:
            run.return_value.returncode = 0
            run.return_value.stdout = "{}"
            run_command(["lsblk", "$(never-executed)"])
            argv, = run.call_args.args
            self.assertEqual(argv, ["lsblk", "$(never-executed)"])
            self.assertNotIn("shell", run.call_args.kwargs)


if __name__ == "__main__":
    unittest.main()
