// Package api provides a minimal HTTP UI for creating Tasks and sending prompts.
package api

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	authclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	"github.com/codemowers/agent/operator/internal/rabbitmq"
)

// Publisher is the subset of rabbitmq.Manager used by the API handler.
type Publisher interface {
	DeclareQueue(name string) error
	DeclareProbeQueue(name string) error
	Publish(ctx context.Context, queue string, msg rabbitmq.PromptMessage) error
}

// Handler serves the web UI and REST API for Task management.
type Handler struct {
	Client    client.Client
	APIReader client.Reader
	Events    *Events
	AMQP      Publisher // nil if RabbitMQ not configured
	Addr      string
	MCPAddr   string
	// HarnessNamespace names the operator's Services, which are the
	// audiences of the tokens its callers present.
	HarnessNamespace string
	// WakeAddr serves the wake endpoint to in-cluster callers.
	WakeAddr        string
	Waker           TaskWaker
	AccessReviews   authclient.AuthorizationV1Interface
	Verifier        tokenVerifier
	Reviewer        accessReviewer
	Issuer          string
	ClientID        string
	Origin          string
	RedirectURI     string
	ModelHTTPClient *http.Client
	TokenReviews    authenticationclient.AuthenticationV1Interface
	WebhookOrigin   string
	WebhookEnabled  bool
	DevicesEnabled  bool
}

// taskQueue resolves the Task's queue through its Agent's worker ServiceAccount.
func (h *Handler) taskQueue(ctx context.Context, task *agentsv1alpha1.Task) (string, error) {
	var agent agentsv1alpha1.Agent
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: task.Spec.AgentRef}, &agent); err != nil {
		return "", fmt.Errorf("Task Agent unavailable: %w", err)
	}
	return agentsv1alpha1.TaskQueue(task, &agent), nil
}

// NeedLeaderElection serves the UI and worker MCP on every replica.
func (h *Handler) NeedLeaderElection() bool { return false }

func (h *Handler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("api")
	srv := &http.Server{Addr: h.Addr, Handler: h.routes()}
	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("POST /mcp", h.agentMCP)
	mcpSrv := &http.Server{Addr: h.MCPAddr, Handler: mcpMux}
	wakeMux := http.NewServeMux()
	wakeMux.HandleFunc("POST /internal/v1/namespaces/{namespace}/tasks/{name}/wake", h.wakeTask)
	wakeSrv := &http.Server{Addr: h.WakeAddr, Handler: wakeMux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(shutdownCtx) != nil {
			srv.Close() //nolint:errcheck
		}
		if mcpSrv.Shutdown(shutdownCtx) != nil {
			mcpSrv.Close() //nolint:errcheck
		}
		if wakeSrv.Shutdown(shutdownCtx) != nil {
			wakeSrv.Close() //nolint:errcheck
		}
	}()
	logger.Info("API server listening", "addr", h.Addr)
	logger.Info("Agent MCP server listening", "addr", h.MCPAddr)
	logger.Info("wake server listening", "addr", h.WakeAddr)
	errors := make(chan error, 3)
	go func() { errors <- srv.ListenAndServe() }()
	go func() { errors <- mcpSrv.ListenAndServe() }()
	go func() { errors <- wakeSrv.ListenAndServe() }()
	if err := <-errors; err != http.ErrServerClosed {
		_ = srv.Close()
		_ = mcpSrv.Close()
		_ = wakeSrv.Close()
		return err
	}
	return nil
}

func (h *Handler) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", h.config)
	mux.HandleFunc("GET /api/agents", h.listAgents)
	mux.HandleFunc("GET /api/agents/{namespace}/{name}/models", h.listAgentModels)
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/tasks", h.listTasks)
	mux.HandleFunc("GET /api/tasks/{namespace}/{name}", h.getTask)
	mux.HandleFunc("POST /api/events", h.streamEvents)
	mux.HandleFunc("POST /api/tasks", h.createTask)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/prompts", h.sendPrompt)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/mcp-probe", h.requestMCPProbe)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/suspend", h.setTaskSuspend)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/stop", h.stopTurn)
	mux.HandleFunc("POST /api/tasks/{namespace}/{name}/mcps", h.setMCPDisabled)
	mux.HandleFunc("DELETE /api/tasks/{namespace}/{name}", h.deleteTask)
	root, _ := fs.Sub(publicAssets, "public")
	static := http.FileServer(http.FS(root))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/callback" {
			http.ServeFileFS(w, r, root, "index.html")
			return
		}
		static.ServeHTTP(w, r)
	}))
	return mux
}

