// Command agent discovers disks and reconciles local DiskPools. The storage API
// and Pulsar controllers are separate processes communicating through Kubernetes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/SongOf/pulsar-operator/internal/agent/controller"
	"github.com/SongOf/pulsar-operator/internal/agent/discovery"
	"github.com/SongOf/pulsar-operator/internal/agent/lvm"
	"github.com/SongOf/pulsar-operator/internal/kubeconfig"
	"k8s.io/client-go/dynamic"
)

func main() {
	if err := run(); err != nil {
		slog.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	node := flag.String("node-name", os.Getenv("NODE_NAME"), "node whose physical disks this agent manages")
	state := flag.String("state-dir", env("STATE_DIR", "/var/lib/diskfleet"), "host directory holding compatible ownership journals and lock")
	discoverOnly := flag.Bool("discover", false, "print read-only local disk inventory as JSON and exit; no Kubernetes access")
	interval := flag.String("reconcile-interval", env("RECONCILE_INTERVAL", "30"), "reconciliation interval in seconds (minimum 10)")
	flag.Parse()
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the node agent requires Linux; use fixture tests on %s", runtime.GOOS)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	discoverer := discovery.Discoverer{}
	if *discoverOnly {
		disks, err := discoverer.Discover(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(disks)
	}
	if *node == "" {
		return fmt.Errorf("NODE_NAME or --node-name is required")
	}
	seconds, err := strconv.Atoi(*interval)
	if err != nil || seconds < 1 || seconds > 86400 {
		return fmt.Errorf("reconcile interval must be an integer between 1 and 86400 seconds")
	}
	level := slog.LevelInfo
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN", "WARNING":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	config, err := kubeconfig.Config()
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	writesEnabled := strings.EqualFold(os.Getenv("ENABLE_DISK_WRITES"), "true")
	backend := &lvm.Backend{StateDir: *state, Inventory: discoverer.Discover, WritesEnabled: writesEnabled}
	reconciler := &controller.Reconciler{Client: client, Backend: backend, Discover: discoverer.Discover,
		NodeName: *node, WritesEnabled: writesEnabled, Logger: logger}
	logger.Info("starting node agent", "node", *node, "writesEnabled", writesEnabled, "intervalSeconds", max(seconds, 10))
	return reconciler.Run(ctx, time.Duration(seconds)*time.Second)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
