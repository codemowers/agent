// Command feeds polls public Feeds, wakes the Tasks of their FeedBindings
// through the operator's wake endpoint and serves recent items to workers over
// MCP. It runs as a single replica.
package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

func main() {
	var wakeURL, tokenFile, probeAddr, mcpAddr string
	flag.StringVar(&wakeURL, "wake-url", os.Getenv("AGENT_WAKE_URL"), "Base URL of the operator wake endpoint.")
	flag.StringVar(&tokenFile, "token-file", "/var/run/agent-wake/token", "Projected token for audience agent-operator-wake.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.StringVar(&mcpAddr, "mcp-bind-address", ":8080", "Address for the workers' feeds MCP.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	logger := ctrl.Log.WithName("feeds")
	if wakeURL == "" {
		logger.Info("wake URL required")
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = agentsv1alpha1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: probeAddr})
	if err != nil {
		logger.Error(err, "create manager")
		os.Exit(1)
	}
	reconciler := &FeedReconciler{Client: mgr.GetClient(), HTTP: publicHTTPClient(), WakeURL: wakeURL, TokenFile: tokenFile}
	// Polls are timed by requeues; the crawler's own status writes must not
	// trigger another poll.
	if err := ctrl.NewControllerManagedBy(mgr).For(&agentsv1alpha1.Feed{},
		builder.WithPredicates(predicate.GenerationChangedPredicate{})).Complete(reconciler); err != nil {
		logger.Error(err, "set up Feed controller")
		os.Exit(1)
	}
	server := &http.Server{Addr: mcpAddr, Handler: &feedsMCP{reader: mgr.GetClient(),
		reviews: authenticationclient.NewForConfigOrDie(mgr.GetConfig())}, ReadHeaderTimeout: 10 * time.Second}
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server.Shutdown(shutdown) //nolint:errcheck
		}()
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	})); err != nil {
		logger.Error(err, "add feeds MCP server")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		logger.Error(err, "set up health check")
		os.Exit(1)
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "run manager")
		os.Exit(1)
	}
}
