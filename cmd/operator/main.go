// Command operator runs the PulsarCluster and BookKeeperCluster controllers and the web console.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/SongOf/pulsar-operator/api/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/console"
	"github.com/SongOf/pulsar-operator/internal/controller"
)

func main() {
	var consoleAddr, metricsAddr, probeAddr, webRoot string
	var leaderElect bool
	flag.StringVar(&consoleAddr, "console-bind-address", env("CONSOLE_ADDR", ":8080"), "address of the web console; empty disables it")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "address of the metrics endpoint; 0 disables it")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address of the health probes")
	flag.StringVar(&webRoot, "web-root", env("WEB_ROOT", "/web"), "directory of the built console frontend")
	flag.BoolVar(&leaderElect, "leader-elect", false, "elect a leader, for running several replicas")
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
	if err := (&controller.BookKeeperReconciler{Client: mgr.GetClient(), Scheme: scheme, Recorder: mgr.GetEventRecorderFor("bookkeepercluster")}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the BookKeeperCluster controller")
		os.Exit(1)
	}
	if err := (&controller.PulsarReconciler{Client: mgr.GetClient(), Scheme: scheme, Recorder: mgr.GetEventRecorderFor("pulsarcluster")}).SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up the PulsarCluster controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	if consoleAddr != "" {
		srv := &http.Server{
			Addr:              consoleAddr,
			Handler:           (&console.Server{Client: mgr.GetClient(), WebRoot: webRoot, Token: os.Getenv("CONSOLE_TOKEN")}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		// The console serves every replica, not only the leader, so it implements NeedLeaderElection as false.
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
