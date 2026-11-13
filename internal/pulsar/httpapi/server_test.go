package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
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
	child := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pc", UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "PulsarCluster", Name: "pc", UID: "pc-uid", Controller: ptr.To(true)}}}}
	standalone := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "bk", UID: "bk-uid"}, Spec: v1.BookKeeperClusterSpec{BookKeeperSpec: v1.BookKeeperSpec{Replicas: 3}}}
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
	// Existing happy-path tests act on a freshly reviewed API snapshot. Race and
	// missing-precondition tests below construct their requests explicitly.
	if method == "PATCH" || method == "DELETE" || (method == "POST" && strings.Contains(url, "/restart")) {
		resourceURL := strings.Split(url, "/restart")[0]
		response, snapshot := do(t, "GET", resourceURL, "", token)
		if response.StatusCode == http.StatusOK {
			var object struct {
				Metadata metav1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(snapshot), &object); err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-Resource-UID", string(object.Metadata.UID))
			req.Header.Set("X-Resource-Version", object.Metadata.ResourceVersion)
		}
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
	if len(list.AllBookKeeper) != 2 || list.AllBookKeeper[0].Name != "bk" || list.AllBookKeeper[1].Name != "pc" {
		t.Fatalf("complete sorted BookKeeper inventory = %+v", list.AllBookKeeper)
	}
}

func inventoryServer(t *testing.T, objects ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Server{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
}

func inventoryList(t *testing.T, s *Server) ClusterList {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/clusters", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status %d: %s", w.Code, w.Body.String())
	}
	var list ClusterList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func inventoryPair(namespace string) (*v1.PulsarCluster, *v1.BookKeeperCluster) {
	pc := &v1.PulsarCluster{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "pc", UID: types.UID(namespace + "-parent")}}
	bk := &v1.BookKeeperCluster{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: pc.Name, UID: types.UID(namespace + "-child"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: v1.GroupVersion.String(), Kind: "PulsarCluster", Name: pc.Name,
			UID: pc.UID, Controller: ptr.To(true),
		}},
	}}
	return pc, bk
}

func TestListVerifiesParentIdentity(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*v1.PulsarCluster, *v1.BookKeeperCluster)
		omitParent bool
		linked     bool
		standalone bool
	}{
		{name: "verified canonical child", linked: true},
		{name: "valid owner but noncanonical child", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.Name = "extra-child" }},
		{name: "duplicate controller references", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) {
			bk.OwnerReferences = append(bk.OwnerReferences, bk.OwnerReferences[0])
		}},
		{name: "multiple controlling kinds", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) {
			other := bk.OwnerReferences[0]
			other.Kind = "OtherController"
			bk.OwnerReferences = append(bk.OwnerReferences, other)
		}},
		{name: "parent was recreated", mutate: func(pc *v1.PulsarCluster, _ *v1.BookKeeperCluster) { pc.UID = "replacement" }},
		{name: "parent UID missing", mutate: func(pc *v1.PulsarCluster, _ *v1.BookKeeperCluster) { pc.UID = "" }},
		{name: "owner UID missing", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.OwnerReferences[0].UID = "" }},
		{name: "wrong API group", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) {
			bk.OwnerReferences[0].APIVersion = "other.example/v1alpha1"
		}},
		{name: "missing API version", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) {
			bk.OwnerReferences[0].APIVersion = "pulsar.songof.dev/"
		}},
		{name: "non-controller reference", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.OwnerReferences[0].Controller = ptr.To(false) }},
		{name: "controller flag missing", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.OwnerReferences[0].Controller = nil }},
		{name: "wrong parent name", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.OwnerReferences[0].Name = "another-parent" }},
		{name: "cross namespace reference", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.Namespace = "other" }},
		{name: "parent missing", omitParent: true},
		{name: "another controlling kind", mutate: func(_ *v1.PulsarCluster, bk *v1.BookKeeperCluster) { bk.OwnerReferences[0].Kind = "OtherController" }},
		{name: "same name without ownership is standalone", standalone: true, mutate: func(pc *v1.PulsarCluster, bk *v1.BookKeeperCluster) {
			bk.Name = pc.Name
			bk.OwnerReferences = nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, bk := inventoryPair("ns")
			if tc.mutate != nil {
				tc.mutate(pc, bk)
			}
			objects := []client.Object{bk}
			if !tc.omitParent {
				objects = append(objects, pc)
			}
			list := inventoryList(t, inventoryServer(t, objects...))
			if len(list.AllBookKeeper) != 1 || list.AllBookKeeper[0].UID != bk.UID {
				t.Fatalf("resource missing from complete inventory: %+v", list.AllBookKeeper)
			}
			if (len(list.Children) == 1) != tc.linked {
				t.Fatalf("children = %+v, linked = %v", list.Children, tc.linked)
			}
			if (len(list.BookKeeper) == 1) != tc.standalone {
				t.Fatalf("standalone list = %+v, standalone = %v", list.BookKeeper, tc.standalone)
			}
		})
	}
}

func TestListSeparatesNamespacesWithDuplicateNames(t *testing.T) {
	a, childA := inventoryPair("a")
	b, childB := inventoryPair("b")
	list := inventoryList(t, inventoryServer(t, b, childB, a, childA))
	if len(list.Children) != 2 || len(list.AllBookKeeper) != 2 || len(list.BookKeeper) != 0 {
		t.Fatalf("inventory = %+v", list)
	}
	for _, namespace := range []string{"a", "b"} {
		child, exists := list.Children[namespace+"/pc"]
		if !exists || child.Namespace != namespace || child.OwnerReferences[0].UID != types.UID(namespace+"-parent") {
			t.Fatalf("wrong namespace relationship: %+v", list.Children)
		}
	}
	if list.AllBookKeeper[0].Namespace != "a" || list.AllBookKeeper[1].Namespace != "b" {
		t.Fatalf("inventory is not sorted: %+v", list.AllBookKeeper)
	}
}

