package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/audit"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *Server) auditTarget(r *http.Request, obj client.Object) {
	info := audit.FromContext(r.Context())
	if info == nil {
		return
	}
	kind := "PulsarCluster"
	if _, ok := obj.(*v1.BookKeeperCluster); ok {
		kind = "BookKeeperCluster"
	}
	info.Target = audit.Resource{Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID()), Version: obj.GetResourceVersion(), Generation: obj.GetGeneration()}
	info.ClusterUID = audit.ClusterUID(obj)
}

type auditResponse struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (r *auditResponse) Header() http.Header { return r.header }
func (r *auditResponse) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *auditResponse) Write(body []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(body)
}

func (s *Server) auditAction(action string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Production startup always supplies the durable store. Isolated handler
		// tests can exercise parsing and preconditions without a running database.
		if s.Audit == nil {
			handler(w, r)
			return
		}
		r = r.WithContext(audit.RequestContext(r.Context(), s.Token, r.RemoteAddr))
		info := audit.FromContext(r.Context())
		w.Header().Set("X-Operation-ID", info.OperationID)
		record, err := s.Audit.Begin(r.Context(), audit.Record{
			OperationID: info.OperationID, Source: info.Source, Actor: info.Actor, ClientIP: info.ClientIP, Action: "request." + action,
			Resource: audit.Resource{Kind: r.PathValue("kind"), Namespace: r.PathValue("namespace"), Name: r.PathValue("name")},
		})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("audit unavailable; operation was not started"))
			return
		}
		defer func() {
			if value := recover(); value != nil {
				record.Result, record.Error = "Unknown", "handler stopped before outcome was recorded"
				_ = s.Audit.Finish(r.Context(), record)
				panic(value)
			}
		}()
		response := &auditResponse{header: make(http.Header)}
		handler(response, r)
		if response.code == 0 {
			response.code = http.StatusOK
		}
		record.HTTPStatus, record.Result = response.code, "Succeeded"
		if response.code >= 400 {
			record.Result = "Failed"
		}
		if response.code >= 500 {
			record.Result = "Unknown"
		}
		if info.Target.Name != "" {
			record.Resource = info.Target
		}
		record.ClusterUID = info.ClusterUID
		if err := s.Audit.Finish(r.Context(), record); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("audit completion unavailable; operation may have been applied; refresh the resource before retrying (operation %s)", info.OperationID))
			return
		}
		for name, values := range response.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.code)
		_, _ = w.Write(response.body.Bytes())
	}
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	if s.Audit == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("audit unavailable"))
		return
	}
	values := r.URL.Query()
	limit, cursor := 50, int64(0)
	var err error
	if values.Has("limit") {
		limit, err = strconv.Atoi(values.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 200 {
		writeError(w, 400, fmt.Errorf("limit must be between 1 and 200"))
		return
	}
	if values.Has("cursor") {
		cursor, err = strconv.ParseInt(values.Get("cursor"), 10, 64)
	}
	if err != nil || cursor < 0 {
		writeError(w, 400, fmt.Errorf("invalid audit cursor"))
		return
	}
	for _, key := range []string{"operationId", "namespace", "name", "uid", "source"} {
		if len(values.Get(key)) > 253 {
			writeError(w, 400, fmt.Errorf("audit filter is too long"))
			return
		}
	}
	page, err := s.Audit.List(r.Context(), audit.Query{OperationID: values.Get("operationId"), Namespace: values.Get("namespace"), Name: values.Get("name"), UID: values.Get("uid"), Source: values.Get("source"), Limit: limit, Cursor: cursor})
	if err != nil {
		writeError(w, 500, fmt.Errorf("could not read audit records"))
		return
	}
	writeJSON(w, http.StatusOK, page)
}