//go:embed public/*
var publicAssets embed.FS

// --- API types ---

type agentItem struct {
	Name            string                           `json:"name"`
	Namespace       string                           `json:"namespace"`
	Backend         string                           `json:"backend"`
	ModelName       string                           `json:"modelName,omitempty"`
	GitHubUsername  string                           `json:"githubUsername,omitempty"`
	Suspended       bool                             `json:"suspended"`
	IdleSeconds     int32                            `json:"idleSeconds"`
	Resources       *corev1.ResourceRequirements     `json:"resources,omitempty"`
	Forges          []agentsv1alpha1.ForgeCapability `json:"forges,omitempty"`
	ForgesCheckedAt string                           `json:"forgesCheckedAt,omitempty"`
}

type taskItem struct {
	Name                string   `json:"name"`
	Title               string   `json:"title,omitempty"`
	Namespace           string   `json:"namespace"`
	AgentName           string   `json:"agentName"`
	Phase               string   `json:"phase"`
	Reason              string   `json:"reason"`
	Objective           string   `json:"objective"`
	IdleSeconds         int32    `json:"idleSeconds"`
	Activity            string   `json:"activity,omitempty"`
	Suspended           bool     `json:"suspended,omitempty"`
	Completed           bool     `json:"completed,omitempty"`
	DisabledMCPs        []string `json:"disabledMcps,omitempty"`
	TurnStartedAt       string   `json:"turnStartedAt,omitempty"`
	LastResultAt        string   `json:"lastResultAt,omitempty"`
	CreatedAt           string   `json:"createdAt,omitempty"`
	ObjectiveSource     string   `json:"objectiveSource,omitempty"`
	WorkerPod           string   `json:"workerPod,omitempty"`
	WorkerPodUID        string   `json:"workerPodUid,omitempty"`
	MCPProbeAt          string   `json:"mcpProbeAt,omitempty"`
	Summary             string   `json:"summary,omitempty"`
	SummaryAt           string   `json:"summaryAt,omitempty"`
	LastMessageID       string   `json:"lastMessageId,omitempty"`
	LastMessageAt       string   `json:"lastMessageAt,omitempty"`
	AgentGitHubUsername string   `json:"agentGitHubUsername,omitempty"`
}

type taskDetail struct {
	taskItem
	Conversation       []agentsv1alpha1.TaskMessage       `json:"conversation"`
	ExternalReferences []agentsv1alpha1.ExternalReference `json:"externalReferences"`
	MCPProbe           *agentsv1alpha1.MCPProbeStatus     `json:"mcpProbe,omitempty"`
}

type createTaskReq struct {
	AgentName      string `json:"agentName"`
	AgentNamespace string `json:"agentNamespace"`
	Objective      string `json:"objective"`
	ModelName      string `json:"modelName,omitempty"`
}

type sendPromptReq struct {
	Body      string `json:"body"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

type mcpToggleReq struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled"`
}

var mcpNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type suspendReq struct {
	Suspend bool `json:"suspend"`
}

func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{"issuer": h.Issuer, "clientId": h.ClientID, "origin": h.Origin,
		"redirectURI": h.RedirectURI, "audience": strings.TrimRight(h.Origin, "/") + "/api",
		"webhookOrigin": h.WebhookOrigin, "webhookEnabled": h.WebhookEnabled,
		"devicesEnabled": h.DevicesEnabled})
}

func (h *Handler) authenticatedCaller(w http.ResponseWriter, r *http.Request) (caller, bool) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
		jsonError(w, fmt.Errorf("sign in to continue"), http.StatusUnauthorized)
		return caller{}, false
	}
	if h.Verifier == nil {
		jsonError(w, fmt.Errorf("OIDC unavailable"), 503)
		return caller{}, false
	}
	identity, err := h.Verifier.Verify(r.Context(), parts[1])
	if err != nil {
		jsonError(w, fmt.Errorf("invalid or expired access token"), http.StatusUnauthorized)
		return caller{}, false
	}
	return identity, true
}

