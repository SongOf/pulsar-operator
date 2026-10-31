package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeProm answers queries by substring, in the Prometheus HTTP API format.
type fakeProm struct {
	queries []string
	answer  func(query string) []map[string]any
}

func (f *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	f.queries = append(f.queries, q)
	result := f.answer(q)
	typ := "vector"
	if strings.HasSuffix(r.URL.Path, "query_range") {
		typ = "matrix"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": typ, "result": result}})
}

func matrix(labels map[string]string, values ...float64) map[string]any {
	pts := [][]any{}
	for i, v := range values {
		pts = append(pts, []any{float64(1_700_000_000 + 15*i), formatValue(v)})
	}
	return map[string]any{"metric": labels, "values": pts}
}

func vector(labels map[string]string, v string) map[string]any {
	return map[string]any{"metric": labels, "value": []any{1_700_000_000.0, v}}
}

func formatValue(v float64) string { b, _ := json.Marshal(v); return string(b) }

func TestBuild(t *testing.T) {
	f := &fakeProm{answer: func(q string) []map[string]any {
		switch {
		case q == "vector(1)":
			return []map[string]any{vector(nil, "1")}
		case strings.HasPrefix(q, "sum(pulsar_broker_rate_in"):
			return []map[string]any{matrix(nil, 10, 20, 30)}
		case strings.Contains(q, "bookie_ledger_dir_") && strings.HasPrefix(q, "max by (pod)"):
			return []map[string]any{matrix(map[string]string{"pod": "demo-bookie-0"}, 5, 6), matrix(map[string]string{"pod": "demo-bookie-1"}, 7, 8)}
		case strings.HasPrefix(q, "max by (pod) (bookkeeper_server_ADD_ENTRY{"):
			return []map[string]any{
				{"metric": map[string]string{"pod": "demo-bookie-0"}, "values": [][]any{{1_700_000_000.0, "NaN"}, {1_700_000_015.0, "2.5"}}},
				{"metric": map[string]string{"pod": "demo-bookie-1"}, "values": [][]any{{1_700_000_000.0, "NaN"}}},
			}
		case strings.HasPrefix(q, "count(up{") && strings.Contains(q, `component="bookie"`):
			return []map[string]any{vector(nil, "3")}
		case strings.HasPrefix(q, "count(bookie_SERVER_STATUS"):
			return []map[string]any{vector(nil, "2")}
		case strings.HasPrefix(q, "max by (pod, component) (up"):
			return []map[string]any{vector(map[string]string{"pod": "demo-broker-0", "component": "broker"}, "1"), vector(map[string]string{"pod": "demo-bookie-2", "component": "bookie"}, "0")}
		case strings.HasPrefix(q, "max by (pod) (jvm_memory_bytes_used"):
			return []map[string]any{vector(map[string]string{"pod": "demo-broker-0"}, "200000000")}
		case strings.HasPrefix(q, "sum by (pod) (rate(process_cpu_seconds_total"):
			return []map[string]any{vector(map[string]string{"pod": "demo-broker-0"}, "0.25")}
		}
		return nil
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	d, err := Build(context.Background(), Prometheus{URL: srv.URL}, "pulsar", "demo", time.Hour, time.Unix(1_700_000_100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Series["rateIn"]["cluster"]; len(got) != 3 || got[2][1] != 30 {
		t.Fatalf("rateIn = %v", got)
	}
	if len(d.Series["diskUsage"]) != 2 || d.Series["diskUsage"]["demo-bookie-1"][1][1] != 8 {
		t.Fatalf("diskUsage = %v", d.Series["diskUsage"])
	}
	if got := d.Series["addP99"]["demo-bookie-0"]; len(got) != 1 || got[0][1] != 2.5 {
		t.Fatalf("addP99 = %v; NaN points must be dropped", got)
	}
	if _, ok := d.Series["addP99"]["demo-bookie-1"]; ok {
		t.Fatal("a series without points was kept; the console cannot draw null")
	}
	if d.Current["bookies"] != 3 || d.Current["writableBookies"] != 2 {
		t.Fatalf("current = %v", d.Current)
	}
	if len(d.Pods) != 2 || d.Pods[1].Pod != "demo-broker-0" || !d.Pods[1].Up || d.Pods[1].HeapUsed != 2e8 || d.Pods[1].CPU != 0.25 || d.Pods[0].Up {
		t.Fatalf("pods = %+v", d.Pods)
	}
	for _, q := range f.queries {
		if q != "vector(1)" && !strings.Contains(q, `namespace="pulsar",pulsar_cluster="demo"`) {
			t.Fatalf("query without the cluster selector: %s", q)
		}
	}
}

func TestBuildFailsWhenPrometheusIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer srv.Close()
	if _, err := Build(context.Background(), Prometheus{URL: srv.URL}, "ns", "c", time.Hour, time.Now()); err == nil {
		t.Fatal("no error from an unreachable Prometheus")
	}
}

func TestBuildReportsFailedQueries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "vector(1)" {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}))
	defer srv.Close()
	d, err := Build(context.Background(), Prometheus{URL: srv.URL}, "ns", "c", time.Hour, time.Now())
	if err != nil || len(d.Errors) == 0 || !strings.Contains(d.Errors[0], "parse error") {
		t.Fatalf("err = %v errors = %v", err, d.Errors)
	}
}

func TestStepAndRange(t *testing.T) {
	if StepFor(time.Hour) != 15*time.Second || StepFor(7*24*time.Hour) != 42*time.Minute {
		t.Fatalf("steps = %v %v", StepFor(time.Hour), StepFor(7*24*time.Hour))
	}
	for in, want := range map[string]time.Duration{"": time.Hour, "15m": 15 * time.Minute, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour} {
		if got, err := ParseRange(in); err != nil || got != want {
			t.Fatalf("ParseRange(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"1m", "31d", "x", "-1h"} {
		if _, err := ParseRange(bad); err == nil {
			t.Fatalf("ParseRange(%q) accepted", bad)
		}
	}
}
