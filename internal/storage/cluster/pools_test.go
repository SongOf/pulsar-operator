package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

func benchmarkFixture(raw object, nodeUID string) {
	pool, err := typedPool(raw)
	if err != nil {
		panic(err)
	}
	end := metav1.NewTime(time.Now().UTC().Add(-time.Second).Truncate(time.Second))
	start := metav1.NewTime(end.Add(-2 * time.Minute))
	b := &storagev1.DiskPoolBenchmark{PoolUID: string(pool.UID), NodeUID: nodeUID, VGName: pool.Spec.VGName, Device: pool.Spec.Devices[0], RunID: poolstate.RunID(string(pool.UID)), Profile: poolstate.Profile, Phase: "Succeeded", StartedAt: &start, CompletedAt: &end, TestSizeBytes: poolstate.TestSizeBytes, CleanupVerified: true}
	for _, w := range []struct {
		name  string
		block int64
	}{{"seq-write", 1 << 20}, {"seq-read", 1 << 20}, {"rand-write", 4096}, {"rand-read", 4096}} {
		b.Workloads = append(b.Workloads, storagev1.DiskBenchmarkWorkload{Name: w.name, BlockSizeBytes: w.block, QueueDepth: poolstate.QueueDepth, DurationSeconds: 30, Bytes: 1 << 30, BandwidthBytesPerSecond: 1 << 25, IOPS: 1000, LatencyMeanNS: 100, LatencyP95NS: 200, LatencyP99NS: 300})
	}
	b.ResultDigest = poolstate.Digest(*b)
	data, _ := json.Marshal(b)
	var decoded object
	if err := json.Unmarshal(data, &decoded); err != nil {
		panic(err)
	}
	if raw["status"] == nil {
		raw["status"] = object{}
	}
	asObject(raw["status"])["benchmark"] = decoded
	asObject(raw["spec"])["allocationApproval"] = object{"poolUID": b.PoolUID, "runID": b.RunID, "resultDigest": b.ResultDigest, "decision": "Approved", "reviewedAt": model.Now()}
}
func TestLegacyReadyPoolCannotAllocateButExistingVolumesRemainManaged(t *testing.T) {
	f := newFixture(t)
	raw := f.get(poolsResource, "", "pool-one")
	delete(asObject(raw["status"]), "benchmark")
	delete(asObject(raw["spec"]), "allocationApproval")
	f.put(poolsResource, raw)
	f.boundVolume(true)
	pool, err := f.cluster.Pool(context.Background(), "pool-one")
	if err != nil || pool["allocationState"] != "LegacyUnreviewed" || pool["canCreateVolume"] != false || pool["canReview"] != false {
		t.Fatal(pool, err)
	}
	_, err = f.cluster.CreateVolume(context.Background(), object{"name": "new-data", "poolId": "pool-one", "sizeGiB": int64(1)})
	assertProblem(t, err, 409)
	if volumes := f.volumes(); len(volumes) != 1 || volumes[0]["canManage"] != true {
		t.Fatal(volumes)
	}
	if _, err := f.cluster.Expand(context.Background(), "default~data", object{"sizeGiB": int64(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cluster.Release(context.Background(), "default~data", "pvc-uid"); err != nil {
		t.Fatal(err)
	}
}

func reviewFixture(t *testing.T) (*fixture, object) {
	t.Helper()
	f := newFixture(t)
	raw := f.get(poolsResource, "", "pool-one")
	delete(asObject(raw["spec"]), "allocationApproval")
	asObject(raw["status"])["phase"] = "AwaitingReview"
	asObject(raw["status"])["storageClass"] = ""
	f.put(poolsResource, raw)
	if err := f.client.Tracker().Delete(classesResource, "", className("pool-one")); err != nil {
		t.Fatal(err)
	}
	return f, object{"uid": "pool-uid", "resourceVersion": "1", "runID": str(raw, "status", "benchmark", "runID"), "resultDigest": str(raw, "status", "benchmark", "resultDigest"), "decision": "Approved"}
}
func TestReviewPersistsIntentBeforeOptimisticPatchAndWaitsForClass(t *testing.T) {
	f, body := reviewFixture(t)
	f.client.PrependReactor("patch", "diskpools", func(action k8stesting.Action) (bool, runtime.Object, error) {
		events, err := f.store.Events(context.Background(), model.ListOptions{Limit: 100})
		if err != nil || len(events.Items) != 1 || events.Items[0]["action"] != "pool.review-requested" {
			t.Fatalf("intent not durable: %#v %v", events, err)
		}
		var patch object
		if err := json.Unmarshal(action.(k8stesting.PatchAction).GetPatch(), &patch); err != nil {
			t.Fatal(err)
		}
		if str(patch, "metadata", "uid") != "pool-uid" || str(patch, "metadata", "resourceVersion") != "1" || str(patch, "spec", "allocationApproval", "reviewedAt") == "" {
			t.Fatalf("missing preconditions: %#v", patch)
		}
		return false, nil, nil
	})
	got, err := f.cluster.ReviewPool(context.Background(), "pool-one", body)
	if err != nil || got["canReview"] != false || got["canCreateVolume"] != false || got["allocationState"] != "AwaitingStorageClass" {
		t.Fatal(got, err)
	}
	events, err := f.store.Events(context.Background(), model.ListOptions{Limit: 100})
	if err != nil || len(events.Items) != 2 || events.Items[0]["action"] != "pool.reviewed" {
		t.Fatal(events, err)
	}
	body["decision"], body["note"] = "Rejected", "cannot revoke"
	_, err = f.cluster.ReviewPool(context.Background(), "pool-one", body)
	assertProblem(t, err, 409)
	raw := f.get(poolsResource, "", "pool-one")
	asObject(raw["status"])["phase"], asObject(raw["status"])["storageClass"] = "Ready", className("pool-one")
	f.put(poolsResource, raw)
	f.put(classesResource, storageClass(raw, "worker-01"))
	got, err = f.cluster.Pool(context.Background(), "pool-one")
	if err != nil || got["canCreateVolume"] != true {
		t.Fatal(got, err)
	}
	if _, err = f.cluster.CreateVolume(context.Background(), object{"name": "approved-data", "poolId": "pool-one", "sizeGiB": int64(1)}); err != nil {
		t.Fatal(err)
	}
}
func TestReviewRejectsStaleOrUnsafeResultWithoutPatch(t *testing.T) {
	for _, scenario := range []string{"uid", "version", "digest", "node-replaced", "not-cleaned", "incomplete", "future", "class-exists", "deleting"} {
		t.Run(scenario, func(t *testing.T) {
			f, body := reviewFixture(t)
			raw := f.get(poolsResource, "", "pool-one")
			switch scenario {
			case "uid":
				body["uid"] = "another-pool"
			case "version":
				body["resourceVersion"] = "old"
			case "digest":
				body["resultDigest"] = "other-result"
			case "node-replaced":
				node := f.get(nodesResource, "", "worker-01")
				asObject(node["metadata"])["uid"] = "new-node"
				f.put(nodesResource, node)
			case "not-cleaned":
				asObject(at(raw, "status", "benchmark"))["cleanupVerified"] = false
			case "incomplete":
				asObject(at(raw, "status", "benchmark"))["workloads"] = []any{}
			case "future":
				pool, _ := typedPool(raw)
				end := metav1.NewTime(time.Now().Add(time.Hour))
				pool.Status.Benchmark.CompletedAt = &end
				pool.Status.Benchmark.ResultDigest = poolstate.Digest(*pool.Status.Benchmark)
				data, _ := json.Marshal(pool.Status.Benchmark)
				var b object
				if err := json.Unmarshal(data, &b); err != nil {
					t.Fatal(err)
				}
				asObject(raw["status"])["benchmark"] = b
				body["resultDigest"] = b["resultDigest"]
			case "class-exists":
				f.put(classesResource, storageClass(raw, "worker-01"))
			case "deleting":
				asObject(raw["metadata"])["deletionTimestamp"] = model.Now()
			}
			f.put(poolsResource, raw)
			patches := 0
			f.client.PrependReactor("patch", "diskpools", func(k8stesting.Action) (bool, runtime.Object, error) { patches++; return false, nil, nil })
			_, err := f.cluster.ReviewPool(context.Background(), "pool-one", body)
			assertProblem(t, err, 409)
			if patches != 0 {
				t.Fatal("unsafe approval patch attempted")
			}
		})
	}
}
func TestReviewPatchConflictHasDurableFailureWithoutErrorLeak(t *testing.T) {
	f, body := reviewFixture(t)
	f.client.PrependReactor("patch", "diskpools", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(poolsResource.GroupResource(), "pool-one", errors.New("secret-backend-response"))
	})
	_, err := f.cluster.ReviewPool(context.Background(), "pool-one", body)
	if !apierrors.IsConflict(err) {
		t.Fatal(err)
	}
	events, err := f.store.Events(context.Background(), model.ListOptions{Limit: 100})
	if err != nil || len(events.Items) != 2 || events.Items[0]["action"] != "pool.review-failed" || events.Items[1]["action"] != "pool.review-requested" {
		t.Fatal(events, err)
	}
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), "secret-backend-response") {
		t.Fatal("audit leaked backend error")
	}
	if at(f.get(poolsResource, "", "pool-one"), "spec", "allocationApproval") != nil {
		t.Fatal("failed patch changed decision")
	}
}
func TestCreatePoolRetryPreservesAllocationApproval(t *testing.T) {
	f := newFixture(t)
	plan := f.plan()
	pool, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil {
		t.Fatal(err)
	}
	raw := f.get(poolsResource, "", str(pool, "id"))
	benchmarkFixture(raw, "node-uid")
	f.put(poolsResource, raw)
	retry, err := f.cluster.CreatePool(context.Background(), str(plan, "planId"), str(plan, "confirmation"))
	if err != nil || retry["allocationApproval"] == nil {
		t.Fatal(retry, err)
	}
}

