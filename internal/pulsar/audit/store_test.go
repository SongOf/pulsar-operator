package audit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPersistenceUnknownOutcomeAndSingleWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("allowed another audit process")
	}
	complete, err := s.Begin(ctx, Record{OperationID: "request-1", Source: "api", Resource: Resource{Name: "deleted-cluster", UID: "old-uid"}})
	if err != nil {
		t.Fatal(err)
	}
	complete.Result = "Succeeded"
	if err := s.Finish(ctx, complete); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, complete); err == nil {
		t.Fatal("rewrote a completed audit record")
	}
	if _, err := s.Begin(ctx, Record{OperationID: "request-2", Source: "controller", Resource: Resource{Name: "deleted-cluster", UID: "old-uid"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.List(ctx, Query{UID: "old-uid"})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("lost records: %#v, %v", page, err)
	}
	if page.Items[0].Result != "Unknown" || page.Items[1].Result != "Succeeded" {
		t.Fatalf("invented an outcome: %#v", page.Items)
	}
}

func TestCursorSurvivesConcurrentInsertsAndFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Begin(ctx, Record{OperationID: "op", Source: "controller", ClusterUID: "root-uid", Resource: Resource{Namespace: "ns", Name: "child"}}); err != nil {
			t.Fatal(err)
		}
	}
	query := Query{OperationID: "op", UID: "root-uid", Namespace: "ns", Limit: 2}
	first, err := s.List(ctx, query)
	if err != nil || len(first.Items) != 2 || first.NextCursor == 0 {
		t.Fatalf("first page: %#v, %v", first, err)
	}
	_, _ = s.Begin(ctx, Record{OperationID: "op", Source: "controller", ClusterUID: "root-uid", Resource: Resource{Namespace: "ns", Name: "child"}})
	query.Cursor = first.NextCursor
	second, err := s.List(ctx, query)
	if err != nil || len(second.Items) != 2 || second.Items[0].Sequence >= first.NextCursor {
		t.Fatalf("unstable cursor: %#v, %v", second, err)
	}
	query.UID = "replacement-uid"
	empty, err := s.List(ctx, query)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("mixed same-name replacements: %#v, %v", empty, err)
	}
}

func TestChangesRedactValuesButKeepOperationalDiff(t *testing.T) {
	before := []byte(`{"metadata":{"resourceVersion":"1"},"spec":{"broker":{"replicas":2},"config":{"password":"OLD_SECRET","image":"OTHER_SECRET"},"template":{"spec":{"containers":[{"env":[{"name":"TOKEN","value":"ENV_SECRET"}]}]}}},"data":{"key":"DATA_SECRET"}}`)
	after := []byte(`{"metadata":{"resourceVersion":"2"},"spec":{"broker":{"replicas":3},"config":{"password":"NEW_SECRET","image":"NEW_OTHER_SECRET"},"template":{"spec":{"containers":[{"env":[{"name":"TOKEN","value":"NEW_ENV_SECRET"}]}]}}},"data":{"key":"NEW_DATA_SECRET"}}`)
	changes, truncated := Diff(before, after, false)
	encoded, _ := json.Marshal(changes)
	if truncated || strings.Contains(string(encoded), "SECRET") {
		t.Fatalf("secret leak: %s", encoded)
	}
	found := false
	for _, change := range changes {
		if change.Path == "/spec/broker/replicas" {
			found = change.Before == json.Number("2") && change.After == json.Number("3") && !change.Redacted
		}
	}
	if !found {
		t.Fatalf("lost operational values: %s", encoded)
	}
	if changes, _ := Diff([]byte(`{"status":{"phase":"Running","observedGeneration":1,"bookies":{"readyReplicas":2}}}`), []byte(`{"status":{"phase":"Running","observedGeneration":2,"bookies":{"readyReplicas":3}}}`), true); len(changes) != 0 {
		t.Fatalf("routine observations produced audit noise: %#v", changes)
	}
}