func (h *Handler) canAccessAgent(r *http.Request, identity caller, agent *agentsv1alpha1.Agent) (bool, error) {
	if agent.Spec.Access != nil {
		return agent.Spec.Access.Admits(identity.subject, identity.groups), nil
	}
	if h.Reviewer == nil {
		return false, fmt.Errorf("Kubernetes authorization unavailable")
	}
	return h.Reviewer.Allowed(r.Context(), identity, agent.Namespace, agent.Name)
}

func (h *Handler) canAccessTask(r *http.Request, identity caller, task *agentsv1alpha1.Task) (bool, error) {
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion != agentsv1alpha1.GroupVersion.String() || owner.Kind != "Agent" {
			continue
		}
		var agent agentsv1alpha1.Agent
		if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: task.Namespace, Name: owner.Name}, &agent); err != nil {
			return false, err
		}
		if owner.UID != agent.UID {
			return false, nil
		}
		return h.canAccessAgent(r, identity, &agent)
	}
	return false, nil
}

// --- Handlers ---

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	var list agentsv1alpha1.AgentList
	if err := h.Client.List(r.Context(), &list); err != nil {
		jsonError(w, err, 500)
		return
	}
	out := make([]agentItem, 0, len(list.Items))
	for _, a := range list.Items {
		allowed, err := h.canAccessAgent(r, identity, &a)
		if err != nil {
			jsonError(w, err, 503)
			return
		}
		if !allowed {
			continue
		}
		forges := a.Status.Forges
		forgeStatusCurrent := agentsv1alpha1.ForgesCurrent(&a)
		if !forgeStatusCurrent {
			forges = make([]agentsv1alpha1.ForgeCapability, 0, len(a.Spec.GitProviders))
			for _, provider := range a.Spec.GitProviders {
				forges = append(forges, agentsv1alpha1.ForgeCapability{Type: provider.Type, ServerURL: provider.ServerURL, Status: "pending"})
			}
		}
		out = append(out, agentItem{
			Name:           a.Name,
			Namespace:      a.Namespace,
			Backend:        a.Spec.Model.Backend,
			ModelName:      a.Spec.Model.Name,
			GitHubUsername: agentGitHubUsername(&a),
			Suspended:      a.Spec.Suspended,
			IdleSeconds:    agentsv1alpha1.AgentIdleSeconds(&a),
			Resources:      agentsv1alpha1.AgentWorkerResources(&a),
			Forges:         forges,
		})
		if forgeStatusCurrent {
			out[len(out)-1].ForgesCheckedAt = a.Status.ForgesCheckedAt.Format(time.RFC3339)
		}
	}
	jsonOK(w, out)
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	var list agentsv1alpha1.TaskList
	if err := h.Client.List(r.Context(), &list); err != nil {
		jsonError(w, err, 500)
		return
	}
	var agents agentsv1alpha1.AgentList
	if err := h.Client.List(r.Context(), &agents); err != nil {
		jsonError(w, err, 500)
		return
	}
	type agentKey struct{ namespace, name, uid string }
	allowed := make(map[agentKey]bool)
	for i := range agents.Items {
		a := &agents.Items[i]
		ok, err := h.canAccessAgent(r, identity, a)
		if err != nil {
			jsonError(w, err, 503)
			return
		}
		if ok {
			allowed[agentKey{a.Namespace, a.Name, string(a.UID)}] = true
		}
	}
	out := make([]taskItem, 0, len(list.Items))
	for _, t := range list.Items {
		agentName := ""
		for _, ref := range t.OwnerReferences {
			if ref.APIVersion == agentsv1alpha1.GroupVersion.String() && ref.Kind == "Agent" &&
				allowed[agentKey{t.Namespace, ref.Name, string(ref.UID)}] {
				agentName = ref.Name
				break
			}
		}
		if agentName == "" {
			continue
		}
		item := taskDisplay(&t)
		item.AgentName = agentName
		for i := range agents.Items {
			if agents.Items[i].Namespace == t.Namespace && agents.Items[i].Name == agentName {
				item.AgentGitHubUsername = agentGitHubUsername(&agents.Items[i])
				break
			}
		}
		out = append(out, item)
	}
	jsonOK(w, out)
}

