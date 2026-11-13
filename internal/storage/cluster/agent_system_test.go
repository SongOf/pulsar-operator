//go:build systemintegration

package cluster

import (
	"context"
	"testing"

	agentcontroller "github.com/SongOf/pulsar-operator/internal/agent/controller"
	agentmodel "github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

type contractBackend struct {
	t      *testing.T
	uid    string
	vg     string
	called bool
}

func (b *contractBackend) EnsurePool(_ context.Context, vg string, devices []agentmodel.Device, approved bool, uid string, legacy bool) (agentmodel.PoolResult, error) {
	b.t.Helper()
	b.called = true
	if vg != b.vg || uid != b.uid || !approved || legacy || len(devices) != 1 || devices[0].Path != "/dev/disk/by-id/wwn-one" || devices[0].Serial != "SER001" || devices[0].SizeBytes != 100*model.GiB {
		b.t.Fatalf("storage API approval did not reach the agent unchanged: vg=%s uid=%s approved=%v legacy=%v devices=%+v", vg, uid, approved, legacy, devices)
	}
	return agentmodel.PoolResult{VGName: vg, CapacityBytes: 99 * model.GiB, AvailableBytes: 99 * model.GiB}, nil
}

// Only the test imports both modules. Production components communicate through
// Kubernetes declarations and run under separate processes and service accounts.
func TestSystemStorageApprovalAgentClassAndVolumeContract(t *testing.T) {
	f := newFixture(t)
	plan := f.plan()
	created, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	raw := f.get(poolsResource, "", str(created, "id"))
	node := f.get(nodesResource, "", "worker-01")
	asObject(at(node, "metadata", "labels"))["kubernetes.io/hostname"] = "rack-host-one"
	f.put(nodesResource, node)
	backend := &contractBackend{t: t, uid: str(raw, "metadata", "uid"), vg: str(raw, "spec", "vgName")}
	agent := &agentcontroller.Reconciler{Client: f.client, Backend: backend, NodeName: "worker-01", WritesEnabled: true, Discover: func(context.Context) ([]agentmodel.Disk, error) { return nil, nil }}
	if err := agent.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !backend.called {
		t.Fatal("agent did not recognize the storage module's DiskPool declaration")
	}
	raw = f.get(poolsResource, "", str(created, "id"))
	expected := storageClass(raw, "rack-host-one")
	actual := f.get(classesResource, "", str(expected, "metadata", "name"))
	if str(raw, "status", "phase") != "Ready" || str(raw, "status", "storageClass") != str(expected, "metadata", "name") || str(actual, "metadata", "annotations", label+"pool-uid") != backend.uid || !classMatches(actual, expected) {
		t.Fatalf("agent and storage API disagree on StorageClass ownership or parameters: actual=%+v expected=%+v", actual, expected)
	}
	volume, err := f.cluster.CreateVolume(context.Background(), object{"name": "from-agent-pool", "poolId": str(created, "id"), "sizeGiB": int64(1)})
	if err != nil || str(volume, "status") != "Pending" {
		t.Fatal(volume, err)
	}
	pvc := f.get(pvcsResource, "default", "from-agent-pool")
	if str(pvc, "spec", "storageClassName") != str(expected, "metadata", "name") {
		t.Fatal("PVC did not use the agent's verified StorageClass", pvc)
	}
}
