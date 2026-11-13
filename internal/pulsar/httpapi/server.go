// Package httpapi serves the web console and the JSON API it uses. Every operation reads or changes the
// PulsarCluster and BookKeeperCluster resources; the controllers do the actual work.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"io"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"net/http"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sort"
	"strings"
	"time"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/audit"
	"github.com/SongOf/pulsar-operator/internal/pulsar/metrics"
	"github.com/SongOf/pulsar-operator/internal/pulsar/resources"
)

// Server is the console HTTP handler.
type Server struct {
	Client  client.Client
	Audit   *audit.Store
	WebRoot string
	// Token, when set, is required as `Authorization: Bearer <token>` on every API request.
	Token string
	Now   func() time.Time
	// HTTP queries Prometheus; a 15s timeout when nil.
	HTTP *http.Client
}

// Handler routes the console.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/clusters", s.auth(s.listClusters))
	mux.HandleFunc("GET /api/audit", s.auth(s.listAudit))
	mux.HandleFunc("POST /api/pulsarclusters", s.auth(s.auditAction("create", s.createPulsar)))
	mux.HandleFunc("POST /api/bookkeeperclusters", s.auth(s.auditAction("create", s.createBookKeeper)))
	mux.HandleFunc("GET /api/{kind}/{namespace}/{name}", s.auth(s.getCluster))
	mux.HandleFunc("PATCH /api/{kind}/{namespace}/{name}", s.auth(s.auditAction("patch", s.patchCluster)))
	mux.HandleFunc("DELETE /api/{kind}/{namespace}/{name}", s.auth(s.auditAction("delete", s.deleteCluster)))
	mux.HandleFunc("POST /api/{kind}/{namespace}/{name}/restart", s.auth(s.auditAction("restart", s.restart)))
	mux.HandleFunc("GET /api/{kind}/{namespace}/{name}/pods", s.auth(s.pods))
	mux.HandleFunc("GET /api/{kind}/{namespace}/{name}/events", s.auth(s.events))
	mux.HandleFunc("GET /api/{kind}/{namespace}/{name}/metrics", s.auth(s.metrics))
	mux.Handle("/", s.static())
	return mux
}
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, errors.New("missing or wrong console token"))
				return
			}
		}
		h(w, r)
	}
}

