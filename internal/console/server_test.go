package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
)

func setup(t *testing.T, token string) (*httptest.Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	pc := &v1.PulsarCluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pc", UID: "pc-uid"},
		Spec: v1.PulsarClusterSpec{
			ZooKeeper:  v1.ZooKeeperSpec{Storage: v1.VolumeSpec{Size: resource.MustParse("1Gi")}},
			BookKeeper: v1.BookKeeperSpec{Replicas: 3},
			Broker:     v1.BrokerSpec{Replicas: 2},
		},
	}
	child := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pc",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PulsarCluster", Name: "pc", UID: "pc-uid", Controller: ptr.To(true)}}}}
	standalone := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk"}, Spec: v1.BookKeeperClusterSpec{BookKeeperSpec: v1.BookKeeperSpec{Replicas: 3}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pc, child, standalone).Build()
	srv := &Server{Client: c, WebRoot: t.TempDir(), Token: token, Now: func() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, c
}

func do(t *testing.T, method, url, body, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestListGroupsChildBookKeeper(t *testing.T) {
	ts, _ := setup(t, "")
	resp, body := do(t, "GET", ts.URL+"/api/clusters", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var list ClusterList
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Pulsar) != 1 || len(list.BookKeeper) != 1 || list.BookKeeper[0].Name != "bk" {
		t.Fatalf("list = %+v", list)
	}
	if _, ok := list.Children["ns/pc"]; !ok {
		t.Fatal("child bookkeeper not attached to its PulsarCluster")
	}
}

func TestPatchScalesBrokers(t *testing.T) {
	ts, c := setup(t, "")
	resp, body := do(t, "PATCH", ts.URL+"/api/pulsarclusters/ns/pc", `{"spec":{"broker":{"replicas":5}}}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	pc := &v1.PulsarCluster{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pc"}, pc)
	if pc.Spec.Broker.Replicas != 5 || pc.Spec.BookKeeper.Replicas != 3 {
		t.Fatalf("spec = %+v", pc.Spec)
	}
}

func TestPatchOnlySpec(t *testing.T) {
	ts, _ := setup(t, "")
	resp, _ := do(t, "PATCH", ts.URL+"/api/pulsarclusters/ns/pc", `{"metadata":{"labels":{"x":"y"}}}`, "")
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestManagedBookKeeperIsReadOnly(t *testing.T) {
	ts, _ := setup(t, "")
	for _, req := range [][2]string{{"PATCH", "/api/bookkeeperclusters/ns/pc"}, {"DELETE", "/api/bookkeeperclusters/ns/pc"}, {"POST", "/api/bookkeeperclusters/ns/pc/restart?component=bookie"}} {
		resp, body := do(t, req[0], ts.URL+req[1], `{"spec":{"replicas":1}}`, "")
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("%s %s: status %d %s", req[0], req[1], resp.StatusCode, body)
		}
	}
	resp, _ := do(t, "PATCH", ts.URL+"/api/bookkeeperclusters/ns/bk", `{"spec":{"replicas":4}}`, "")
	if resp.StatusCode != 200 {
		t.Fatalf("standalone patch status %d", resp.StatusCode)
	}
}

func TestRestartSetsRestartedAt(t *testing.T) {
	ts, c := setup(t, "")
	for _, comp := range []string{"broker", "bookie", "autorecovery"} {
		resp, body := do(t, "POST", ts.URL+"/api/pulsarclusters/ns/pc/restart?component="+comp, "", "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d %s", comp, resp.StatusCode, body)
		}
	}
	pc := &v1.PulsarCluster{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "pc"}, pc)
	want := "2026-11-01T12:00:00Z"
	if pc.Spec.Broker.RestartedAt != want || pc.Spec.BookKeeper.RestartedAt != want || pc.Spec.BookKeeper.AutoRecovery.RestartedAt != want {
		t.Fatalf("restartedAt = %q %q %q", pc.Spec.Broker.RestartedAt, pc.Spec.BookKeeper.RestartedAt, pc.Spec.BookKeeper.AutoRecovery.RestartedAt)
	}
	resp, _ := do(t, "POST", ts.URL+"/api/pulsarclusters/ns/pc/restart?component=nope", "", "")
	if resp.StatusCode != 400 {
		t.Fatalf("unknown component status %d", resp.StatusCode)
	}
}

func TestTokenRequired(t *testing.T) {
	ts, _ := setup(t, "s3cret")
	if resp, _ := do(t, "GET", ts.URL+"/api/clusters", "", ""); resp.StatusCode != 401 {
		t.Fatalf("no token: status %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/api/clusters", "", "wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong token: status %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/api/clusters", "", "s3cret"); resp.StatusCode != 200 {
		t.Fatalf("right token: status %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/healthz", "", ""); resp.StatusCode != 200 {
		t.Fatal("healthz needs no token")
	}
}

func TestCreateRejectsUnknownFields(t *testing.T) {
	ts, _ := setup(t, "")
	resp, _ := do(t, "POST", ts.URL+"/api/pulsarclusters", `{"metadata":{"name":"x","namespace":"ns"},"spec":{"brokers":3}}`, "")
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
