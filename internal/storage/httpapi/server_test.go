package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/SongOf/pulsar-operator/internal/storage/model"
	storageweb "github.com/SongOf/pulsar-operator/web/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const testToken = "storage-test-token-at-least-24-characters"

type fixtureBackend struct {
	calls    []string
	key, uid string
	opts     model.ListOptions
	body     model.Object
	err      error
}

func (b *fixtureBackend) result(call string) (model.Object, error) {
	b.calls = append(b.calls, call)
	return model.Object{"status": call}, b.err
}
func (b *fixtureBackend) Overview(context.Context) (model.Object, error) { return b.result("overview") }
func (b *fixtureBackend) List(_ context.Context, kind string, opts model.ListOptions) (model.Page, error) {
	b.calls = append(b.calls, kind)
	b.opts = opts
	return model.Page{Items: []model.Object{{"name": "test"}}, Total: 1, Limit: opts.Limit, Offset: opts.Offset}, b.err
}
func (b *fixtureBackend) Events(ctx context.Context, opts model.ListOptions) (model.Page, error) {
	return b.List(ctx, "events", opts)
}
func (b *fixtureBackend) Plan(_ context.Context, body model.Object) (model.Object, error) {
	b.body = body
	return b.result("plan")
}
func (b *fixtureBackend) CreatePool(_ context.Context, id, confirmation string) (model.Object, error) {
	b.key, b.uid = id, confirmation
	return b.result("pool")
}
func (b *fixtureBackend) CreateVolume(_ context.Context, body model.Object) (model.Object, error) {
	b.body = body
	return b.result("volume")
}
func (b *fixtureBackend) Expand(_ context.Context, id string, body model.Object) (model.Object, error) {
	b.key, b.body = id, body
	return b.result("expand")
}
func (b *fixtureBackend) Release(_ context.Context, id, uid string) (model.Object, error) {
	b.key, b.uid = id, uid
	return b.result("release")
}

func fixture() (*Server, *fixtureBackend) {
	b := &fixtureBackend{}
	return &Server{Mode: "demo", Token: testToken, Backend: b, Static: storageweb.FS, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, b
}
func request(server *Server, method, target, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	if auth {
		r.Header.Set("Authorization", "Bearer "+server.Token)
	}
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	return w
}
func expectStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status=%d want=%d: %s", response.Code, want, response.Body.String())
	}
	if want >= 400 {
		var body model.Object
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || model.String(body["detail"]) == "" {
			t.Fatalf("invalid error JSON: %s", response.Body.String())
		}
	}
}

