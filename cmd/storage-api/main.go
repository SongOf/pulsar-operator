// Command storage-api serves the storage console. Node devices are exclusively
// managed by the independently deployed agent binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/SongOf/pulsar-operator/internal/kubeconfig"
	"github.com/SongOf/pulsar-operator/internal/storage/cluster"
	"github.com/SongOf/pulsar-operator/internal/storage/httpapi"
	"github.com/SongOf/pulsar-operator/internal/storage/model"
	"github.com/SongOf/pulsar-operator/internal/storage/store"
	storageweb "github.com/SongOf/pulsar-operator/web/storage"
	"k8s.io/client-go/dynamic"
)

type options struct {
	host, port, mode, stateDir, token string
	snapshot                          bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, serve); err != nil {
		fmt.Fprintf(os.Stderr, "DiskFleet 启动失败：%v。请检查状态目录、端口占用和运行环境。\n", err)
		os.Exit(2)
	}
}

func parseOptions(args []string) (options, error) {
	opts := options{token: os.Getenv("API_TOKEN")}
	flags := flag.NewFlagSet("storage-api", flag.ContinueOnError)
	flags.StringVar(&opts.host, "host", env("API_HOST", "127.0.0.1"), "HTTP listen address")
	flags.StringVar(&opts.port, "port", env("API_PORT", "8080"), "HTTP listen port")
	flags.StringVar(&opts.mode, "mode", env("MODE", "demo"), "demo or cluster")
	flags.StringVar(&opts.stateDir, "state-dir", env("STATE_DIR", ".state"), "plans, audit and demo state directory")
	flags.BoolVar(&opts.snapshot, "snapshot", false, "emit a seeded demo JSON snapshot without opening a listening socket")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected positional arguments")
	}
	port, err := strconv.Atoi(opts.port)
	if err != nil || port < 1 || port > 65535 {
		return opts, fmt.Errorf("端口须为 1–65535")
	}
	if err := httpapi.ValidateConfig(opts.mode, opts.host, opts.token); err != nil {
		return opts, err
	}
	if opts.snapshot && opts.mode != "demo" {
		return opts, fmt.Errorf("--snapshot is available only in demo mode")
	}
	return opts, nil
}

func run(ctx context.Context, args []string, output io.Writer, serveHTTP func(context.Context, *http.Server) error) error {
	opts, err := parseOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err := store.New(opts.stateDir)
	if err != nil {
		return err
	}
	defer state.Close()
	var backend model.Backend = state
	if opts.mode == "demo" {
		if err := state.Seed(ctx); err != nil {
			return err
		}
	} else {
		config, err := kubeconfig.Config()
		if err != nil {
			return err
		}
		client, err := dynamic.NewForConfig(config)
		if err != nil {
			return err
		}
		backend = cluster.New(state, client)
	}
	if opts.snapshot {
		value, err := snapshot(ctx, backend)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(value)
	}
	server := &http.Server{
		Addr:              net.JoinHostPort(opts.host, opts.port),
		Handler:           (&httpapi.Server{Mode: opts.mode, Token: opts.token, Backend: backend, Static: storageweb.FS}).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 * 1024,
		ConnContext:       httpapi.ConnContext,
	}
	return serveHTTP(ctx, server)
}

func snapshot(ctx context.Context, backend model.Backend) (model.Object, error) {
	overview, err := backend.Overview(ctx)
	if err != nil {
		return nil, err
	}
	result := model.Object{"overview": overview}
	for _, kind := range []string{"nodes", "disks", "pools", "volumes", "events"} {
		opts := model.ListOptions{Limit: 500}
		var page model.Page
		if kind == "events" {
			page, err = backend.Events(ctx, opts)
		} else {
			page, err = backend.List(ctx, kind, opts)
		}
		if err != nil {
			return nil, err
		}
		result[kind] = page
	}
	return result, nil
}

func serve(ctx context.Context, server *http.Server) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(httpapi.StrictListener(listener)) }()
	select {
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
