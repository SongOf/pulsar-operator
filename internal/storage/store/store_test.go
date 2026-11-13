package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
)

func fixture(t *testing.T, seed bool) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if seed {
		if err := s.Seed(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	return s, root
}

func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	var problem *model.Problem
	if !errors.As(err, &problem) || problem.Status != status {
		t.Fatalf("expected status %d, got %v", status, err)
	}
}

func firstPool(t *testing.T, s *Store) model.Object {
	t.Helper()
	page, err := s.List(context.Background(), "pools", model.ListOptions{})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("missing pool: %+v %v", page, err)
	}
	return page.Items[0]
}

func makePlan(t *testing.T, s *Store) model.Object {
	t.Helper()
	plan, err := s.Plan(context.Background(), model.Object{"name": "test-pool", "node": "baremetal-001", "diskIds": []any{"disk-001-2"}})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestSeedAndPagination(t *testing.T) {
	s, _ := fixture(t, true)
	ctx := context.Background()
	if err := s.Seed(ctx); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		kind  string
		count int
	}{{"nodes", 24}, {"disks", 144}, {"pools", 6}, {"volumes", 6}} {
		page, err := s.List(ctx, item.kind, model.ListOptions{})
		if err != nil || page.Total != item.count {
			t.Fatalf("unexpected %s: %+v %v", item.kind, page, err)
		}
	}
	page, err := s.List(ctx, "disks", model.ListOptions{Limit: 3, Offset: 3, Node: "baremetal-001"})
	if err != nil || page.Total != 6 || len(page.Items) != 3 || page.Limit != 3 || page.Offset != 3 {
		t.Fatalf("invalid page: %+v %v", page, err)
	}
	for _, query := range []string{"%", "_", "!", "x' OR 1=1 --"} {
		page, err := s.List(ctx, "disks", model.ListOptions{Query: query})
		if err != nil || page.Total != 0 {
			t.Fatalf("search metacharacters were interpreted: %+v %v", page, err)
		}
	}
	page, err = s.List(ctx, "disks", model.ListOptions{State: "Protected"})
	if err != nil || page.Total != 24 {
		t.Fatalf("incorrect state filter: %+v %v", page, err)
	}
	page, err = s.List(ctx, "disks", model.ListOptions{Limit: 1000000})
	if err != nil || len(page.Items) != 144 {
		t.Fatalf("internal large page clipped: %+v %v", page, err)
	}
	overview, err := s.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	totals := overview["totals"].(model.Object)
	if totals["nodes"] != 24 || totals["volumes"] != 6 || model.Int64(totals["capacityBytes"]) != model.Int64(totals["availableBytes"])+model.Int64(totals["allocatedBytes"]) {
		t.Fatalf("incorrect overview: %+v", overview)
	}
	pool := firstPool(t, s)
	nodes, err := s.List(ctx, "nodes", model.ListOptions{Node: model.String(pool["node"])})
	if err != nil || len(nodes.Items) != 1 || model.Int64(nodes.Items[0]["allocatedBytes"]) != model.Int64(pool["allocatedBytes"]) {
		t.Fatalf("node allocation stale: %+v %v", nodes, err)
	}
}

func TestPlanProtectionConfirmationAndIdentity(t *testing.T) {
	s, _ := fixture(t, true)
	ctx := context.Background()
	for _, body := range []model.Object{
		{"name": "bad", "node": "baremetal-001", "diskIds": []any{"disk-001-0"}},
		{"name": "bad", "node": "baremetal-002", "diskIds": []any{"disk-001-2"}},
	} {
		_, err := s.Plan(ctx, body)
		requireStatus(t, err, 409)
	}
	plan := makePlan(t, s)
	_, err := s.CreatePool(ctx, model.String(plan["planId"]), "wrong")
	requireStatus(t, err, 409)
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		disk, err := s.Get(ctx, tx, "disks", "disk-001-2")
		if err != nil {
			return err
		}
		disk["serial"] = "REPLACED"
		return s.Put(ctx, tx, "disks", disk)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreatePool(ctx, model.String(plan["planId"]), model.String(plan["confirmation"]))
	requireStatus(t, err, 409)
	pools, err := s.List(ctx, "pools", model.ListOptions{})
	if err != nil || pools.Total != 6 {
		t.Fatalf("failed plan changed pool inventory: %+v %v", pools, err)
	}
}

