// agent-operator: controller-runtime manager for Agent and Task CRDs.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/api"
	"github.com/codemowers/agent/operator/internal/controller"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
)

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func buildAMQPURL() string {
	if u := os.Getenv("AMQP_URL"); u != "" {
		return u
	}
	host := os.Getenv("AMQP_HOST")
	if host == "" {
		return ""
	}
	port := envOrDefault("AMQP_PORT", "5672")
	user := envOrDefault("AMQP_USERNAME", "guest")
	pass := envOrDefault("AMQP_PASSWORD", "guest")
	vhost := envOrDefault("AMQP_VHOST", "/")
	return fmt.Sprintf("amqp://%s@%s/%s", url.UserPassword(user, pass),
		net.JoinHostPort(host, port), url.PathEscape(vhost))
}

// workerAMQPURL drops the operator's broker credentials. Workers and Mailbox
// Pods log in with their projected ServiceAccount token instead.
func workerAMQPURL(amqpURL string) string {
	parsed, err := url.Parse(amqpURL)
	if err != nil {
		return ""
	}
	parsed.User = nil
	return parsed.String()
}

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	corev1.AddToScheme(scheme)
}

func main() {
	var metricsAddr string
	var probeAddr string
	var apiAddr string
	var mcpAddr string
	var wakeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "Address for health probes.")
	flag.StringVar(&apiAddr, "api-bind-address", ":8080", "Address for the OIDC web UI and task API.")
	flag.StringVar(&mcpAddr, "mcp-bind-address", ":8083", "Address for the internal Agent MCP server.")
	flag.StringVar(&wakeAddr, "wake-bind-address", ":8084", "Address for the internal Task wake endpoint.")
	opts := zap.Options{Development: os.Getenv("DEBUG") == "true"}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		// Controllers and the event consumer run only on the Lease holder, so a
		// rollout never runs two reconcilers; the API serves on every replica.
		LeaderElection:                true,
		LeaderElectionID:              "agent-operator.codemowers.cloud",
		LeaderElectionNamespace:       os.Getenv("POD_NAMESPACE"),
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if err := metrics.Registry.Register(controller.NewTaskMetrics(mgr.GetClient())); err != nil {
		ctrl.Log.Error(err, "unable to register Task metrics")
		os.Exit(1)
	}
	issuer := os.Getenv("OIDC_IDP_URI")
	origin := strings.TrimRight(os.Getenv("OIDC_CLIENT_ORIGIN"), "/")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	redirectURI := strings.FieldsFunc(os.Getenv("OIDC_REDIRECT_URIS"), func(r rune) bool { return r == ' ' || r == ',' || r == '\n' })
	if issuer == "" || origin == "" || clientID == "" || len(redirectURI) == 0 {
		ctrl.Log.Error(fmt.Errorf("missing OIDC owner Secret configuration"), "operator UI authentication unavailable")
		os.Exit(1)
	}
	verifier, err := api.NewOIDCVerifier(issuer, origin)
	if err != nil {
		ctrl.Log.Error(err, "invalid OIDC configuration")
		os.Exit(1)
	}
	clientset, err := authclient.NewForConfig(mgr.GetConfig())
	if err != nil {
		ctrl.Log.Error(err, "create Kubernetes authorization client")
		os.Exit(1)
	}

	if err := (&controller.AgentReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up Agent controller")
		os.Exit(1)
	}

	// RabbitMQ is optional. When configured, per-task queues carry objectives and
	// follow-up prompts; the credential-free AMQP_URL and TASK_QUEUE are
	// injected into every worker pod.
	var amqpMgr *rabbitmq.Manager
	amqpURL := buildAMQPURL()
	if amqpURL != "" {
		amqpMgr = rabbitmq.New(amqpURL)
		ctrl.Log.Info("RabbitMQ configured")
	}

	serverVersion, err := discovery.NewDiscoveryClientForConfigOrDie(mgr.GetConfig()).ServerVersion()
	if err != nil {
		ctrl.Log.Error(err, "read API server version")
		os.Exit(1)
	}
	kubectlTag, err := controller.KubectlTag(serverVersion.GitVersion)
	if err != nil {
		ctrl.Log.Error(err, "derive kubectl image tag")
		os.Exit(1)
	}
	if err := (&controller.TaskReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		AMQP:           amqpMgr,
		AMQPURL:        workerAMQPURL(amqpURL),
		WorkerImage:    os.Getenv("AGENT_WORKER_IMAGE"),
		DisableDevices: os.Getenv("AGENT_DEVICES_ENABLED") == "false",
		EnableFeeds:    os.Getenv("AGENT_FEEDS_ENABLED") == "true",
		KubectlTag:     kubectlTag,
		// Only the Pod's metadata.namespace fieldRef supplies this.
		HarnessNamespace: os.Getenv("POD_NAMESPACE"),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up Task controller")
		os.Exit(1)
	}
	if err := (&controller.MailboxReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
		Image: os.Getenv("AGENT_MAILBOX_IMAGE"), AMQPURL: workerAMQPURL(amqpURL),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to set up Mailbox controller")
		os.Exit(1)
	}

	// The consumer routes broker events on the leader; any replica can record
	// wake requests through it.
	consumer := &controller.EventConsumer{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		AMQPURL:   amqpURL,
		Queue:     envOrDefault("AMQP_QUEUE", "agent.intake"),
		Exchange:  envOrDefault("AMQP_EXCHANGE", "agent.intake"),
		// The webhook receiver publishes as its ServiceAccount; RabbitMQ
		// verifies the user_id it stamps on each event.
		WebhookPublisher: os.Getenv("AGENT_WEBHOOK_PUBLISHER"),
	}

	if amqpMgr != nil {
		consumer.AMQP = amqpMgr
	}

	events := api.NewEvents(mgr.GetCache())
	if err := mgr.Add(events); err != nil {
		ctrl.Log.Error(err, "unable to add API event stream")
		os.Exit(1)
	}
	if err := mgr.Add(&api.Handler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		Events:         events,
		AMQP:           amqpMgr,
		Addr:           apiAddr,
		MCPAddr:        mcpAddr,
		WakeAddr:       wakeAddr,
		Waker:          consumer,
		AccessReviews:  clientset,
		Verifier:       verifier,
		Reviewer:       api.NewSubjectAccessReviewer(clientset),
		TokenReviews:   authenticationclient.NewForConfigOrDie(mgr.GetConfig()),
		WebhookOrigin:  os.Getenv("AGENT_WEBHOOK_ORIGIN"),
		WebhookEnabled: os.Getenv("AGENT_WEBHOOK_ENABLED") != "false",
		DevicesEnabled: os.Getenv("AGENT_DEVICES_ENABLED") != "false",
		Issuer:         issuer,
		ClientID:       clientID,
		Origin:         origin,
		RedirectURI:    redirectURI[0],
	}); err != nil {
		ctrl.Log.Error(err, "unable to add API server")
		os.Exit(1)
	}

	if amqpURL != "" {
		if err := mgr.Add(consumer); err != nil {
			ctrl.Log.Error(err, "unable to add event consumer")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager", "workerPodVersion", controller.WorkerPodVersion)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}
