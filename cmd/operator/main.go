package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Cluster struct {
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Replicas  map[string]int32  `json:"replicas"`
	Storage   map[string]string `json:"storage"`
	Phase     string            `json:"phase"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type Store struct {
	mu       sync.RWMutex
	clusters map[string]Cluster
}

func main() {
	store := &Store{clusters: map[string]Cluster{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK); _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/api/clusters", store.handleClusters)
	mux.HandleFunc("/api/clusters/", store.handleClusterAction)
	mux.Handle("/", http.FileServer(http.Dir(env("WEB_ROOT", "./web/dist"))))
	addr := ":" + env("PORT", "8080")
	fmt.Printf("pulsar operator management server listening on %s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil { panic(err) }
}

func (s *Store) handleClusters(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock(); defer s.mu.RUnlock()
		items := make([]Cluster, 0, len(s.clusters))
		for _, c := range s.clusters { items = append(items, c) }
		_ = json.NewEncoder(w).Encode(items)
	case http.MethodPost:
		var c Cluster
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil { http.Error(w, err.Error(), http.StatusBadRequest); return }
		if c.Name == "" || c.Namespace == "" { http.Error(w, "name and namespace are required", http.StatusBadRequest); return }
		if c.Kind == "" { c.Kind = "Pulsar" }
		if c.Replicas == nil { c.Replicas = map[string]int32{"brokers": 3, "bookies": 3} }
		if c.Storage == nil { c.Storage = map[string]string{"journal": "10Gi", "ledger": "50Gi"} }
		c.Phase = "Reconciling"; c.UpdatedAt = time.Now().UTC()
		s.mu.Lock(); s.clusters[key(c.Namespace, c.Name)] = c; s.mu.Unlock()
		w.WriteHeader(http.StatusCreated); _ = json.NewEncoder(w).Encode(c)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Store) handleClusterAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/clusters/"), "/")
	if len(parts) < 3 { http.NotFound(w, r); return }
	ns, name, action := parts[0], parts[1], parts[2]
	s.mu.Lock(); defer s.mu.Unlock()
	c, ok := s.clusters[key(ns, name)]
	if !ok { http.NotFound(w, r); return }
	switch action {
	case "scale":
		component := r.URL.Query().Get("component")
		replicas, err := strconv.Atoi(r.URL.Query().Get("replicas"))
		if err != nil || replicas < 0 { http.Error(w, "replicas must be a non-negative integer", http.StatusBadRequest); return }
		if c.Replicas == nil { c.Replicas = map[string]int32{} }
		c.Replicas[component] = int32(replicas)
		c.Phase = "Scaling"
	case "restart":
		c.Phase = "Restarting " + r.URL.Query().Get("component")
	case "storage":
		journal, ledger := r.URL.Query().Get("journal"), r.URL.Query().Get("ledger")
		if journal != "" { c.Storage["journal"] = journal }
		if ledger != "" { c.Storage["ledger"] = ledger }
		c.Phase = "UpdatingStorage"
	default:
		http.NotFound(w, r); return
	}
	c.UpdatedAt = time.Now().UTC(); s.clusters[key(ns, name)] = c
	w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(c)
}

func key(ns, name string) string { return ns + "/" + name }
func env(k, fallback string) string { if v := os.Getenv(k); v != "" { return v }; return fallback }
