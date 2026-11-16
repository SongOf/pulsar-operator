//go:build systemintegration

package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	agentcontroller "github.com/SongOf/pulsar-operator/internal/agent/controller"
	agentmodel "github.com/SongOf/pulsar-operator/internal/agent/model"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type contractBackend struct {
	t              *testing.T
	uid            string
	vg             string
	called         bool
	poolCalls      int
	benchmarkCalls int
	benchmark      *storagev1.DiskPoolBenchmark
	alterBenchmark func(*storagev1.DiskPoolBenchmark)
}

func TestSystemNodeEnrollmentDiscoveryFreshnessAndPoolApproval(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, target := range []struct{ kind, namespace, name string }{{"pools", "", "pool-one"}, {"disks", "", "disk-one"}, {"pods", agentNamespace, "agent-one"}} {
		resource := poolsResource
		if target.kind == "disks" {
			resource = disksResource
		} else if target.kind == "pods" {
			resource = podsResource
		}
		if err := f.client.Tracker().Delete(resource, target.namespace, target.name); err != nil {
			t.Fatal(err)
		}
	}
	node := f.get(nodesResource, "", "worker-01")
	delete(asObject(at(node, "metadata", "labels")), label+"enabled")
	f.put(nodesResource, node)
	detail, err := f.cluster.Node(ctx, "worker-01", model.ListOptions{Limit: 50})
	if err != nil || at(detail, "node", "canEnableAgent") != true || detail["disks"].(model.Page).Total != 0 {
		t.Fatalf("joined node without an agent or disks is not visible: detail=%v error=%v", detail, err)
	}
	enrolled, err := f.cluster.EnableAgent(ctx, "worker-01", object{"uid": str(node, "metadata", "uid"), "resourceVersion": str(node, "metadata", "resourceVersion")})
	if err != nil || str(enrolled, "agentStatus") != "Pending" || enrolled["canCreatePool"] != false {
		t.Fatalf("enrollment must wait for the Agent: node=%v error=%v", enrolled, err)
	}
	f.put(podsResource, testAgentPod("worker-01"))
	disks := []agentmodel.Disk{{Path: "/dev/disk/by-id/wwn-one", Serial: "SER001", SizeBytes: 100 * model.GiB, Model: "test", Media: "SSD", State: "Available"}}
	backend := &contractBackend{t: t}
	agent := &agentcontroller.Reconciler{Client: f.client, Backend: backend, NodeName: "worker-01", ReconcileInterval: 30 * time.Second,
		Discover: func(context.Context) ([]agentmodel.Disk, error) { return disks, nil }}
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	detail, err = f.cluster.Node(ctx, "worker-01", model.ListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	page := detail["disks"].(model.Page)
	if len(page.Items) != 1 || page.Items[0]["canCreatePool"] != true || str(page.Items[0], "nodeUID") != "node-uid" {
		t.Fatalf("Agent report was not admitted by Storage: %+v", page)
	}
	diskID := str(page.Items[0], "id")
	body := object{"name": "fresh-node-pool", "node": "worker-01", "diskIds": []any{diskID}}
	plan, err := f.cluster.Plan(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	// A plan remains valid for longer than a scan; expiration of the scan
	// independently blocks submission, even if the last phase was Available.
	agent.Now = func() time.Time { return time.Now().Add(-time.Minute) }
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cluster.CreatePool(ctx, str(plan, "planId"), str(plan, "confirmation")); err == nil {
		t.Fatal("expired Agent observation permitted a DiskPool request")
	}
	before := f.get(disksResource, "", diskID)
	agent.Now = nil
	agent.Discover = func(context.Context) ([]agentmodel.Disk, error) { return nil, errors.New("discovery failed") }
	if err := agent.ReconcileOnce(ctx); err == nil {
		t.Fatal("failed discovery unexpectedly succeeded")
	}
	after := f.get(disksResource, "", diskID)
	if !equal(before["status"], after["status"]) {
		t.Fatal("a failed scan renewed the disk observation")
	}
	if _, err := f.cluster.Plan(ctx, body); err == nil {
		t.Fatal("failed scan made stale inventory usable")
	}
	agent.Discover = func(context.Context) ([]agentmodel.Disk, error) { return disks, nil }
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err = f.cluster.Plan(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := f.cluster.CreatePool(ctx, str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := f.client.Resource(poolsResource).Get(ctx, str(pool, "id"), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if str(created.Object, "status", "phase") != "WritesDisabled" || backend.called {
		t.Fatalf("request must preserve the disk-write switch: pool=%v backendCalled=%v error=%v", created, backend.called, err)
	}
}

func (b *contractBackend) EnsurePool(_ context.Context, vg string, devices []agentmodel.Device, approved bool, uid string, legacy bool) (agentmodel.PoolResult, error) {
	b.t.Helper()
	b.called = true
	b.poolCalls++
	if vg != b.vg || uid != b.uid || !approved || (legacy && b.poolCalls <= 2) || len(devices) != 1 || devices[0].Path != "/dev/disk/by-id/wwn-one" || devices[0].Serial != "SER001" || devices[0].SizeBytes != 100*model.GiB {
		b.t.Fatalf("storage API approval did not reach the agent unchanged: vg=%s uid=%s approved=%v legacy=%v devices=%+v", vg, uid, approved, legacy, devices)
	}
	return agentmodel.PoolResult{VGName: vg, CapacityBytes: 99 * model.GiB, AvailableBytes: 99 * model.GiB}, nil
}

func (b *contractBackend) EnsureBenchmark(ctx context.Context, request agentmodel.BenchmarkRequest, guard func(context.Context) error) (storagev1.DiskPoolBenchmark, error) {
	b.t.Helper()
	b.benchmarkCalls++
	if !b.called || !request.Approved || request.PoolUID != b.uid || request.NodeUID != "node-uid" || request.VGName != b.vg || request.RunID != poolstate.RunID(b.uid) || request.Device != (agentmodel.Device{Path: "/dev/disk/by-id/wwn-one", Serial: "SER001", SizeBytes: 100 * model.GiB}) {
		b.t.Fatalf("benchmark did not receive the approved pool and current node identity: %+v", request)
	}
	if guard == nil {
		b.t.Fatal("benchmark requires a live Kubernetes identity guard")
	}
	if err := guard(ctx); err != nil {
		return storagev1.DiskPoolBenchmark{}, err
	}
	if b.benchmark == nil {
		end := metav1.NewTime(time.Now().UTC().Add(-time.Second).Truncate(time.Second))
		start := metav1.NewTime(end.Add(-2 * time.Minute))
		result := &storagev1.DiskPoolBenchmark{PoolUID: request.PoolUID, NodeUID: request.NodeUID, VGName: request.VGName, Device: request.Device, RunID: request.RunID, Profile: poolstate.Profile, Phase: "Succeeded", StartedAt: &start, CompletedAt: &end, TestSizeBytes: poolstate.TestSizeBytes, CleanupVerified: true}
		for _, workload := range []struct {
			name  string
			block int64
		}{{"seq-write", 1 << 20}, {"seq-read", 1 << 20}, {"rand-write", 4096}, {"rand-read", 4096}} {
			result.Workloads = append(result.Workloads, storagev1.DiskBenchmarkWorkload{Name: workload.name, BlockSizeBytes: workload.block, QueueDepth: 16, DurationSeconds: 30, Bytes: 3 << 30, BandwidthBytesPerSecond: (3 << 30) / 30, IOPS: 1000, LatencyMeanNS: 100000, LatencyP95NS: 200000, LatencyP99NS: 300000})
		}
		if b.alterBenchmark != nil {
			b.alterBenchmark(result)
		}
		result.ResultDigest = poolstate.Digest(*result)
		b.benchmark = result
	}
	return *b.benchmark.DeepCopy(), nil
}

func newAgentPoolContract(t *testing.T) (*fixture, *agentcontroller.Reconciler, *contractBackend, string) {
	t.Helper()
	f := newFixture(t)
	plan := f.plan()
	created, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	id := str(created, "id")
	raw := f.get(poolsResource, "", id)
	node := f.get(nodesResource, "", "worker-01")
	asObject(at(node, "metadata", "labels"))["kubernetes.io/hostname"] = "rack-host-one"
	f.put(nodesResource, node)
	backend := &contractBackend{t: t, uid: str(raw, "metadata", "uid"), vg: str(raw, "spec", "vgName")}
	agent := &agentcontroller.Reconciler{Client: f.client, Backend: backend, NodeName: "worker-01", WritesEnabled: true, Discover: func(context.Context) ([]agentmodel.Disk, error) { return nil, nil }}
	return f, agent, backend, id
}

func contractReviewBody(raw object, decision string) object {
	return object{"uid": str(raw, "metadata", "uid"), "resourceVersion": str(raw, "metadata", "resourceVersion"), "runID": str(raw, "status", "benchmark", "runID"), "resultDigest": str(raw, "status", "benchmark", "resultDigest"), "decision": decision, "note": "Reviewed the recorded single-disk measurements"}
}

func assertContractUnpublished(t *testing.T, f *fixture, id string) {
	t.Helper()
	_, err := f.client.Resource(classesResource).Get(context.Background(), className(id), metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("unreviewed pool published a StorageClass: %v", err)
	}
	assertContractAllocationBlocked(t, f, id)
}

func assertContractAllocationBlocked(t *testing.T, f *fixture, id string) {
	t.Helper()
	view, err := f.cluster.Pool(context.Background(), id)
	if err != nil || view["canCreateVolume"] != false {
		t.Fatalf("pool detail advertised allocation before all live gates passed: %+v, %v", view, err)
	}
	f.client.ClearActions()
	_, err = f.cluster.CreateVolume(context.Background(), object{"name": "must-not-be-created", "poolId": id, "sizeGiB": int64(1)})
	assertProblem(t, err, 409)
	assertNoWrites(t, f.client)
}

// Only the test imports both modules. Production components communicate through
// Kubernetes declarations and run under separate processes and service accounts.
func TestSystemStorageApprovalAgentClassAndVolumeContract(t *testing.T) {
	ctx := context.Background()
	f, agent, backend, id := newAgentPoolContract(t)
	assertContractUnpublished(t, f, id)
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	raw := f.get(poolsResource, "", id)
	if !backend.called || backend.benchmarkCalls != 1 || str(raw, "status", "phase") != "AwaitingReview" {
		t.Fatalf("Agent must measure once and wait for human review: pool=%+v benchmarkCalls=%d", raw, backend.benchmarkCalls)
	}
	view, err := f.cluster.Pool(ctx, id)
	if err != nil || view["canReview"] != true || str(view, "allocationState") != "AwaitingReview" || len(array(at(view, "benchmark", "workloads"))) != 4 {
		t.Fatalf("Storage did not expose the complete reviewable Agent report: %+v, %v", view, err)
	}
	assertContractUnpublished(t, f, id)
	for _, field := range []string{"uid", "resourceVersion", "runID", "resultDigest"} {
		t.Run("stale-review-"+field, func(t *testing.T) {
			body := contractReviewBody(raw, "Approved")
			body[field] = "stale-identity"
			f.client.ClearActions()
			_, err := f.cluster.ReviewPool(ctx, id, body)
			assertProblem(t, err, 409)
			assertNoWrites(t, f.client)
		})
	}
	reviewed, err := f.cluster.ReviewPool(ctx, id, contractReviewBody(raw, "Approved"))
	if err != nil || str(reviewed, "allocationState") != "AwaitingStorageClass" || reviewed["canCreateVolume"] != false || reviewed["canReview"] != false {
		t.Fatalf("approval must wait for Agent publication: %+v, %v", reviewed, err)
	}
	approval := asObject(at(f.get(poolsResource, "", id), "spec", "allocationApproval"))
	if str(approval, "poolUID") != backend.uid || str(approval, "runID") != backend.benchmark.RunID || str(approval, "resultDigest") != backend.benchmark.ResultDigest || str(approval, "reviewedAt") == "" {
		t.Fatalf("review was not bound to the exact measured result: %+v", approval)
	}
	assertContractUnpublished(t, f, id)
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	raw = f.get(poolsResource, "", id)
	expected := storageClass(raw, "rack-host-one")
	actual := f.get(classesResource, "", str(expected, "metadata", "name"))
	if backend.benchmarkCalls != 1 || str(raw, "status", "phase") != "Ready" || str(raw, "status", "storageClass") != str(expected, "metadata", "name") || str(actual, "metadata", "annotations", label+"pool-uid") != backend.uid || !classMatches(actual, expected) {
		t.Fatalf("agent and storage API disagree on StorageClass ownership or parameters: actual=%+v expected=%+v", actual, expected)
	}
	volume, err := f.cluster.CreateVolume(ctx, object{"name": "from-agent-pool", "poolId": id, "sizeGiB": int64(1)})
	if err != nil || str(volume, "status") != "Pending" {
		t.Fatal(volume, err)
	}
	pvc := f.get(pvcsResource, "default", "from-agent-pool")
	if str(pvc, "spec", "storageClassName") != str(expected, "metadata", "name") {
		t.Fatal("PVC did not use the agent's verified StorageClass", pvc)
	}
	// Keep the previous Ready status and published class while changing live
	// identity/authorization behind Storage's cache. New requests must fail
	// immediately, without waiting for another Agent reconciliation.
	node := f.get(nodesResource, "", "worker-01")
	for _, scenario := range []string{"initialization-revoked", "pool-replaced", "node-replaced", "device-replaced", "report-changed", "approval-changed"} {
		t.Run("live-gate-"+scenario, func(t *testing.T) {
			changed := (&unstructured.Unstructured{Object: raw}).DeepCopy()
			switch scenario {
			case "initialization-revoked":
				asObject(changed.Object["spec"])["approved"] = false
			case "pool-replaced":
				changed.SetUID("replacement-pool")
			case "node-replaced":
				replacement := (&unstructured.Unstructured{Object: node}).DeepCopy()
				replacement.SetUID("replacement-node")
				if err := f.client.Tracker().Update(nodesResource, replacement, ""); err != nil {
					t.Fatal(err)
				}
			case "device-replaced":
				asObject(array(at(changed.Object, "spec", "devices"))[0])["serial"] = "REPLACEMENT"
			case "report-changed":
				asObject(at(changed.Object, "status", "benchmark"))["resultDigest"] = "changed-result"
			case "approval-changed":
				asObject(at(changed.Object, "spec", "allocationApproval"))["runID"] = "another-run"
			}
			if err := f.client.Tracker().Update(poolsResource, changed, ""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := f.client.Tracker().Update(poolsResource, &unstructured.Unstructured{Object: raw}, ""); err != nil {
					t.Error(err)
				}
				if err := f.client.Tracker().Update(nodesResource, &unstructured.Unstructured{Object: node}, ""); err != nil {
					t.Error(err)
				}
			})
			assertContractAllocationBlocked(t, f, id)
		})
	}
}

func TestSystemAgentFailedBenchmarkCannotBeReviewedOrAllocate(t *testing.T) {
	for _, scenario := range []string{"fio-failed", "cleanup-pending", "metric-errors"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f, agent, backend, id := newAgentPoolContract(t)
			backend.alterBenchmark = func(result *storagev1.DiskPoolBenchmark) {
				switch scenario {
				case "fio-failed":
					result.Phase, result.ErrorCode = "Failed", "FIOFailed"
					result.Workloads = result.Workloads[:2]
					result.Workloads[1].Errors = 1
					result.Workloads[1].DurationSeconds = 7
				case "cleanup-pending":
					result.CleanupVerified = false
				case "metric-errors":
					result.Workloads[0].Errors = 1
				}
			}
			if err := agent.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			raw := f.get(poolsResource, "", id)
			if str(raw, "status", "phase") != "BenchmarkFailed" || str(raw, "status", "benchmark", "resultDigest") != backend.benchmark.ResultDigest || len(array(at(raw, "status", "benchmark", "workloads"))) != len(backend.benchmark.Workloads) {
				t.Fatalf("failed or unsafe result was lost or admitted: %+v", raw)
			}
			f.client.ClearActions()
			_, err := f.cluster.ReviewPool(ctx, id, contractReviewBody(raw, "Approved"))
			assertProblem(t, err, 409)
			assertNoWrites(t, f.client)
			assertContractUnpublished(t, f, id)
			if err := agent.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if scenario != "cleanup-pending" && backend.benchmarkCalls != 1 {
				t.Fatal("completed failed results must not automatically rerun the benchmark")
			}
			assertContractUnpublished(t, f, id)
		})
	}
}

func TestSystemRejectedBenchmarkRemainsUnpublishedUntilNewReview(t *testing.T) {
	ctx := context.Background()
	f, agent, backend, id := newAgentPoolContract(t)
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	raw := f.get(poolsResource, "", id)
	rejected, err := f.cluster.ReviewPool(ctx, id, contractReviewBody(raw, "Rejected"))
	if err != nil || str(rejected, "allocationState") != "Rejected" {
		t.Fatalf("rejection did not persist: %+v, %v", rejected, err)
	}
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	raw = f.get(poolsResource, "", id)
	if str(raw, "status", "phase") != "Rejected" || backend.benchmarkCalls != 1 {
		t.Fatalf("rejection did not survive reconciliation: %+v", raw)
	}
	assertContractUnpublished(t, f, id)
	if _, err := f.cluster.ReviewPool(ctx, id, contractReviewBody(raw, "Approved")); err != nil {
		t.Fatal(err)
	}
	assertContractUnpublished(t, f, id)
	if err := agent.ReconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := f.cluster.Pool(ctx, id)
	if err != nil || view["canCreateVolume"] != true || backend.benchmarkCalls != 1 {
		t.Fatalf("new human review of the same immutable result did not unblock publication: %+v, %v", view, err)
	}
}