func TestListKeepsNoncanonicalOwnedBookKeeperReadOnly(t *testing.T) {
	pc, child := inventoryPair("ns")
	extra := child.DeepCopy()
	extra.Name, extra.UID = "a-noncanonical", "extra-uid"
	list := inventoryList(t, inventoryServer(t, pc, extra, child))
	if len(list.AllBookKeeper) != 2 || len(list.BookKeeper) != 0 || len(list.Children) != 1 {
		t.Fatalf("inventory = %+v", list)
	}
	if got := list.Children["ns/pc"]; got.Name != pc.Name || got.UID != child.UID {
		t.Fatalf("parent linked to a resource it does not reconcile: %+v", got)
	}
}

func TestOrphanBookKeeperCannotBeChangedDirectly(t *testing.T) {
	for _, mutation := range []string{"stale UID", "wrong group", "non-controller", "other controller", "noncanonical child", "multiple controllers"} {
		t.Run(mutation, func(t *testing.T) {
			pc, bk := inventoryPair("ns")
			switch mutation {
			case "stale UID":
				bk.OwnerReferences[0].UID = "old-parent"
			case "wrong group":
				bk.OwnerReferences[0].APIVersion = "other.example/v1"
			case "non-controller":
				bk.OwnerReferences[0].Controller = ptr.To(false)
			case "other controller":
				bk.OwnerReferences[0].Kind = "OtherController"
			case "noncanonical child":
				bk.Name = "extra-child"
			case "multiple controllers":
				bk.OwnerReferences = append(bk.OwnerReferences, bk.OwnerReferences[0])
			}
			s := inventoryServer(t, pc, bk)
			path := "/api/bookkeeperclusters/ns/" + bk.Name
			for _, request := range [][2]string{
				{http.MethodPatch, path},
				{http.MethodDelete, path},
				{http.MethodPost, path + "/restart?component=bookie"},
			} {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, httptest.NewRequest(request[0], request[1], strings.NewReader(`{"spec":{"replicas":1}}`)))
				if w.Code != http.StatusConflict {
					t.Fatalf("%s: status %d: %s", request[0], w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestMetricsRejectsUnverifiedPulsarParent(t *testing.T) {
	for _, mutation := range []string{"recreated parent", "noncanonical child", "multiple controllers"} {
		t.Run(mutation, func(t *testing.T) {
			pc, bk := inventoryPair("ns")
			switch mutation {
			case "recreated parent":
				pc.UID = "replacement-parent"
			case "noncanonical child":
				bk.Name = "extra-child"
			case "multiple controllers":
				bk.OwnerReferences = append(bk.OwnerReferences, bk.OwnerReferences[0])
			}
			s := inventoryServer(t, pc, bk)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/bookkeeperclusters/ns/"+bk.Name+"/metrics", nil))
			if w.Code != http.StatusConflict {
				t.Fatalf("metrics accepted invalid relationship: status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestBookKeeperEventsMatchResourceKindAndUID(t *testing.T) {
	pc, bk := inventoryPair("ns")
	bk.Name = pc.Name
	event := func(name, kind string, uid types.UID) *corev1.Event {
		return &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			InvolvedObject: corev1.ObjectReference{APIVersion: v1.GroupVersion.String(), Kind: kind,
				Namespace: "ns", Name: pc.Name, UID: uid},
			Reason: name,
		}
	}
	s := inventoryServer(t, pc, bk,
		event("current-bookie", "BookKeeperCluster", bk.UID),
		event("same-name-parent", "PulsarCluster", pc.UID),
		event("replaced-bookie", "BookKeeperCluster", "previous-uid"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/bookkeeperclusters/ns/pc/events", nil))
	var got []Event
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(got) != 1 || got[0].Reason != "current-bookie" {
		t.Fatalf("events status %d: %s", w.Code, w.Body.String())
	}
}

func TestBookKeeperPodsExcludeBrokerComponents(t *testing.T) {
	pc, bk := inventoryPair("ns")
	bk.Name = pc.Name
	objects := []client.Object{pc, bk}
	for _, component := range []string{"bookie", "autorecovery", "broker", "zookeeper", "proxy"} {
		objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "pc-" + component,
			Labels: map[string]string{"app.kubernetes.io/instance": pc.Name,
				"app.kubernetes.io/managed-by": "pulsar-operator", "app.kubernetes.io/component": component},
		}})
	}
	s := inventoryServer(t, objects...)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/bookkeeperclusters/ns/pc/pods", nil))
	var got []Pod
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(got) != 2 || got[0].Component != "autorecovery" || got[1].Component != "bookie" {
		t.Fatalf("pods status %d: %s", w.Code, w.Body.String())
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

func TestCreateRejectsUnusableClusterNames(t *testing.T) {
	for _, kind := range []string{"pulsarclusters", "bookkeeperclusters"} {
		for _, name := range []string{strings.Repeat("a", 52), "1cluster", "cluster.name"} {
			body, _ := json.Marshal(map[string]any{"metadata": map[string]string{"name": name, "namespace": "ns"}})
			response := httptest.NewRecorder()
			// A nil client proves invalid names are rejected before a Kubernetes write.
			(&Server{}).Handler().ServeHTTP(response, httptest.NewRequest("POST", "/api/"+kind, strings.NewReader(string(body))))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("%s %s: status %d", kind, name, response.Code)
			}
		}
	}
}