// static serves the built frontend, falling back to index.html for client-side routes.
func (s *Server) static() http.Handler {
	files := http.FileServer(http.Dir(s.WebRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.WebRoot, filepath.Clean("/"+r.URL.Path))
		if _, err := os.Stat(path); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// ClusterList supports separate broker and bookie inventories while retaining the
// original standalone list and parent-to-child lookup for older clients.
type ClusterList struct {
	Pulsar        []v1.PulsarCluster     `json:"pulsar"`
	BookKeeper    []v1.BookKeeperCluster `json:"bookkeeper"`
	AllBookKeeper []v1.BookKeeperCluster `json:"allBookkeeper"`
	// Children contains only verified controller relationships. Orphans remain
	// visible in AllBookKeeper, without being presented as writable standalone clusters.
	Children map[string]v1.BookKeeperCluster `json:"children"`
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pcs := &v1.PulsarClusterList{}
	if err := s.Client.List(ctx, pcs); err != nil {
		writeError(w, status(err), err)
		return
	}
	bks := &v1.BookKeeperClusterList{}
	if err := s.Client.List(ctx, bks); err != nil {
		writeError(w, status(err), err)
		return
	}
	out := ClusterList{
		Pulsar: pcs.Items, BookKeeper: []v1.BookKeeperCluster{},
		AllBookKeeper: append([]v1.BookKeeperCluster{}, bks.Items...),
		Children:      map[string]v1.BookKeeperCluster{},
	}
	sort.Slice(out.Pulsar, func(i, j int) bool { return key(&out.Pulsar[i]) < key(&out.Pulsar[j]) })
	sort.Slice(out.AllBookKeeper, func(i, j int) bool { return key(&out.AllBookKeeper[i]) < key(&out.AllBookKeeper[j]) })
	parents := map[string]*v1.PulsarCluster{}
	for i := range out.Pulsar {
		parents[key(&out.Pulsar[i])] = &out.Pulsar[i]
	}
	for _, bk := range out.AllBookKeeper {
		parent := parents[bk.Namespace+"/"+pulsarOwner(&bk)]
		if parent != nil && pulsarOwnedBy(&bk, parent) {
			out.Children[key(parent)] = bk
		} else if !managed(&bk) {
			out.BookKeeper = append(out.BookKeeper, bk)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
func key(o metav1.Object) string { return o.GetNamespace() + "/" + o.GetName() }

func validPulsarOwner(ref metav1.OwnerReference) bool {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == v1.GroupVersion.Group && gv.Version != "" &&
		ref.Kind == "PulsarCluster" && ref.Controller != nil && *ref.Controller &&
		ref.Name != "" && ref.UID != ""
}

func pulsarOwner(bk *v1.BookKeeperCluster) string {
	controllers, owner := 0, ""
	for _, ref := range bk.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			controllers++
			// ChildBookKeeper() always reconciles the same namespace/name as
			// its PulsarCluster. An owner reference on a differently named object
			// cannot make parent writes target that object.
			if validPulsarOwner(ref) && bk.Name == ref.Name {
				owner = ref.Name
			}
		}
	}
	if controllers != 1 {
		return ""
	}
	return owner
}

func pulsarOwnedBy(bk *v1.BookKeeperCluster, pc *v1.PulsarCluster) bool {
	if bk.Namespace != pc.Namespace || bk.Name != pc.Name || pc.UID == "" || pulsarOwner(bk) != pc.Name {
		return false
	}
	for _, ref := range bk.OwnerReferences {
		if validPulsarOwner(ref) && ref.Name == pc.Name && ref.UID == pc.UID {
			return true
		}
	}
	return false
}
func (s *Server) createPulsar(w http.ResponseWriter, r *http.Request) {
	pc := &v1.PulsarCluster{}
	if err := decode(r, pc); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := v1.ValidateClusterName(pc.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.auditTarget(r, pc)
	audit.Stamp(r.Context(), pc, 1)
	if err := s.Client.Create(r.Context(), pc); err != nil {
		writeError(w, status(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, pc)
}
func (s *Server) createBookKeeper(w http.ResponseWriter, r *http.Request) {
	bk := &v1.BookKeeperCluster{}
	if err := decode(r, bk); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := v1.ValidateBookKeeperName(bk.Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.auditTarget(r, bk)
	audit.Stamp(r.Context(), bk, 1)
	if err := s.Client.Create(r.Context(), bk); err != nil {
		writeError(w, status(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, bk)
}

// object returns an empty object of the URL's kind.
func object(kind string) (client.Object, error) {
	switch kind {
	case "pulsarclusters":
		return &v1.PulsarCluster{}, nil
	case "bookkeeperclusters":
		return &v1.BookKeeperCluster{}, nil
	}
	return nil, fmt.Errorf("unknown kind %q", kind)
}
func (s *Server) load(r *http.Request) (client.Object, error) {
	obj, err := object(r.PathValue("kind"))
	if err != nil {
		return nil, err
	}
	err = s.Client.Get(r.Context(), types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}, obj)
	if err == nil {
		s.auditTarget(r, obj)
	}
	return obj, err
}

func (s *Server) getCluster(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

// Missing or stale owners never make a managed BookKeeperCluster safe to mutate directly.
var errManaged = errors.New("this BookKeeperCluster has an owner and cannot be changed directly; use its verified parent or resolve the owner reference")
var errResourceChanged = errors.New("resource identity or version changed; reload the resource, review your changes, and retry")

// The browser must identify the version it actually reviewed. Checking the
// cached Get alone would silently authorize changes to a same-name replacement.
func matchResource(w http.ResponseWriter, r *http.Request, obj client.Object) bool {
	uid, version := r.Header.Get("X-Resource-UID"), r.Header.Get("X-Resource-Version")
	if uid == "" || version == "" {
		writeError(w, http.StatusPreconditionRequired, errors.New("X-Resource-UID and X-Resource-Version are required; reload the resource before changing it"))
		return false
	}
	if len(r.Header.Values("X-Resource-UID")) != 1 || len(r.Header.Values("X-Resource-Version")) != 1 {
		writeError(w, http.StatusBadRequest, errors.New("resource precondition headers must not be repeated"))
		return false
	}
	if uid != string(obj.GetUID()) || version != obj.GetResourceVersion() {
		writeError(w, http.StatusConflict, errResourceChanged)
		return false
	}
	return true
}

// Preserve merge-patch semantics for spec, then test identity and version in the
// same atomic Kubernetes patch. A stale cache or concurrent owner change cannot
// turn the earlier managed() check into permission to change another object.
func (s *Server) patchSpec(r *http.Request, obj client.Object, merge []byte) error {
	original, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	merged, err := jsonpatch.MergePatch(original, merge)
	if err != nil {
		return err
	}
	var updated struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(merged, &updated); err != nil {
		return err
	}
	if len(updated.Spec) == 0 {
		updated.Spec = json.RawMessage("null")
	}
	operations := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": obj.GetUID()},
		{"op": "test", "path": "/metadata/resourceVersion", "value": obj.GetResourceVersion()},
		{"op": "add", "path": "/spec", "value": updated.Spec},
	}
	if audit.FromContext(r.Context()) != nil {
		generation := obj.GetGeneration()
		if !jsonpatch.Equal(original, merged) {
			generation++
		}
		stamped := obj.DeepCopyObject().(client.Object)
		audit.Stamp(r.Context(), stamped, generation)
		operations = append(operations, map[string]any{"op": "add", "path": "/metadata/annotations", "value": stamped.GetAnnotations()})
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		return err
	}
	err = s.Client.Patch(r.Context(), obj, client.RawPatch(types.JSONPatchType, patch))
	// Kubernetes returns a bare 422 for a failed JSON Patch test, unlike the
	// detailed Invalid status (with field causes) used for schema violations.
	var response apierrors.APIStatus
	bareTestFailure := err != nil && errors.As(err, &response) && response.Status().Code == http.StatusUnprocessableEntity &&
		(response.Status().Details == nil || len(response.Status().Details.Causes) == 0)
	if err != nil && (bareTestFailure || errors.Is(err, jsonpatch.ErrTestFailed) || strings.Contains(err.Error(), "test failed")) {
		return apierrors.NewConflict(schema.GroupResource{Group: v1.GroupVersion.Group, Resource: r.PathValue("kind")}, obj.GetName(), errResourceChanged)
	}
	return err
}

func managed(obj client.Object) bool {
	bk, ok := obj.(*v1.BookKeeperCluster)
	if !ok {
		return false
	}
	for _, ref := range bk.OwnerReferences {
		if ref.Kind == "PulsarCluster" || (ref.Controller != nil && *ref.Controller) {
			return true
		}
	}
	return false
}

// patchCluster applies a JSON merge patch to the spec, for example {"spec":{"broker":{"replicas":3}}}.
func (s *Server) patchCluster(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	if managed(obj) {
		writeError(w, http.StatusConflict, errManaged)
		return
	}
	if !matchResource(w, r, obj) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("body must be a JSON merge patch: %w", err))
		return
	}
	for k := range probe {
		if k != "spec" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("only spec can be patched, not %q", k))
			return
		}
	}
	if len(probe) != 1 || probe["spec"] == nil {
		writeError(w, http.StatusBadRequest, errors.New("body must contain a spec merge patch"))
		return
	}
	if err := s.patchSpec(r, obj, body); err != nil {
		writeError(w, status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}
func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	if managed(obj) {
		writeError(w, http.StatusConflict, errManaged)
		return
	}
	if !matchResource(w, r, obj) {
		return
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	if err := s.Client.Delete(r.Context(), obj, client.PropagationPolicy(metav1.DeletePropagationForeground),
		client.Preconditions{UID: &uid, ResourceVersion: &version}); err != nil {
		writeError(w, status(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restart sets restartedAt on one component, which rolls its pods.
func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	if managed(obj) {
		writeError(w, http.StatusConflict, errManaged)
		return
	}
	if !matchResource(w, r, obj) {
		return
	}
	component := r.URL.Query().Get("component")
	now := s.now().UTC().Format(time.RFC3339)
	var patch map[string]any
	switch obj.(type) {
	case *v1.PulsarCluster:
		if component == resources.AutoRecovery {
			patch = map[string]any{"spec": map[string]any{"bookkeeper": map[string]any{"autoRecovery": map[string]any{"restartedAt": now}}}}
			break
		}
		field, ok := map[string]string{resources.ZooKeeper: "zookeeper", resources.Bookie: "bookkeeper", resources.Broker: "broker", resources.Proxy: "proxy"}[component]
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Errorf("component must be zookeeper, bookie, autorecovery, broker or proxy, not %q", component))
			return
		}
		patch = map[string]any{"spec": map[string]any{field: map[string]any{"restartedAt": now}}}
	case *v1.BookKeeperCluster:
		switch component {
		case resources.Bookie, "":
			patch = map[string]any{"spec": map[string]any{"restartedAt": now}}
		case resources.AutoRecovery:
			patch = map[string]any{"spec": map[string]any{"autoRecovery": map[string]any{"restartedAt": now}}}
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("component must be bookie or autorecovery, not %q", component))
			return
		}
	}
	body, _ := json.Marshal(patch)
	if err := s.patchSpec(r, obj, body); err != nil {
		writeError(w, status(err), err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

// Pod is a pod of a cluster as the console lists it.
type Pod struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
	Restarts  int32  `json:"restarts"`
	Node      string `json:"node"`
	Image     string `json:"image"`
	StartedAt string `json:"startedAt,omitempty"`
}

func (s *Server) pods(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	list := &corev1.PodList{}
	if err := s.Client.List(r.Context(), list, client.InNamespace(obj.GetNamespace()),
		client.MatchingLabels{"app.kubernetes.io/instance": obj.GetName(), "app.kubernetes.io/managed-by": "pulsar-operator"}); err != nil {
		writeError(w, status(err), err)
		return
	}
	out := make([]Pod, 0, len(list.Items))
	for _, p := range list.Items {
		// A generated BookKeeperCluster commonly has the same instance label as
		// its PulsarCluster; its own read endpoint must not include broker pods.
		if _, bookkeeper := obj.(*v1.BookKeeperCluster); bookkeeper &&
			p.Labels["app.kubernetes.io/component"] != "bookie" && p.Labels["app.kubernetes.io/component"] != "autorecovery" {
			continue
		}
		pod := Pod{Name: p.Name, Component: p.Labels["app.kubernetes.io/component"], Phase: string(p.Status.Phase), Node: p.Spec.NodeName}
		if len(p.Spec.Containers) > 0 {
			pod.Image = p.Spec.Containers[0].Image
		}
		if p.Status.StartTime != nil {
			pod.StartedAt = p.Status.StartTime.UTC().Format(time.RFC3339)
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady {
				pod.Ready = c.Status == corev1.ConditionTrue
			}
		}
		for _, cs := range p.Status.ContainerStatuses {
			pod.Restarts += cs.RestartCount
		}
		if p.DeletionTimestamp != nil {
			pod.Phase = "Terminating"
		}
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

// Event is a Kubernetes event about the cluster resource.
type Event struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Time    string `json:"time"`
	Count   int32  `json:"count"`
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	list := &corev1.EventList{}
	if err := s.Client.List(r.Context(), list, client.InNamespace(obj.GetNamespace())); err != nil {
		writeError(w, status(err), err)
		return
	}
	kind := "PulsarCluster"
	if _, bookkeeper := obj.(*v1.BookKeeperCluster); bookkeeper {
		kind = "BookKeeperCluster"
	}
	out := []Event{}
	for _, e := range list.Items {
		if e.InvolvedObject.Name != obj.GetName() || e.InvolvedObject.Kind != kind ||
			(obj.GetUID() != "" && e.InvolvedObject.UID != obj.GetUID()) {
			continue
		}
		t := e.LastTimestamp.Time
		if t.IsZero() {
			t = e.EventTime.Time
		}
		out = append(out, Event{Type: e.Type, Reason: e.Reason, Message: e.Message, Time: t.UTC().Format(time.RFC3339), Count: e.Count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if len(out) > 50 {
		out = out[:50]
	}
	writeJSON(w, http.StatusOK, out)
}

// metrics returns the dashboard of a cluster over ?range= (5m to 30d, default 1h), queried from the
// cluster's Prometheus.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	obj, err := s.load(r)
	if err != nil {
		writeError(w, status(err), err)
		return
	}
	rng, err := metrics.ParseRange(r.URL.Query().Get("range"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	url, cluster := "", obj.GetName()
	switch o := obj.(type) {
	case *v1.PulsarCluster:
		url = resources.Monitoring{Cluster: o.Name, Namespace: o.Namespace, Domain: o.Spec.ClusterDomain, Spec: o.Spec.Monitoring}.URL()
	case *v1.BookKeeperCluster:
		if managed(o) {
			owner := pulsarOwner(o)
			if owner == "" {
				writeError(w, http.StatusConflict, errors.New("BookKeeperCluster owner cannot be verified for monitoring"))
				return
			}
			pc := &v1.PulsarCluster{}
			if err := s.Client.Get(r.Context(), types.NamespacedName{Namespace: o.Namespace, Name: owner}, pc); err != nil {
				writeError(w, status(err), err)
				return
			}
			if !pulsarOwnedBy(o, pc) {
				writeError(w, http.StatusConflict, errors.New("BookKeeperCluster owner UID does not match its PulsarCluster"))
				return
			}
			url, cluster = resources.Monitoring{Cluster: pc.Name, Namespace: pc.Namespace, Domain: pc.Spec.ClusterDomain, Spec: pc.Spec.Monitoring}.URL(), pc.Name
		} else {
			url = resources.Monitoring{Cluster: o.Name, Namespace: o.Namespace, Domain: o.Spec.ClusterDomain, Spec: o.Spec.Monitoring}.URL()
		}
	}
	if url == "" {
		writeError(w, http.StatusNotFound, errors.New("monitoring is not enabled for this cluster; set spec.monitoring.enabled"))
		return
	}
	httpc := s.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 15 * time.Second}
	}
	d, err := metrics.Build(r.Context(), metrics.Prometheus{URL: url, HTTP: httpc}, obj.GetNamespace(), cluster, rng, s.now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("cannot query Prometheus at %s: %w", url, err))
		return
	}
	writeJSON(w, http.StatusOK, d)
}
func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func status(err error) int {
	var se apierrors.APIStatus
	if errors.As(err, &se) {
		return int(se.Status().Code)
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return http.StatusBadRequest
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