func TestReviewCannotPatchWithoutDurableAudit(t *testing.T) {
	f, body := reviewFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	writes := 0
	f.client.PrependReactor("patch", "diskpools", func(k8stesting.Action) (bool, runtime.Object, error) { writes++; return false, nil, nil })
	if _, err := f.cluster.ReviewPool(context.Background(), "pool-one", body); err == nil {
		t.Fatal("approval succeeded without audit")
	}
	if writes != 0 {
		t.Fatal("approval patched before durable intent")
	}
}
func TestPoolAllocationRequiresActualExpectedClass(t *testing.T) {
	for _, kind := range []string{"name", "configuration", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "name":
				raw := f.get(poolsResource, "", "pool-one")
				asObject(raw["status"])["storageClass"] = "different-class"
				f.put(poolsResource, raw)
			case "configuration":
				raw := f.get(classesResource, "", className("pool-one"))
				asObject(raw["parameters"])["vgpattern"] = ".*"
				f.put(classesResource, raw)
			case "missing":
				if err := f.client.Tracker().Delete(classesResource, "", className("pool-one")); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.cluster.Pool(context.Background(), "pool-one")
			if err != nil || got["canCreateVolume"] != false {
				t.Fatal(got, err)
			}
			page, err := f.cluster.List(context.Background(), "pools", model.ListOptions{Limit: 10})
			if err != nil || len(page.Items) != 1 || page.Items[0]["canCreateVolume"] != false {
				t.Fatal(page, err)
			}
		})
	}
}