func (h *Handler) getTask(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}, &task); err != nil {
		jsonError(w, err, http.StatusNotFound)
		return
	}
	allowed, err := h.canAccessTask(r, identity, &task)
	if err != nil {
		jsonError(w, err, 503)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Task denied"), http.StatusForbidden)
		return
	}
	item := taskDisplay(&task)
	for _, owner := range task.OwnerReferences {
		if owner.APIVersion == agentsv1alpha1.GroupVersion.String() && owner.Kind == "Agent" {
			item.AgentName = owner.Name
			var agent agentsv1alpha1.Agent
			if h.Client.Get(r.Context(), client.ObjectKey{Namespace: task.Namespace, Name: owner.Name}, &agent) == nil && agent.UID == owner.UID {
				item.AgentGitHubUsername = agentGitHubUsername(&agent)
			}
			break
		}
	}
	conversation := task.Status.Conversation
	if conversation == nil {
		conversation = []agentsv1alpha1.TaskMessage{}
	}
	jsonOK(w, taskDetail{taskItem: item, Conversation: conversation, ExternalReferences: taskExternalReferences(&task), MCPProbe: task.Status.MCPProbe})
}

func (h *Handler) requestMCPProbe(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	if h.AMQP == nil {
		jsonError(w, fmt.Errorf("RabbitMQ not configured"), http.StatusServiceUnavailable)
		return
	}
	key := client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	var task agentsv1alpha1.Task
	if err := h.mcpReader().Get(r.Context(), key, &task); err != nil {
		jsonError(w, err, http.StatusNotFound)
		return
	}
	allowed, err := h.canAccessTask(r, identity, &task)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Task denied"), http.StatusForbidden)
		return
	}
	if task.Spec.Suspend || task.Status.Phase != agentsv1alpha1.TaskPhaseRunning || task.Status.WorkerPodUID == "" {
		jsonError(w, fmt.Errorf("Task has no running worker"), http.StatusConflict)
		return
	}
	if probe := task.Status.MCPProbe; probe != nil && probe.PodUID == task.Status.WorkerPodUID && time.Since(probe.UpdatedAt.Time) < 20*time.Second {
		jsonOK(w, probe)
		return
	}
	requestID := randHex(16)
	podUID := task.Status.WorkerPodUID
	taskQueue, err := h.taskQueue(r.Context(), &task)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	queue := rabbitmq.ProbeQueue(taskQueue, podUID)
	if err := h.AMQP.DeclareProbeQueue(queue); err != nil {
		jsonError(w, fmt.Errorf("probe queue could not be declared: %w", err), http.StatusServiceUnavailable)
		return
	}
	var coalesced *agentsv1alpha1.MCPProbeStatus
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Task
		if err := h.mcpReader().Get(r.Context(), key, &current); err != nil {
			return err
		}
		if current.Spec.Suspend || current.Status.Phase != agentsv1alpha1.TaskPhaseRunning || current.Status.WorkerPodUID != podUID {
			return fmt.Errorf("worker changed")
		}
		if probe := current.Status.MCPProbe; probe != nil && probe.PodUID == podUID && time.Since(probe.UpdatedAt.Time) < 20*time.Second {
			coalesced = probe
			return nil
		}
		next := &agentsv1alpha1.MCPProbeStatus{RequestID: requestID, PodUID: podUID, State: "pending", UpdatedAt: metav1.Now()}
		if previous := current.Status.MCPProbe; previous != nil {
			// Keep the last completed result for display while this check runs.
			next.Endpoints, next.CheckedAt = previous.Endpoints, previous.CheckedAt
		}
		current.Status.MCPProbe = next
		return h.Client.Status().Update(r.Context(), &current)
	})
	if err != nil {
		jsonError(w, err, http.StatusConflict)
		return
	}
	if coalesced != nil {
		jsonOK(w, coalesced)
		return
	}
	if err := h.AMQP.Publish(r.Context(), queue, rabbitmq.PromptMessage{Type: "mcp_probe", MessageID: requestID, PodUID: podUID}); err != nil {
		_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var current agentsv1alpha1.Task
			if getErr := h.Client.Get(r.Context(), key, &current); getErr != nil {
				return getErr
			}
			if current.Status.MCPProbe == nil || current.Status.MCPProbe.RequestID != requestID {
				return nil
			}
			current.Status.MCPProbe.State = "failed"
			current.Status.MCPProbe.UpdatedAt = metav1.Now()
			return h.Client.Status().Update(r.Context(), &current)
		})
		jsonError(w, fmt.Errorf("probe request could not be published"), http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, map[string]string{"requestId": requestID, "state": "pending"})
}

