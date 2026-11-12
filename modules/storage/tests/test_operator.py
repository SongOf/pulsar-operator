import copy
import unittest
from unittest.mock import Mock

from diskfleet.operator import (BASE, FINALIZER, GROUP, NODE_LABEL, NodeOperator,
                                disk_name, node_label, storage_class, storage_class_name)


class FakeError(Exception):
    def __init__(self, status):
        self.status = status
        super().__init__(str(status))


class FakeKube:
    def __init__(self):
        self.objects = {}
        self.calls = []

    def request(self, method, path, body=None):
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == "GET":
            if path not in self.objects:
                raise FakeError(404)
            return copy.deepcopy(self.objects[path])
        if method == "POST":
            target = path + "/" + body["metadata"]["name"]
            if target in self.objects:
                raise FakeError(409)
            obj = copy.deepcopy(body)
            obj["metadata"].update(uid=obj["metadata"].get("uid", "uid-" + obj["metadata"]["name"]), resourceVersion="1")
            self.objects[target] = obj
            return copy.deepcopy(obj)
        if method == "PATCH":
            target = path.removesuffix("/status")
            current = self.objects[target]
            current["metadata"].update(body.get("metadata", {}))
            current["metadata"]["resourceVersion"] = str(int(current["metadata"].get("resourceVersion", "0")) + 1)
            if "status" in body:
                current["status"] = copy.deepcopy(body["status"])
            return copy.deepcopy(current)
        raise AssertionError((method, path))

    def list(self, path, label_selector=None, field_selector=None):
        objects = [copy.deepcopy(obj) for target, obj in self.objects.items()
                   if target.rsplit("/", 1)[0] == path]
        if not label_selector:
            return objects
        key, value = label_selector.split("=", 1)
        return [obj for obj in objects if obj["metadata"].get("labels", {}).get(key) == value]


def make_pool(node="worker-01", approved=True):
    return {"metadata": {"name": "ssd-01", "uid": "uid-pool", "resourceVersion": "1", "generation": 1,
                         "labels": {NODE_LABEL: node}},
            "spec": {"nodeName": node, "vgName": "df_ssd01", "approved": approved,
                     "devices": [{"path": "/dev/disk/by-id/wwn-test", "serial": "SER001", "sizeBytes": 100 * 1024**3}]}}


