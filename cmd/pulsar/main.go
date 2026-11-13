// Command pulsar runs the PulsarCluster and BookKeeperCluster controllers and the web console.
package main

import (
	"context"
	"errors"
	"flag"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"net/http"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"time"

	v1 "github.com/SongOf/pulsar-operator/api/pulsar/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/pulsar/audit"
	"github.com/SongOf/pulsar-operator/internal/pulsar/controller"
	"github.com/SongOf/pulsar-operator/internal/pulsar/httpapi"
)

func main() {
	var auditDir string
	flag.StringVar(&auditDir, "audit-dir", env("AUDIT_DIR", "modules/pulsar/.state"), "persistent directory for operation audit")
	var consoleAddr, metricsAddr, probeAddr, webRoot string
	var leaderElect bool
	flag.StringVar(&consoleAddr, "console-bind-address", env("CONSOLE_ADDR", ":8080"), "address of the web console; empty disables it")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "address of the metrics endpoint; 0 disables it")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address of the health probes")
	flag.StringVar(&webRoot, "web-root", env("WEB_ROOT", "/web"), "directory of the built console frontend")
	flag.BoolVar(&leaderElect, "leader-elect", false, "coordinate controller leadership; local audit state requires a single replica")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1.AddToScheme(scheme))
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "pulsar-operator.pulsar.songof.dev",
	})
	if err != nil {
		log.Error(err, "creating the manager")
		os.Exit(1)
	}
	auditStore, err := audit.Open(auditDir)
	if err != nil {
		log.Error(err, "opening durable audit state")
		os.Exit(1)
	}
	defer auditStore.Close()
	auditedClient := &audit.Client{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Store: auditStore}
	if err := (&controller.BookKeeperReconciler{Client: auditedClient, Scheme: scheme, Recorder: mgr.GetEventRecorderFor("bookkeepercluster")}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the BookKeeperCluster controller")
		os.Exit(1)
	}
	if err := (&controller.PulsarReconciler{Client: auditedClient, Scheme: scheme, Recorder: mgr.GetEventRecorderFor("pulsarcluster")}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the PulsarCluster controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("audit", func(r *http.Request) error { return auditStore.Check(r.Context()) }))
	if consoleAddr != "" {
		srv := &http.Server{
			Addr:              consoleAddr,
			Handler:           (&httpapi.Server{Client: auditedClient, Audit: auditStore, WebRoot: webRoot, Token: os.Getenv("CONSOLE_TOKEN")}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		// Serve the management API while controller leadership is being acquired.
		utilruntime.Must(mgr.Add(consoleRunnable{srv}))
		log.Info("console enabled", "address", consoleAddr, "webRoot", webRoot, "tokenRequired", os.Getenv("CONSOLE_TOKEN") != "")
	}
	log.Info("starting the manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running the manager")
		os.Exit(1)
	}
}

type consoleRunnable struct{ srv *http.Server }

func (c consoleRunnable) NeedLeaderElection() bool { return false }
func (c consoleRunnable) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- c.srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.srv.Shutdown(shutdown)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