func TestPlanReplayExpirationAndMissingConsumedObject(t *testing.T) {
	s, _ := fixture(t, true)
	ctx := context.Background()
	plan := makePlan(t, s)
	id, confirmation := model.String(plan["planId"]), model.String(plan["confirmation"])
	a, err := s.CreatePool(ctx, id, confirmation)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreatePool(ctx, id, confirmation)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(a)
	second, _ := json.Marshal(b)
	if string(first) != string(second) {
		t.Fatalf("plan replay changed result: %s %s", first, second)
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error { return s.Delete(ctx, tx, "pools", model.String(a["id"])) })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreatePool(ctx, id, confirmation)
	requireStatus(t, err, 404)
	err = s.Transaction(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, "UPDATE plans SET expires=0"); return err })
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreatePool(ctx, id, confirmation)
	requireStatus(t, err, 409)
}

func TestVolumeIdempotenceAndRetainAfterRestart(t *testing.T) {
	s, root := fixture(t, true)
	ctx := context.Background()
	pool := firstPool(t, s)
	body := model.Object{"name": "persist", "namespace": "test", "poolId": pool["id"], "sizeGiB": 2}
	a, err := s.CreateVolume(ctx, body)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateVolume(ctx, body)
	if err != nil || a["id"] != b["id"] {
		t.Fatalf("duplicate volume: %+v %+v %v", a, b, err)
	}
	body["sizeGiB"] = 3
	_, err = s.CreateVolume(ctx, body)
	requireStatus(t, err, 409)
	sentinel := filepath.Join(model.String(a["mountPath"]), "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("retained-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Expand(ctx, model.String(a["id"]), model.Object{"sizeGiB": 3}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Expand(ctx, model.String(a["id"]), model.Object{"sizeGiB": 1})
	requireStatus(t, err, 409)
	before := firstPool(t, s)
	if _, err := s.Release(ctx, model.String(a["id"]), ""); err != nil {
		t.Fatal(err)
	}
	_, err = s.Expand(ctx, model.String(a["id"]), model.Object{"sizeGiB": 4})
	requireStatus(t, err, 409)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	after := firstPool(t, restarted)
	if model.Int64(before["availableBytes"]) != model.Int64(after["availableBytes"]) {
		t.Fatal("release freed retained capacity")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "retained-data" {
		t.Fatalf("retained data lost: %q %v", data, err)
	}
	err = restarted.Transaction(ctx, func(tx *sql.Tx) error {
		volume, err := restarted.Get(ctx, tx, "volumes", model.String(a["id"]))
		if err != nil {
			return err
		}
		if volume["status"] != "Released" {
			t.Fatalf("release status lost: %+v", volume)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A reused PVC name receives a new directory; the original remains retained.
	newVolume, err := restarted.CreateVolume(ctx, body)
	if err != nil || newVolume["id"] == a["id"] || newVolume["mountPath"] == a["mountPath"] {
		t.Fatalf("name reuse reused data: %+v %v", newVolume, err)
	}
}

func TestConcurrentAllocationAcrossConnectionsNeverOvercommits(t *testing.T) {
	s, root := fixture(t, true)
	ctx := context.Background()
	pool := firstPool(t, s)
	other, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*Store{s, other}
	capacity := model.Int64(pool["availableBytes"])
	var accepted atomic.Int64
	var wg sync.WaitGroup
	errorsFound := make(chan error, 30)
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := stores[i%len(stores)].CreateVolume(ctx, model.Object{"name": fmt.Sprintf("race-%d", i), "poolId": pool["id"], "sizeGiB": 200})
			if err == nil {
				accepted.Add(1)
				return
			}
			var problem *model.Problem
			if !errors.As(err, &problem) || problem.Status != 409 {
				errorsFound <- err
			}
		}(i)
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
	if accepted.Load() != capacity/(200*model.GiB) {
		t.Fatalf("allocation count %d for capacity %d", accepted.Load(), capacity)
	}
	current := firstPool(t, s)
	if model.Int64(current["availableBytes"]) < 0 || model.Int64(current["capacityBytes"]) != model.Int64(current["availableBytes"])+model.Int64(current["allocatedBytes"]) {
		t.Fatalf("overcommitted pool: %+v", current)
	}
}

func TestConcurrentPlanCommitsOnlyConsumeDiskOnce(t *testing.T) {
	s, root := fixture(t, true)
	ctx := context.Background()
	plan := makePlan(t, s)
	other, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	results := make(chan model.Object, 2)
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			pool, err := store.CreatePool(ctx, model.String(plan["planId"]), model.String(plan["confirmation"]))
			results <- pool
			failures <- err
		}(store)
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id any
	for pool := range results {
		if id == nil {
			id = pool["id"]
		} else if id != pool["id"] {
			t.Fatalf("plan committed twice: %v %v", id, pool)
		}
	}
	page, err := s.List(ctx, "pools", model.ListOptions{})
	if err != nil || page.Total != 7 {
		t.Fatalf("unexpected pools: %+v %v", page, err)
	}
}

func TestTransactionRollbackAndDurableReleaseIntent(t *testing.T) {
	s, root := fixture(t, false)
	ctx := context.Background()
	intent := model.Object{"id": "retained-uid", "name": "data (retained)", "pvcUid": "uid", "pvcKey": "default~data", "sizeBytes": int64(9007199254740993), "status": "ReleaseRequested"}
	rejected := errors.New("cancel operation")
	err := s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := s.Put(ctx, tx, "release-intents", intent); err != nil {
			return err
		}
		if err := s.Event(ctx, tx, "requested", "message", "node", "info"); err != nil {
			return err
		}
		return rejected
	})
	if !errors.Is(err, rejected) {
		t.Fatal(err)
	}
	page, err := s.List(ctx, "release-intents", model.ListOptions{})
	if err != nil || page.Total != 0 {
		t.Fatalf("rolled-back intent survived: %+v %v", page, err)
	}
	events, err := s.Events(ctx, model.ListOptions{})
	if err != nil || events.Total != 0 {
		t.Fatalf("rolled-back audit survived: %+v %v", events, err)
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		if err := s.Put(ctx, tx, "release-intents", intent); err != nil {
			return err
		}
		return s.Event(ctx, tx, "requested", "message", "node", "info")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	err = restarted.Transaction(ctx, func(tx *sql.Tx) error {
		actual, err := restarted.Get(ctx, tx, "release-intents", "retained-uid")
		if err != nil {
			return err
		}
		if model.Int64(actual["sizeBytes"]) != 9007199254740993 {
			t.Fatal("large persisted integer lost precision")
		}
		if err := restarted.Put(ctx, tx, "retained", actual); err != nil {
			return err
		}
		return restarted.Delete(ctx, tx, "release-intents", "retained-uid")
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err = restarted.List(ctx, "release-intents", model.ListOptions{})
	if err != nil || page.Total != 0 {
		t.Fatalf("completed intent retained twice: %+v %v", page, err)
	}
	page, err = restarted.List(ctx, "retained", model.ListOptions{})
	if err != nil || page.Total != 1 {
		t.Fatalf("retained record missing: %+v %v", page, err)
	}
}

func TestPythonDatabaseFormatAndPlansRemainCompatible(t *testing.T) {
	root := t.TempDir()
	// This SQL dump was generated with the original Python Store and sqlite3
	// iterdump. No Python installation is required to run the Go regression test.
	fixture, err := os.ReadFile("testdata/python-state.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(fixture)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	page, err := s.List(ctx, "disks", model.ListOptions{})
	if err != nil || page.Total != 1 || model.Int64(page.Items[0]["sizeBytes"]) != 9007199254740993 {
		t.Fatalf("Python disk corrupted: %+v %v", page, err)
	}
	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil || journalMode != "wal" {
		t.Fatalf("WAL disabled: %s %v", journalMode, err)
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		plan, consumed, err := s.ReadPlan(ctx, tx, "python-plan", "接管 PYTHON-SERIAL")
		if err != nil {
			return err
		}
		if consumed || plan["vgName"] != "df_python" {
			t.Fatalf("Python plan changed: %+v %v", plan, consumed)
		}
		intent, err := s.Get(ctx, tx, "release-intents", "retained-old-pvc-uid")
		if err != nil {
			return err
		}
		if intent["pvcUid"] != "old-pvc-uid" || model.Int64(intent["sizeBytes"]) != 9007199254740993 {
			t.Fatalf("Python intent changed: %+v", intent)
		}
		return s.ConsumePlan(ctx, tx, "python-plan")
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, model.ListOptions{})
	if err != nil || events.Total != 1 || events.Items[0]["message"] != "原 Python 数据库兼容夹具" {
		t.Fatalf("Python audit changed: %+v %v", events, err)
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		_, consumed, err := s.ReadPlan(ctx, tx, "python-plan", "接管 PYTHON-SERIAL")
		if err != nil {
			return err
		}
		if !consumed {
			t.Fatal("plan consumption was not persisted")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMissingObjectsAndInvalidCapacities(t *testing.T) {
	s, _ := fixture(t, true)
	ctx := context.Background()
	pool := firstPool(t, s)
	for _, value := range []any{nil, true, "2", json.Number("1.0"), json.Number("1e3"), json.Number("9223372036854775808"), 0, -1, 1048577} {
		_, err := s.CreateVolume(ctx, model.Object{"name": "invalid", "poolId": pool["id"], "sizeGiB": value})
		requireStatus(t, err, 400)
	}
	_, err := s.CreateVolume(ctx, model.Object{"name": "missing", "poolId": "missing-pool", "sizeGiB": 1})
	requireStatus(t, err, 404)
	_, err = s.Expand(ctx, "missing-volume", model.Object{"sizeGiB": 1})
	requireStatus(t, err, 404)
	_, err = s.Release(ctx, "missing-volume", "")
	requireStatus(t, err, 404)
	_, err = s.Release(ctx, "../escape", "")
	requireStatus(t, err, 400)
	_, err = s.Plan(ctx, model.Object{"name": "missing", "node": "node", "diskIds": []any{"missing-disk"}})
	requireStatus(t, err, 404)
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		value, err := s.Get(ctx, tx, "retained", "missing-id")
		if value != nil {
			t.Fatalf("Get missing returned %+v", value)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSavePlanRoundTripDoesNotMutateInput(t *testing.T) {
	s, _ := fixture(t, false)
	ctx := context.Background()
	input := model.Object{"confirmation": "接管 设备", "sizeBytes": int64(9007199254740993)}
	copy := model.Object{"confirmation": "接管 设备", "sizeBytes": int64(9007199254740993)}
	plan, err := s.SavePlan(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, copy) {
		t.Fatalf("input mutated: %+v", input)
	}
	if plan["planId"] == "" || plan["expiresAt"] == "" {
		t.Fatalf("plan metadata missing: %+v", plan)
	}
	err = s.Transaction(ctx, func(tx *sql.Tx) error {
		value, consumed, err := s.ReadPlan(ctx, tx, model.String(plan["planId"]), "接管 设备")
		if err != nil {
			return err
		}
		if consumed || model.Int64(value["sizeBytes"]) != 9007199254740993 {
			t.Fatalf("bad plan roundtrip: %+v", value)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