class OperatorTests(unittest.TestCase):
    def setUp(self):
        self.kube = FakeKube()
        self.kube.objects['/api/v1/nodes/worker-01'] = dict(metadata=dict(name='worker-01',labels={'kubernetes.io/hostname':'worker-01'}))
        self.backend = Mock()
        self.backend.ensure_pool.return_value = {"capacityBytes": 90 * 1024**3,
                                                "availableBytes": 80 * 1024**3, "vgName": "df_ssd01"}
        self.agent = NodeOperator(self.kube, self.backend, "worker-01", discover=lambda: [], writes_enabled=True)

    def add_pool(self, **kw):
        pool = make_pool(**kw)
        self.kube.objects[f"{BASE}/diskpools/ssd-01"] = copy.deepcopy(pool)
        return pool

    def test_node_is_required(self):
        with self.assertRaises(ValueError):
            NodeOperator(self.kube, self.backend, "", discover=lambda: [])

    def test_both_write_gates_are_required(self):
        pool = self.add_pool(approved=False)
        self.agent.reconcile_pool(pool)
        self.assertEqual(pool["status"]["phase"], "PendingApproval")
        self.backend.ensure_pool.assert_not_called()
        pool["spec"]["approved"] = True
        self.agent.writes_enabled = False
        self.agent.reconcile_pool(pool)
        self.assertEqual(pool["status"]["phase"], "WritesDisabled")
        self.backend.ensure_pool.assert_not_called()

    def test_ready_pool_gets_safe_storage_class(self):
        pool = self.add_pool()
        self.agent.reconcile_pool(pool)
        self.backend.ensure_pool.assert_called_once_with("df_ssd01", pool["spec"]["devices"], approved=True, owner_uid="uid-pool", allow_legacy_migration=False)
        self.assertIn(FINALIZER, pool["metadata"]["finalizers"])
        self.assertEqual(pool["status"]["phase"], "Ready")
        sc = self.kube.objects["/apis/storage.k8s.io/v1/storageclasses/diskfleet-ssd-01"]
        self.assertEqual(sc["reclaimPolicy"], "Retain")
        self.assertEqual(sc["volumeBindingMode"], "WaitForFirstConsumer")
        self.assertEqual(sc["parameters"]["vgpattern"], "^df_ssd01$")
        self.assertEqual(sc["parameters"]["thinProvision"], "no")
        self.assertEqual(sc["allowedTopologies"][0]["matchLabelExpressions"][0]["values"], ["worker-01"])

    def test_repeated_reconcile_does_not_churn_status(self):
        pool = self.add_pool()
        self.agent.reconcile_pool(pool)
        self.kube.calls.clear()
        self.agent.reconcile_pool(pool)
        self.assertFalse(any(method == "PATCH" for method, _, _ in self.kube.calls))

    def test_foreign_node_is_ignored_even_if_in_list(self):
        pool = self.add_pool(node="worker-02")
        self.agent.reconcile_pool(pool)
        self.backend.ensure_pool.assert_not_called()
        self.assertFalse(self.kube.calls)

    def test_deletion_retains_media_and_does_not_reconcile(self):
        pool = self.add_pool()
        pool["metadata"].update(deletionTimestamp="2026-11-13T00:00:00Z", finalizers=[FINALIZER])
        self.agent.reconcile_pool(pool)
        self.backend.ensure_pool.assert_not_called()
        self.assertEqual(pool["status"]["phase"], "Retained")
        self.assertIn(FINALIZER, pool["metadata"]["finalizers"])

    def test_existing_storage_class_owner_is_checked(self):
        pool = self.add_pool()
        sc = storage_class(pool)
        sc["metadata"]["annotations"][f"{GROUP}/pool-uid"] = "someone-else"
        self.kube.objects["/apis/storage.k8s.io/v1/storageclasses/diskfleet-ssd-01"] = sc
        with self.assertLogs("diskfleet.operator", level="ERROR"):
            self.agent.reconcile_pool(pool)
        self.assertEqual(pool["status"]["phase"], "Error")
        self.assertIn("another pool", pool["status"]["message"])

    def test_class_drift_fails_closed(self):
        pool = self.add_pool()
        sc = storage_class(pool)
        sc["reclaimPolicy"] = "Delete"
        self.kube.objects["/apis/storage.k8s.io/v1/storageclasses/diskfleet-ssd-01"] = sc
        with self.assertLogs("diskfleet.operator", level="ERROR"):
            self.agent.reconcile_pool(pool)
        self.assertEqual(pool["status"]["phase"], "Error")

    def test_disk_identity_is_not_silently_updated(self):
        disk = {"path": "/dev/disk/by-id/wwn-test", "serial": "SER001", "sizeBytes": 100 * 1024**3,
                "model": "TEST", "media": "SSD", "state": "Available", "reasons": []}
        self.agent.discover = lambda: [dict(disk)]
        self.agent.reconcile_disks([])
        name = disk_name("worker-01", disk)
        current = self.kube.objects[f"{BASE}/physicaldisks/{name}"]
        self.assertEqual(current["status"]["phase"], "Available")
        disk["sizeBytes"] *= 2
        self.agent.reconcile_disks([])
        self.assertEqual(current["status"]["phase"], "Protected")
        self.assertNotEqual(current["spec"]["sizeBytes"], disk["sizeBytes"])

    def test_missing_disk_is_offline(self):
        disk = {"path": "/dev/disk/by-id/wwn-test", "serial": "SER001", "sizeBytes": 100 * 1024**3,
                "model": "TEST", "media": "SSD", "state": "Available", "reasons": []}
        self.agent.discover = lambda: [disk]
        self.agent.reconcile_disks([])
        self.agent.discover = lambda: []
        self.agent.reconcile_disks([])
        current = self.kube.objects[f"{BASE}/physicaldisks/{disk_name('worker-01', disk)}"]
        self.assertEqual(current["status"]["phase"], "Offline")

    def test_class_names_are_bounded_and_stable(self):
        a = storage_class_name("a" * 100)
        b = storage_class_name("a" * 99 + "b")
        self.assertLessEqual(len(a), 63)
        self.assertNotEqual(a, b)
        self.assertEqual(a, storage_class_name("a" * 100))

    def test_topology_uses_actual_hostname_instead_of_node_name(self):
        self.kube.objects['/api/v1/nodes/worker-01']['metadata']['labels']['kubernetes.io/hostname']='actual-rack-host'
        pool=self.add_pool()
        self.agent.reconcile_pool(pool)
        sc=self.kube.objects['/apis/storage.k8s.io/v1/storageclasses/diskfleet-ssd-01']
        self.assertEqual(sc['allowedTopologies'][0]['matchLabelExpressions'][0]['values'],['actual-rack-host'])
        self.assertEqual(pool['status']['topologyHostname'],'actual-rack-host')

    def test_missing_hostname_fails_before_disk_mutation(self):
        self.kube.objects['/api/v1/nodes/worker-01']['metadata']['labels']={}
        pool=self.add_pool()
        with self.assertLogs('diskfleet.operator',level='ERROR'):
            self.agent.reconcile_pool(pool)
        self.assertEqual(pool['status']['phase'],'Error')
        self.backend.ensure_pool.assert_not_called()

    def test_long_node_name_is_hashed_for_label_only(self):
        node='worker.'+'a'*60+'.example.com'
        self.assertLessEqual(len(node_label(node)),63)
        self.assertEqual(node_label('worker.example.com'),'worker.example.com')
        pool=make_pool(node=node)
        pool['metadata']['labels'][NODE_LABEL]=node_label(node)
        self.kube.objects[BASE+'/diskpools/ssd-01']=pool
        agent=NodeOperator(self.kube,self.backend,node,discover=lambda:[])
        self.assertEqual(agent._list('diskpools')[0]['spec']['nodeName'],node)

    def test_duplicate_claim_even_without_node_label_blocks_backend(self):
        pool = self.add_pool()
        duplicate = copy.deepcopy(pool)
        duplicate['metadata'].update(name='other-pool', uid='other-uid', labels={})
        for mutation in ('same-vg', 'same-serial', 'same-path'):
            with self.subTest(mutation=mutation):
                other = copy.deepcopy(duplicate)
                if mutation != 'same-vg': other['spec']['vgName'] = 'df_other'
                if mutation == 'same-serial': other['spec']['devices'][0]['path'] = '/dev/disk/by-id/another-alias'
                if mutation == 'same-path': other['spec']['devices'][0]['serial'] = 'different-serial'
                self.kube.objects[BASE+'/diskpools/other-pool'] = other
                with self.assertLogs('diskfleet.operator', level='ERROR'):
                    self.agent.reconcile_pool(pool)
                self.assertEqual(pool['status']['phase'], 'Error')
                self.assertIn('also claimed', pool['status']['message'])
        self.backend.ensure_pool.assert_not_called()

    def test_legacy_migration_is_authorized_only_by_existing_owned_class(self):
        pool = self.add_pool()
        pool['status'] = dict(phase='Ready', storageClass='diskfleet-ssd-01', topologyHostname='worker-01')
        path = '/apis/storage.k8s.io/v1/storageclasses/diskfleet-ssd-01'
        self.kube.objects[path] = storage_class(pool)
        self.agent.reconcile_pool(pool)
        self.assertTrue(self.backend.ensure_pool.call_args.kwargs['allow_legacy_migration'])
        self.kube.objects[path]['metadata']['annotations'][GROUP+'/pool-uid'] = 'replaced-uid'
        self.assertFalse(self.agent._legacy_owner_proven(pool, 'worker-01'))
        self.kube.objects[path] = storage_class(pool)
        self.kube.objects[path]['reclaimPolicy'] = 'Delete'
        self.assertFalse(self.agent._legacy_owner_proven(pool, 'worker-01'))


if __name__ == "__main__":
    unittest.main()