func taskExternalReferences(task *agentsv1alpha1.Task) []agentsv1alpha1.ExternalReference {
	references := make([]agentsv1alpha1.ExternalReference, 0, len(task.Spec.ExternalReferences)+len(task.Status.ExternalReferences))
	references = append(references, task.Spec.ExternalReferences...)
	for _, observed := range task.Status.ExternalReferences {
		found := false
		for _, existing := range references {
			if existing.Source == observed.Source && existing.Kind == observed.Kind && existing.ID == observed.ID &&
				existing.ServerURL == observed.ServerURL && existing.MailboxUID == observed.MailboxUID && existing.Folder == observed.Folder {
				found = true
				break
			}
		}
		if !found {
			references = append(references, observed)
		}
	}
	return references
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	var req createTaskReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 192<<10)).Decode(&req); err != nil {
		jsonError(w, err, 400)
		return
	}
	if req.Objective == "" || req.AgentName == "" || req.AgentNamespace == "" {
		jsonError(w, fmt.Errorf("agentName, agentNamespace and objective are required"), 400)
		return
	}
	if req.ModelName != "" && !modelNamePattern.MatchString(req.ModelName) {
		jsonError(w, fmt.Errorf("invalid model name"), 400)
		return
	}

	var agent agentsv1alpha1.Agent
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: req.AgentNamespace, Name: req.AgentName}, &agent); err != nil {
		jsonError(w, err, 404)
		return
	}
	allowed, err := h.canAccessAgent(r, identity, &agent)
	if err != nil {
		jsonError(w, err, 503)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Agent denied"), http.StatusForbidden)
		return
	}

	taskName := fmt.Sprintf("%s-%s", slugify(req.Objective), randHex(4))
	task := &agentsv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: agent.Namespace,
			Labels: map[string]string{
				"codemowers.ai/agent": agent.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         agentsv1alpha1.GroupVersion.String(),
				Kind:               "Agent",
				Name:               agent.Name,
				UID:                agent.UID,
				BlockOwnerDeletion: boolPtr(true),
				Controller:         boolPtr(true),
			}},
		},
		Spec: agentsv1alpha1.TaskSpec{
			AgentRef:    agent.Name,
			Objective:   req.Objective,
			Suspend:     agent.Spec.Suspended,
			IdleSeconds: int32Ptr(agentsv1alpha1.AgentIdleSeconds(&agent)),
			Resources:   agentsv1alpha1.AgentWorkerResources(&agent),
		},
	}
	if req.ModelName != "" && req.ModelName != agent.Spec.Model.Name {
		model := agent.Spec.Model
		model.Name = req.ModelName
		task.Spec.Model = &model
	}
	if err := h.Client.Create(r.Context(), task); err != nil {
		jsonError(w, err, 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"name": task.Name, "namespace": task.Namespace}) //nolint:errcheck
}