func TestPublicConfigurationAndBearerAuthentication(t *testing.T) {
	s, b := fixture()
	for _, route := range []string{"/api/config", "/healthz", "/", "/?embedded=1", "/static/app.js", "/static/styles.css"} {
		expectStatus(t, request(s, "GET", route, "", false), 200)
	}
	if len(b.calls) != 0 {
		t.Fatal("public route accessed backend")
	}
	config := request(s, "GET", "/api/config", "", false)
	if !strings.Contains(config.Body.String(), `"tokenRequired":true`) || !strings.Contains(config.Body.String(), `"mode":"demo"`) {
		t.Fatal(config.Body.String())
	}
	expectStatus(t, request(s, "GET", "/api/overview", "", false), 401)
	for _, token := range []string{"bearer " + testToken, "Bearer wrong", "Bearer \xff"} {
		r := httptest.NewRequest("GET", "/api/overview", nil)
		r.Header.Set("Authorization", token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		expectStatus(t, w, 401)
	}
	expectStatus(t, request(s, "GET", "/api/overview", "", true), 200)
	s.Token = ""
	expectStatus(t, request(s, "GET", "/api/disks", "", false), 200)
}

func TestPaginationAndFilterContract(t *testing.T) {
	s, b := fixture()
	for _, kind := range []string{"nodes", "disks", "pools", "volumes", "events"} {
		w := request(s, "GET", "/api/"+kind+"?limit=12&offset=3&node=node-a&state=Ready&q="+url.QueryEscape(strings.Repeat("盘", 300)), "", true)
		expectStatus(t, w, 200)
		if b.opts.Limit != 12 || b.opts.Offset != 3 || b.opts.Node != "node-a" || b.opts.State != "Ready" || len([]rune(b.opts.Query)) != 256 {
			t.Fatalf("options=%+v", b.opts)
		}
		var page model.Page
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || page.Limit != 12 || page.Offset != 3 || page.Total != 1 || len(page.Items) != 1 {
			t.Fatal(w.Body.String())
		}
	}
	expectStatus(t, request(s, "GET", "/api/disks", "", true), 200)
	if b.opts.Limit != 50 || b.opts.Offset != 0 {
		t.Fatalf("default options=%+v", b.opts)
	}
	for _, query := range []string{"limit=0", "limit=501", "offset=-1", "limit=abc", "limit=1.1", "offset=9999999999999999999999", strings.Repeat("q=x&", 100) + "q=x"} {
		t.Run(query[:min(len(query), 30)], func(t *testing.T) { expectStatus(t, request(s, "GET", "/api/disks?"+query, "", true), 400) })
	}
	expectStatus(t, request(s, "GET", "/api/missing", "", true), 404)
}

func TestWriteRoutesAndPVCUIDAreForwarded(t *testing.T) {
	s, b := fixture()
	expectStatus(t, request(s, "POST", "/api/pools/plan", `{"name":"pool-a","diskIds":["disk-a"]}`, true), 200)
	if b.calls[len(b.calls)-1] != "plan" || b.body["name"] != "pool-a" {
		t.Fatal(b)
	}
	expectStatus(t, request(s, "POST", "/api/pools", `{"planId":"plan-a","confirmation":"接管 SERIAL"}`, true), 200)
	if b.key != "plan-a" || b.uid != "接管 SERIAL" {
		t.Fatal(b)
	}
	expectStatus(t, request(s, "POST", "/api/volumes", `{"poolId":"pool-a","sizeGiB":2}`, true), 200)
	if value, ok := b.body["sizeGiB"].(json.Number); !ok || value != "2" {
		t.Fatalf("capacity numeric token lost: %#v", b.body)
	}
	expectStatus(t, request(s, "POST", "/api/volumes/default~data/expand", `{"sizeGiB":3}`, true), 200)
	if b.key != "default~data" {
		t.Fatal(b.key)
	}
	s.Mode = "cluster"
	expectStatus(t, request(s, "DELETE", "/api/volumes/default%7Edata?uid=pvc-uid", "", true), 200)
	if b.key != "default~data" || b.uid != "pvc-uid" {
		t.Fatal(b)
	}
	count := len(b.calls)
	for _, target := range []string{"/api/volumes/default~data?uid=one&uid=two", "/api/volumes/default~data?uid=bad%2Fuid", "/api/volumes/a%2Fb", "/api/volumes/a%252Fb"} {
		expectStatus(t, request(s, "DELETE", target, "", true), 400)
	}
	if len(b.calls) != count {
		t.Fatal("invalid id reached backend")
	}
	expectStatus(t, request(s, "POST", "/api/volumes/a/extra/expand", `{}`, true), 404)
	for _, id := range []string{"null", "[]", "{}", "true", "24", `"../outside"`, `"x/y"`, `"x\u0000y"`} {
		expectStatus(t, request(s, "POST", "/api/pools", `{"planId":`+id+`}`, true), 400)
		expectStatus(t, request(s, "POST", "/api/volumes", `{"poolId":`+id+`}`, true), 400)
	}
}

func TestJSONValidationPrecedesAnyBackendWrite(t *testing.T) {
	s, b := fixture()
	payloads := []string{`{`, `[]`, `null`, `true`, `{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `{"nested":{"a":1,"a":2}}`, `{"a":NaN}`, `{"a":Infinity}`, `{"a":-Infinity}`, `{} {}`, "{\"a\":\"\xff\"}", `{"a":` + strings.Repeat("[", 1200) + strings.Repeat("]", 1200) + `}`}
	for i, payload := range payloads {
		t.Run(strconv.Itoa(i), func(t *testing.T) { expectStatus(t, request(s, "POST", "/api/pools/plan", payload, true), 400) })
	}
	if len(b.calls) != 0 {
		t.Fatal("malformed JSON reached backend")
	}
	for _, payload := range []string{"", strings.Repeat(" ", MaxBodyBytes+1)} {
		expectStatus(t, request(s, "POST", "/api/pools/plan", payload, true), 413)
	}
	expectStatus(t, request(s, "POST", "/api/pools/plan", "{}"+strings.Repeat(" ", MaxBodyBytes-2), true), 200)
}

func TestAmbiguousHTTPFramingRejected(t *testing.T) {
	s, b := fixture()
	cases := []struct {
		name   string
		status int
		mutate func(*http.Request)
	}{
		{"duplicate length", 400, func(r *http.Request) { r.Header.Add("Content-Length", "2") }},
		{"duplicate auth", 400, func(r *http.Request) { r.Header.Add("Authorization", "Bearer other") }},
		{"transfer header", 400, func(r *http.Request) { r.Header.Set("Transfer-Encoding", "chunked") }},
		{"parsed chunking", 400, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} }},
		{"truncated", 400, func(r *http.Request) { r.Header.Set("Content-Length", "12") }},
		{"signed length", 400, func(r *http.Request) { r.Header.Set("Content-Length", "+2") }},
		{"oversized", 413, func(r *http.Request) { r.Header.Set("Content-Length", "65537") }},
		{"missing length", 411, func(r *http.Request) { r.Header.Del("Content-Length"); r.ContentLength = -1 }},
		{"wrong media", 415, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"timeout", 408, func(r *http.Request) { r.Body = timeoutBody{} }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/pools/plan", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Content-Length", "2")
			r.Header.Set("Authorization", "Bearer "+testToken)
			test.mutate(r)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			expectStatus(t, w, test.status)
		})
	}
	if len(b.calls) != 0 {
		t.Fatal("invalid framing reached backend")
	}
}

type timeoutBody struct{}

func (timeoutBody) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
func (timeoutBody) Close() error             { return nil }

func TestErrorStatusAndSensitiveDetails(t *testing.T) {
	s, b := fixture()
	var logs bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	for _, test := range []struct {
		err    error
		status int
	}{
		{model.NewProblem(409, "设备身份变化"), 409},
		{apierrors.NewForbidden(schema.GroupResource{Resource: "diskpools"}, "test", errors.New("denied")), 403},
		{context.DeadlineExceeded, 408},
		{errors.New("secret: " + testToken), 500},
	} {
		b.err = test.err
		w := request(s, "GET", "/api/overview", "", true)
		expectStatus(t, w, test.status)
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatal("secret in response")
		}
	}
	if strings.Contains(logs.String(), testToken) {
		t.Fatal("secret in log")
	}
}

func TestEmbeddedConsoleAssetsAndTraversal(t *testing.T) {
	s, _ := fixture()
	index := request(s, "GET", "/?embedded=1", "", false)
	expectStatus(t, index, 200)
	for _, marker := range []string{`lang="zh-CN"`, `id="main"`, `id="dialog"`, `id="token-button"`, `id="mode"`, `id="refresh-button"`, `./static/app.js`, `./static/styles.css`} {
		if !strings.Contains(index.Body.String(), marker) {
			t.Fatalf("missing %s", marker)
		}
	}
	for _, target := range []string{"/", "/static/app.js", "/static/styles.css"} {
		w := request(s, "GET", target, "", false)
		expectStatus(t, w, 200)
		if len(w.Body.Bytes()) < 100 || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
			t.Fatal("missing asset")
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "SAMEORIGIN" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
			t.Fatal(w.Header())
		}
	}
	s.Static = fstest.MapFS{"static/index.html": {Data: []byte("index")}, "secret.txt": {Data: []byte(testToken)}}
	for _, target := range []string{"/static/../secret.txt", "/static/%2e%2e/secret.txt", "/static/../../secret.txt", "/static/nonexistent.js", "/static/..%5csecret.txt"} {
		w := request(s, "GET", target, "", false)
		expectStatus(t, w, 404)
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatal("filesystem escape")
		}
	}
	expectStatus(t, request(s, "GET", "/api/disks%00", "", true), 400)
}

func TestConfigurationRequiresTokenForClusterAndExternalBinding(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		if err := ValidateConfig("demo", host, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ mode, host, token string }{{"cluster", "localhost", ""}, {"demo", "0.0.0.0", "short"}, {"bad", "localhost", testToken}} {
		if ValidateConfig(test.mode, test.host, test.token) == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	if err := ValidateConfig("cluster", "0.0.0.0", testToken); err != nil {
		t.Fatal(err)
	}
}
