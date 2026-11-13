package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"MODE", "API_HOST", "API_PORT", "API_TOKEN", "STATE_DIR"} {
		t.Setenv(name, "")
	}
}

func TestCLIOptionsAndEnvironment(t *testing.T) {
	clearEnvironment(t)
	opts, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.mode != "demo" || opts.host != "127.0.0.1" || opts.port != "8080" || opts.stateDir != ".state" {
		t.Fatalf("defaults=%+v", opts)
	}
	t.Setenv("API_HOST", "::1")
	t.Setenv("API_PORT", "8089")
	t.Setenv("STATE_DIR", "configured")
	opts, err = parseOptions([]string{"--port", "8082", "--state-dir", "override"})
	if err != nil || opts.host != "::1" || opts.port != "8082" || opts.stateDir != "override" {
		t.Fatalf("options=%+v error=%v", opts, err)
	}
	for _, args := range [][]string{{"--port", "0"}, {"--port", "65536"}, {"--port", "abc"}, {"--mode", "invalid"}, {"--mode", "cluster"}, {"--host", "0.0.0.0"}, {"extra"}} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Setenv("API_TOKEN", "a-valid-token-at-least-24-characters")
	if _, err := parseOptions([]string{"--mode", "cluster", "--host", "0.0.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOptions([]string{"--mode", "cluster", "--snapshot"}); err == nil {
		t.Fatal("cluster snapshot accepted")
	}
}

func TestSnapshotProducesConsoleFixtureWithoutListening(t *testing.T) {
	clearEnvironment(t)
	var output bytes.Buffer
	serve := func(context.Context, *http.Server) error { t.Fatal("snapshot opened HTTP server"); return nil }
	if err := run(context.Background(), []string{"--snapshot", "--state-dir", t.TempDir()}, &output, serve); err != nil {
		t.Fatal(err)
	}
	var fixture map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"nodes", "disks", "pools", "volumes", "events"} {
		var page struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
			Limit int              `json:"limit"`
		}
		if err := json.Unmarshal(fixture[kind], &page); err != nil || page.Total == 0 || len(page.Items) == 0 || page.Limit != 500 {
			t.Fatalf("%s page=%s error=%v", kind, fixture[kind], err)
		}
	}
	if !bytes.Contains(fixture["overview"], []byte(`"mode":"demo"`)) {
		t.Fatal("missing demo overview")
	}
}

func TestStartupPreparesHTTPServerAndReportsSocketError(t *testing.T) {
	clearEnvironment(t)
	denied := errors.New("socket denied")
	called := false
	err := run(context.Background(), []string{"--state-dir", t.TempDir()}, io.Discard, func(_ context.Context, server *http.Server) error {
		called = true
		if server.Addr != "127.0.0.1:8080" || server.ReadTimeout != 15*time.Second || server.ReadHeaderTimeout != 15*time.Second || server.WriteTimeout == 0 {
			t.Fatalf("HTTP deadlines/address: %+v", server)
		}
		w := httptest.NewRecorder()
		server.Handler.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"demo"`) {
			t.Fatal(w.Body.String())
		}
		return denied
	})
	if !called || !errors.Is(err, denied) {
		t.Fatalf("socket error not propagated: %v", err)
	}
}

func TestInvalidConfigAndHelpDoNotCreateState(t *testing.T) {
	clearEnvironment(t)
	state := filepath.Join(t.TempDir(), "should-not-exist")
	serve := func(context.Context, *http.Server) error { t.Fatal("unexpected listener"); return nil }
	if err := run(context.Background(), []string{"--state-dir", state, "--mode", "cluster"}, io.Discard, serve); err == nil {
		t.Fatal("cluster token requirement skipped")
	}
	if err := run(context.Background(), []string{"--state-dir", state, "--help"}, io.Discard, serve); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("unexpected state directory: %v", err)
	}
}