func (h *Handler) sendPrompt(w http.ResponseWriter, r *http.Request) {
	requestedAt := time.Now().UnixMilli()
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	if h.AMQP == nil {
		jsonError(w, fmt.Errorf("RabbitMQ not configured"), 503)
		return
	}
	var req sendPromptReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonError(w, err, 400)
		return
	}
	if req.Body == "" {
		jsonError(w, fmt.Errorf("body is required"), 400)
		return
	}
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &task); err != nil {
		jsonError(w, err, http.StatusNotFound)
		return
	}
	allowed, err := h.canAccessTask(r, identity, &task)
	if err != nil {
		jsonError(w, err, 503)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Task denied"), http.StatusForbidden)
		return
	}
	// Suspended Tasks accept durable steering without resuming their worker.
	queue, err := h.taskQueue(r.Context(), &task)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	if err := h.AMQP.DeclareQueue(queue); err != nil {
		jsonError(w, fmt.Errorf("declare queue: %w", err), 500)
		return
	}
	msg := rabbitmq.PromptMessage{Type: "prompt", Body: req.Body, MessageID: randHex(16)}
	if err := h.AMQP.Publish(r.Context(), queue, msg); err != nil {
		jsonError(w, fmt.Errorf("publish: %w", err), 500)
		return
	}
	// A published prompt is durable in RabbitMQ. Retry status conflicts so the
	// transcript also records concurrent prompts from other browser sessions.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Task
		if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &current); err != nil {
			return err
		}
		deliveryState := "queued"
		if slices.Contains(current.Status.DeliveredMessageIDs, msg.MessageID) {
			deliveryState = "delivered"
		}
		current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: msg.MessageID, Role: "user", Source: "web", Actor: identity.user,
			Content: req.Body, DeliveryState: deliveryState, CreatedAt: metav1.Now()})
		// The TaskReconciler ensures a worker Pod is running.
		current.Status.Events.WakePending = true
		return h.Client.Status().Update(r.Context(), &current)
	}); err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	result := map[string]string{"status": "published"}
	if req.Interrupt && !task.Spec.Suspend && task.Status.Phase == agentsv1alpha1.TaskPhaseRunning && task.Status.Activity == "working" && task.Status.WorkerPodUID != "" {
		controlQueue := rabbitmq.ProbeQueue(queue, task.Status.WorkerPodUID)
		stop := rabbitmq.PromptMessage{Type: "stop", MessageID: randHex(16), PodUID: task.Status.WorkerPodUID, ReceivedAt: requestedAt}
		err := h.AMQP.DeclareProbeQueue(controlQueue)
		if err == nil {
			err = h.AMQP.Publish(r.Context(), controlQueue, stop)
		}
		if err != nil {
			// The prompt is already durable; do not invite a duplicate submission.
			result["interruptError"] = "Message queued, but interrupt failed. Use Stop to interrupt the current turn."
		} else {
			h.recordUserAction(r, &task, identity, "Interrupt requested; steering queued")
		}
	}
	jsonOK(w, result)
}

// authorizedTask authenticates the caller and loads a Task it may access.
func (h *Handler) authorizedTask(w http.ResponseWriter, r *http.Request) (caller, *agentsv1alpha1.Task, bool) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return caller{}, nil, false
	}
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}, &task); err != nil {
		jsonError(w, err, http.StatusNotFound)
		return caller{}, nil, false
	}
	allowed, err := h.canAccessTask(r, identity, &task)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return caller{}, nil, false
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Task denied"), http.StatusForbidden)
		return caller{}, nil, false
	}
	return identity, &task, true
}

// setTaskSuspend pauses or resumes a Task. Pausing stops its worker while
// retaining the PVC, native session and queued messages.
func (h *Handler) setTaskSuspend(w http.ResponseWriter, r *http.Request) {
	identity, task, ok := h.authorizedTask(w, r)
	if !ok {
		return
	}
	var req suspendReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil {
		jsonError(w, err, 400)
		return
	}
	if !req.Suspend && task.Status.Completed() {
		jsonError(w, fmt.Errorf("completed Task cannot be resumed"), http.StatusConflict)
		return
	}
	if task.Spec.Suspend == req.Suspend {
		jsonOK(w, map[string]bool{"suspend": req.Suspend})
		return
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Spec.Suspend = req.Suspend
	if err := h.Client.Patch(r.Context(), task, patch); err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	action := "Suspended"
	if !req.Suspend {
		action = "Resumed"
	}
	h.recordUserAction(r, task, identity, action)
	jsonOK(w, map[string]bool{"suspend": req.Suspend})
}

// setMCPDisabled adds or removes one MCP endpoint from Task.spec.disabledMCPs.
func (h *Handler) setMCPDisabled(w http.ResponseWriter, r *http.Request) {
	identity, task, ok := h.authorizedTask(w, r)
	if !ok {
		return
	}
	var req mcpToggleReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || !mcpNamePattern.MatchString(req.Name) {
		jsonError(w, fmt.Errorf("a valid MCP name is required"), 400)
		return
	}
	if req.Name == "agent_operator" {
		jsonError(w, fmt.Errorf("agent_operator is required by the worker"), http.StatusConflict)
		return
	}
	disabled := make([]string, 0, len(task.Spec.DisabledMCPs)+1)
	for _, name := range task.Spec.DisabledMCPs {
		if name != req.Name {
			disabled = append(disabled, name)
		}
	}
	if req.Disabled {
		if len(disabled) >= 32 {
			jsonError(w, fmt.Errorf("too many disabled MCPs"), 400)
			return
		}
		disabled = append(disabled, req.Name)
	}
	if slices.Equal(disabled, task.Spec.DisabledMCPs) {
		jsonOK(w, map[string][]string{"disabledMcps": disabled})
		return
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Spec.DisabledMCPs = disabled
	if err := h.Client.Patch(r.Context(), task, patch); err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	action := "Enabled MCP " + req.Name
	if req.Disabled {
		action = "Disabled MCP " + req.Name
	}
	h.recordUserAction(r, task, identity, action)
	jsonOK(w, map[string][]string{"disabledMcps": disabled})
}

// stopTurn interrupts the current CLI turn of the running worker. The worker
// keeps its Pod, PVC and native session for follow-up prompts.
func (h *Handler) stopTurn(w http.ResponseWriter, r *http.Request) {
	identity, task, ok := h.authorizedTask(w, r)
	if !ok {
		return
	}
	if h.AMQP == nil {
		jsonError(w, fmt.Errorf("RabbitMQ not configured"), http.StatusServiceUnavailable)
		return
	}
	if task.Spec.Suspend || task.Status.Phase != agentsv1alpha1.TaskPhaseRunning || task.Status.WorkerPodUID == "" || task.Status.Activity != "working" {
		jsonError(w, fmt.Errorf("Task has no running turn"), http.StatusConflict)
		return
	}
	podUID := task.Status.WorkerPodUID
	taskQueue, err := h.taskQueue(r.Context(), task)
	if err != nil {
		jsonError(w, err, http.StatusServiceUnavailable)
		return
	}
	queue := rabbitmq.ProbeQueue(taskQueue, podUID)
	if err := h.AMQP.DeclareProbeQueue(queue); err != nil {
		jsonError(w, fmt.Errorf("control queue could not be declared: %w", err), http.StatusServiceUnavailable)
		return
	}
	msg := rabbitmq.PromptMessage{Type: "stop", MessageID: randHex(16), PodUID: podUID, ReceivedAt: time.Now().UnixMilli()}
	if err := h.AMQP.Publish(r.Context(), queue, msg); err != nil {
		jsonError(w, fmt.Errorf("stop request could not be published"), http.StatusServiceUnavailable)
		return
	}
	h.recordUserAction(r, task, identity, "Stop requested")
	jsonOK(w, map[string]string{"requestId": msg.MessageID})
}

// recordUserAction appends a bounded lifecycle line; the action already
// succeeded, so a status write failure is not reported to the caller.
func (h *Handler) recordUserAction(r *http.Request, task *agentsv1alpha1.Task, identity caller, action string) {
	display := identity.name
	if display == "" {
		display = identity.username
	}
	content := action
	if display != "" {
		content += " by " + display
	}
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Task
		if err := h.Client.Get(r.Context(), client.ObjectKeyFromObject(task), &current); err != nil {
			return err
		}
		current.Status.AppendMessage(agentsv1alpha1.TaskMessage{ID: "action:" + randHex(12), Role: "system", Source: "web",
			Actor: identity.user, Content: content, CreatedAt: metav1.Now()})
		return h.Client.Status().Update(r.Context(), &current)
	})
}

func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	identity, ok := h.authenticatedCaller(w, r)
	if !ok {
		return
	}
	ns := r.PathValue("namespace")
	name := r.PathValue("name")
	var task agentsv1alpha1.Task
	if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &task); err != nil {
		jsonError(w, err, 404)
		return
	}
	allowed, err := h.canAccessTask(r, identity, &task)
	if err != nil {
		jsonError(w, err, 503)
		return
	}
	if !allowed {
		jsonError(w, fmt.Errorf("access to Task denied"), http.StatusForbidden)
		return
	}
	if err := h.Client.Delete(r.Context(), &task); err != nil {
		jsonError(w, err, 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Helpers ---

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	words := strings.SplitN(s, "-", 6)
	if len(words) > 5 {
		words = words[:5]
	}
	slug := strings.Join(words, "-")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "task"
	}
	return slug
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b) //nolint:errcheck
	return fmt.Sprintf("%x", b)
}

func boolPtr(b bool) *bool { return &b }

func int32Ptr(v int32) *int32 { return &v }

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func jsonError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}) //nolint:errcheck
}
