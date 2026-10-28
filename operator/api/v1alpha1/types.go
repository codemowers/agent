// Package v1alpha1 contains the Agent and Task CRD types for codemowers.ai.
package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	neturl "net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "codemowers.ai", Version: "v1alpha1"}
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=".spec.model.backend"
// +kubebuilder:printcolumn:name="Tasks",type=integer,JSONPath=".status.taskCount"
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=".spec.suspend"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Agent defines a privilege domain: which credentials and MCP endpoints worker
// pods may access. Tasks select an Agent through spec.agentRef; the operator
// adds an ownerReference so deleting the Agent cascades to its Tasks.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec"`
	Status AgentStatus `json:"status,omitempty"`
}

type AgentSpec struct {
	// Suspend stops the workers of every Task of the Agent and keeps new ones
	// from starting, retaining PVCs, native sessions and queued messages. Each
	// Task keeps its own spec.suspend, so resuming the Agent resumes only Tasks
	// that are not suspended themselves.
	Suspend bool `json:"suspend,omitempty"`
	// IdleSeconds is copied into new Tasks. Zero shuts the worker down as soon
	// as its queue is empty after a completed turn; nil defaults to one hour.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	IdleSeconds *int32 `json:"idleSeconds,omitempty"`
	// CoalesceSeconds is the default event coalescing window of its Tasks.
	// The first event on a routing key wakes the Task at once and opens the
	// window. Within it, events from other bindings are notifications of the
	// same activity and only recorded; events from the same binding receive
	// one trailing wake. Zero wakes on every event; nil defaults to three
	// minutes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	CoalesceSeconds *int32 `json:"coalesceSeconds,omitempty"`
	// ResourcePrefix confines the worker ServiceAccount, through the
	// agent-resource-prefix admission policy, to resources named with it and
	// starts the names of worker Pods and Task PVCs. Empty leaves the worker to
	// plain RBAC and names Pods and PVCs "<Agent name>-".
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?-$`
	ResourcePrefix string `json:"resourcePrefix,omitempty"`
	// CredentialSecretName names the Secret in the Agent's namespace that
	// holds the keys its credentialKeyRefs select, so Agents sharing a
	// namespace can keep their credentials apart. Empty uses
	// agent-operator-provider-tokens.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	CredentialSecretName string `json:"credentialSecretName,omitempty"`
	// Instructions describes the Agent's role to its workers and leads every
	// turn's prompt; changes reach the next worker Pod.
	// +kubebuilder:validation:MaxLength=4096
	Instructions string `json:"instructions,omitempty"`
	// Resources is copied to each new Task and controls the worker container's
	// requests and limits. If absent, the worker template resources are used.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Template is the pod template for worker pods, identical in structure to
	// Deployment/StatefulSet spec.template. template.spec is corev1.PodSpec;
	// template.metadata carries pod-level labels and annotations. The operator:
	//   - deep-copies template and appends workspace PVC and credential volumes
	//   - supplies task, model and broker environment to the container named worker
	//   - sets the worker image from the operator deployment's AGENT_WORKER_IMAGE
	//   - enforces restartPolicy=Never and hostUsers=false
	//   - defaults serviceAccountName to "agent-worker-<Agent name>" when empty
	Template corev1.PodTemplateSpec `json:"template"`

	// Model selects the AI backend and its credential secret.
	Model ModelSpec `json:"model"`

	// VolumeClaimTemplates is analogous to StatefulSet's volumeClaimTemplates.
	// The operator stamps one PVC per Task per entry, using metadata.name as the
	// volume name and PVC name suffix. Mount by name from podSpec.containers[*].volumeMounts.
	VolumeClaimTemplates []VolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`

	// GitProviders lists git server credentials available to workers.
	GitProviders []GitProvider `json:"gitProviders,omitempty"`

	// Access, when set, alone decides which dashboard users may read this
	// Agent's Tasks, create them and prompt or control them; an empty Access
	// admits nobody. Without it, Kubernetes RBAC get on the Agent decides.
	Access *AgentAccess `json:"access,omitempty"`
}

// AgentAccess names dashboard users by the operator's OIDC issuer identity.
type AgentAccess struct {
	// Users are OIDC subject (sub) claims.
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Users []string `json:"users,omitempty"`
	// Groups are OIDC groups claim values.
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Groups []string `json:"groups,omitempty"`
}

// Admits reports whether Access names the subject or one of its groups.
func (a *AgentAccess) Admits(subject string, groups []string) bool {
	if subject != "" && slices.Contains(a.Users, subject) {
		return true
	}
	for _, group := range groups {
		if slices.Contains(a.Groups, group) {
			return true
		}
	}
	return false
}

// VolumeClaimTemplate follows StatefulSet's metadata/spec claim shape.
type VolumeClaimTemplate struct {
	Metadata VolumeClaimTemplateMetadata      `json:"metadata"`
	Spec     corev1.PersistentVolumeClaimSpec `json:"spec"`
}

type VolumeClaimTemplateMetadata struct {
	Name        string            `json:"name"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.backend == 'opencode' ? has(self.baseURL) && has(self.name) : !has(self.baseURL)",message="opencode requires baseURL and name; other backends take no baseURL"
type ModelSpec struct {
	// Backend is claude, codex or opencode.
	// +kubebuilder:validation:Enum=claude;codex;opencode
	Backend string `json:"backend"`
	// Name is the provider model identifier. Empty uses the CLI default.
	Name string `json:"name,omitempty"`
	// BaseURL is the OpenAI-compatible API, ending in /v1, that the opencode
	// backend calls, such as a llama.cpp server.
	BaseURL string `json:"baseURL,omitempty"`

	// CredentialKeyRef selects an API key in the Agent's credential Secret.
	CredentialKeyRef string `json:"credentialKeyRef"`
}

type GitProvider struct {
	// +kubebuilder:validation:Enum=github;gitea
	Type      string `json:"type"`
	ServerURL string `json:"serverURL"`
	// CredentialKeyRef selects a token in the Agent's credential Secret.
	CredentialKeyRef string `json:"credentialKeyRef"`
}

// MailboxResourceName names a Mailbox's Pod and Service.
func MailboxResourceName(mailbox string) string {
	base := "agent-mailbox-" + mailbox
	if len(base) <= 63 {
		return base
	}
	sum := sha256.Sum256([]byte(mailbox))
	return base[:56] + "-" + hex.EncodeToString(sum[:3])
}

// MCPEndpoint is an MCP server in a worker's configuration.
type MCPEndpoint struct {
	// Name identifies the server in CLI tool names, probe results and
	// Task.spec.disabledMCPs.
	Name      string `json:"name"`
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile,omitempty"`
}

// WorkerMCPEndpoints returns the operator's agent_operator endpoint followed
// by the advertised servers the worker may use and its Mailboxes. The
// operator runs in the harness namespace; Mailboxes in the Task's namespace.
func WorkerMCPEndpoints(harnessNamespace, namespace string, servers []MCPEndpoint, mailboxes []string) []MCPEndpoint {
	endpoints := make([]MCPEndpoint, 0, len(servers)+len(mailboxes)+1)
	endpoints = append(endpoints, MCPEndpoint{Name: "agent_operator",
		URL:       OperatorMCPURL(harnessNamespace),
		TokenFile: WorkerTokenDir + "/agent_operator"})
	endpoints = append(endpoints, servers...)
	// Each Mailbox Pod serves its own MCP and still checks RBAC per call.
	for _, mailbox := range mailboxes {
		endpoints = append(endpoints, MCPEndpoint{Name: "mailbox-" + mailbox,
			URL: MailboxMCPURL(mailbox, namespace), TokenFile: WorkerTokenDir + "/mcp/mailbox-" + mailbox})
	}
	return endpoints
}

// OperatorMCPURL is the execution-bound Agent MCP endpoint of the operator.
func OperatorMCPURL(harnessNamespace string) string {
	return "http://agent-operator." + harnessNamespace + ".svc.cluster.local:8083/mcp"
}

// OperatorWakeURL is the base URL of the wake endpoint. It has a Service of
// its own so that its token audience differs from the Agent MCP's.
func OperatorWakeURL(harnessNamespace string) string {
	return "http://agent-operator-wake." + harnessNamespace + ".svc.cluster.local"
}

// MailboxMCPURL is a Mailbox's MCP endpoint.
func MailboxMCPURL(mailbox, namespace string) string {
	return "http://" + MailboxResourceName(mailbox) + "." + namespace + ".svc.cluster.local:8080/mcp"
}

// ServiceAudience is the projected token audience of an in-cluster service:
// <service>.<namespace>. Each such Service checks tokens on one port only.
func ServiceAudience(url string) string {
	parsed, err := neturl.Parse(url)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	host, found := strings.CutSuffix(parsed.Hostname(), ".svc.cluster.local")
	if !found {
		host, found = strings.CutSuffix(parsed.Hostname(), ".svc")
	}
	if !found || strings.Count(host, ".") != 1 {
		return ""
	}
	return host
}

// WorkerMetricsPort serves a worker's /metrics and /readyz.
const WorkerMetricsPort = 9464

// WorkerTokenDir holds one projected token file per audience a worker uses.
const WorkerTokenDir = "/var/run/agent-tokens"

type AgentStatus struct {
	TaskCount       int                `json:"taskCount,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
	Forges          []ForgeCapability  `json:"forges,omitempty"`
	ForgesCheckedAt *metav1.Time       `json:"forgesCheckedAt,omitempty"`
	// ForgesObservedProviders is ForgeProvidersHash of the gitProviders the
	// snapshot was probed with, so other spec changes such as suspend keep it
	// current.
	ForgesObservedProviders string `json:"forgesObservedProviders,omitempty"`
}

// ForgeProvidersHash identifies an Agent's gitProviders for its forge snapshot.
func ForgeProvidersHash(agent *Agent) string {
	sum := sha256.New()
	for _, provider := range agent.Spec.GitProviders {
		sum.Write([]byte(provider.Type + "\x00" + provider.ServerURL + "\x00" + provider.CredentialKeyRef + "\x00"))
	}
	return hex.EncodeToString(sum.Sum(nil)[:8])
}

// ForgesCurrent reports whether the forge snapshot reflects the current
// gitProviders.
func ForgesCurrent(agent *Agent) bool {
	return agent.Status.ForgesCheckedAt != nil && agent.Status.ForgesObservedProviders == ForgeProvidersHash(agent)
}

// ForgeCapability is a bounded, non-secret snapshot of one Agent credential's
// visible repositories and repository-creation destinations.
type ForgeCapability struct {
	Type            string             `json:"type"`
	ServerURL       string             `json:"serverURL"`
	Status          string             `json:"status"`
	Detail          string             `json:"detail,omitempty"`
	Login           string             `json:"login,omitempty"`
	RepositoryCount int                `json:"repositoryCount,omitempty"`
	Clipped         bool               `json:"clipped,omitempty"`
	Repositories    []ForgeRepository  `json:"repositories,omitempty"`
	Destinations    []ForgeDestination `json:"destinations,omitempty"`
}

type ForgeRepository struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Writable bool   `json:"writable,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	Private  bool   `json:"private,omitempty"`
}

type ForgeDestination struct {
	Owner           string `json:"owner"`
	Kind            string `json:"kind"`
	Policy          string `json:"policy"`
	TokenPermission string `json:"tokenPermission"`
	Role            string `json:"role,omitempty"`
	URL             string `json:"url"`
}

// +kubebuilder:object:root=true
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=task
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.reason"
// +kubebuilder:printcolumn:name="Worker",type=string,JSONPath=".status.workerPodName"
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=".spec.suspend"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Task tracks one unit of work. It is owned by an Agent CR. Deleting a Task
// cascades to its worker Pod and workspace PVC via ownerReferences.
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskSpec   `json:"spec"`
	Status TaskStatus `json:"status,omitempty"`
}

type TaskSpec struct {
	// AgentRef names the Agent in this Task's namespace. The operator adds the
	// corresponding ownerReference and routing label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="agentRef is immutable"
	AgentRef string `json:"agentRef"`
	// Objective is the human-readable goal for the worker.
	Objective string `json:"objective"`
	// Title is a short display name for the Task. The worker may set it through
	// the operator's execution-bound Agent MCP; the Task name remains its ID.
	// +kubebuilder:validation:MaxLength=120
	Title string `json:"title,omitempty"`
	// IdleSeconds is the Task's copied or overridden worker idle timeout.
	// A change replaces the current worker Pod after it terminates.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	IdleSeconds *int32 `json:"idleSeconds,omitempty"`
	// CoalesceSeconds overrides the Agent's event coalescing window for this
	// Task, such as a longer one for an alert fallback Task.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	CoalesceSeconds *int32 `json:"coalesceSeconds,omitempty"`
	// Resources is the Task's copied or overridden worker requests and limits.
	// Changes replace the current worker Pod.
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Suspend stops the current worker and prevents a new one from starting.
	// Analogous to CronJob suspend.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// RoutingKeys are exact canonical resource URIs scoped to this Task's
	// Agent and namespace. The Task name remains its primary identity.
	RoutingKeys []string `json:"routingKeys,omitempty"`

	// ExternalReferences are starting source links. A matching forge reference
	// selects checkout; later observations belong in status.
	ExternalReferences []ExternalReference `json:"externalReferences,omitempty"`

	// CompletionPolicy defines when the controller considers this Task done.
	CompletionPolicy *CompletionPolicy `json:"completionPolicy,omitempty"`

	// Model overrides the Agent's default model for this Task.
	Model *ModelSpec `json:"model,omitempty"`

	// DisabledMCPs names MCP endpoints left out of the worker's CLI
	// configuration. Changes reach a running worker before its next turn.
	// The agent_operator endpoint cannot be disabled.
	DisabledMCPs []string `json:"disabledMCPs,omitempty"`
}

type ExternalReference struct {
	// +kubebuilder:validation:Enum=github;gitea;slack;alertmanager;imap
	Source          string `json:"source"`
	ServerURL       string `json:"serverURL,omitempty"`
	RepositoryOwner string `json:"repositoryOwner,omitempty"`
	RepositoryName  string `json:"repositoryName,omitempty"`
	WorkspaceID     string `json:"workspaceId,omitempty"`
	ChannelID       string `json:"channelId,omitempty"`
	MailboxRef      string `json:"mailboxRef,omitempty"`
	MailboxUID      string `json:"mailboxUid,omitempty"`
	Folder          string `json:"folder,omitempty"`
	// IMAPThreadID is the optional server-assigned OBJECTID or Gmail thread ID.
	IMAPThreadID string `json:"imapThreadId,omitempty"`
	// +kubebuilder:validation:Enum=issue;pull_request;thread;alert
	Kind string `json:"kind"`
	// ID is an issue/PR number, Slack thread_ts, Alertmanager fingerprint,
	// or the RFC Message-ID at the root of an IMAP thread.
	ID string `json:"id"`
}

type CompletionPolicy struct {
	// +kubebuilder:validation:Enum=PullRequestCreated;VerifiedChecks;Approved;Merged
	Milestone      string   `json:"milestone"`
	RequiredChecks []string `json:"requiredChecks,omitempty"`
}

// TaskPhase describes the current lifecycle state of the Task.
// +kubebuilder:validation:Enum=Pending;Running;Waiting;Paused;Completed;Failed
type TaskPhase string

const (
	TaskPhasePending   TaskPhase = "Pending"
	TaskPhaseRunning   TaskPhase = "Running"
	TaskPhaseWaiting   TaskPhase = "Waiting"
	TaskPhasePaused    TaskPhase = "Paused"
	TaskPhaseCompleted TaskPhase = "Completed"
	TaskPhaseFailed    TaskPhase = "Failed"
)

// TaskEvents records routed event deliveries and pending wakes.
type TaskEvents struct {
	// InitialDeliveryID is the receipt of the event that created the Task.
	InitialDeliveryID string `json:"initialDeliveryID,omitempty"`
	// Receipts holds the latest accepted delivery receipts, newest last.
	Receipts []string `json:"receipts,omitempty"`
	// WakePending asks the reconciler to start a worker for queued messages.
	WakePending bool `json:"wakePending,omitempty"`
	// Windows maps routing keys to their open coalescing windows.
	Windows map[string]EventWindow `json:"windows,omitempty"`
}

// EventWindow follows the first event on a routing key.
type EventWindow struct {
	// Binding is the UID of the binding whose event opened the window.
	Binding string      `json:"binding"`
	Until   metav1.Time `json:"until"`
	// Trailing is set when the same binding delivered more events.
	Trailing bool `json:"trailing,omitempty"`
}

// TaskCompletion records who completed a Task and why.
type TaskCompletion struct {
	// Requested is set by the worker on its own Task; completion follows its
	// turn result.
	Requested   bool         `json:"requested,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Actor       string       `json:"actor,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// Completed reports a finished Task.
func (s *TaskStatus) Completed() bool {
	return s.Completion != nil && s.Completion.CompletedAt != nil
}

type TaskStatus struct {
	// DeliveredMessageIDs preserves bounded input receipts across publish/status races.
	DeliveredMessageIDs []string `json:"deliveredMessageIds,omitempty"`
	// TurnID and TurnStartedAt identify the current model turn, not Pod uptime.
	TurnID        string       `json:"turnId,omitempty"`
	TurnStartedAt *metav1.Time `json:"turnStartedAt,omitempty"`
	Phase         TaskPhase    `json:"phase,omitempty"`
	// Activity is reported by the current worker and reconciled on Pod exit.
	// +kubebuilder:validation:Enum=starting;working;idle;stopped
	Activity       string            `json:"activity,omitempty"`
	Reason         string            `json:"reason,omitempty"`
	WorkerPodName  string            `json:"workerPodName,omitempty"`
	WorkerPodUID   string            `json:"workerPodUID,omitempty"`
	LastWorkerExit string            `json:"lastWorkerExit,omitempty"`
	WorkspacePVCs  map[string]string `json:"workspacePVCs,omitempty"`
	SessionEpoch   int               `json:"sessionEpoch,omitempty"`
	QueueMessageID string            `json:"queueMessageId,omitempty"`
	// Queue is the declared RabbitMQ Task queue, deleted with the Task even
	// after its Agent is gone.
	Queue        string          `json:"queue,omitempty"`
	Progress     *TaskProgress   `json:"progress,omitempty"`
	MCPProbe     *MCPProbeStatus `json:"mcpProbe,omitempty"`
	Conversation []TaskMessage   `json:"conversation,omitempty"`
	// LastMessageAt survives bounded conversation history and changes only for
	// user, external, and agent messages, never lifecycle status entries.
	LastMessageAt *metav1.Time `json:"lastMessageAt,omitempty"`
	// ExternalReferences are source links collected after Task creation.
	ExternalReferences []ExternalReference `json:"externalReferences,omitempty"`
	LastResultAt       *metav1.Time        `json:"lastResultAt,omitempty"`
	// Events is the operator's intake bookkeeping for routed events.
	Events TaskEvents `json:"events,omitempty"`
	// Completion is set when the worker or a closing forge event completes the Task.
	Completion *TaskCompletion `json:"completion,omitempty"`
	// ObjectivePublished records that the objective reached the Task queue.
	ObjectivePublished bool `json:"objectivePublished,omitempty"`
	// RuntimePublishedGeneration is the latest Task generation sent to the
	// resident worker as runtime configuration.
	RuntimePublishedGeneration int64 `json:"runtimePublishedGeneration,omitempty"`
	// RetryWorker authorizes replacing a worker the operator stopped itself.
	RetryWorker bool               `json:"retryWorker,omitempty"`
	Outputs     map[string]string  `json:"outputs,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// MCPProbeStatus is the latest worker-side connectivity check for this Task.
// It contains only endpoint names and bounded results, never credentials or URLs.
type MCPProbeStatus struct {
	RequestID string      `json:"requestId"`
	PodUID    string      `json:"podUid"`
	State     string      `json:"state"`
	UpdatedAt metav1.Time `json:"updatedAt"`
	// CheckedAt is the time of the last completed check. Endpoints keep that
	// result while a newer check is pending or after it failed.
	CheckedAt *metav1.Time        `json:"checkedAt,omitempty"`
	Endpoints []MCPEndpointStatus `json:"endpoints,omitempty"`
}

type MCPEndpointStatus struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// ServerTitle is the server's own serverInfo.title.
	ServerTitle string `json:"serverTitle,omitempty"`
	// Instructions is the server's bounded initialize instructions text.
	Instructions string `json:"instructions,omitempty"`
	// Tools lists the tool names the server advertised.
	Tools []string `json:"tools,omitempty"`
}

// TaskMessage is one bounded, displayable turn in a Task conversation.
type TaskMessage struct {
	// DeliveryState is queued until the worker starts the turn containing this input.
	// +kubebuilder:validation:Enum=queued;delivered
	DeliveryState string       `json:"deliveryState,omitempty"`
	ID            string       `json:"id"`
	Role          string       `json:"role"`
	Source        string       `json:"source,omitempty"`
	Actor         string       `json:"actor,omitempty"`
	Backend       string       `json:"backend,omitempty"`
	Model         string       `json:"model,omitempty"`
	Metrics       *TurnMetrics `json:"metrics,omitempty"`
	Content       string       `json:"content"`
	URL           string       `json:"url,omitempty"`
	CreatedAt     metav1.Time  `json:"createdAt"`
}

// TurnMetrics contains measurements for one completed CLI turn.
type TurnMetrics struct {
	DurationMs       *int64 `json:"durationMs,omitempty"`
	APIDurationMs    *int64 `json:"apiDurationMs,omitempty"`
	InputTokens      *int64 `json:"inputTokens,omitempty"`
	OutputTokens     *int64 `json:"outputTokens,omitempty"`
	CacheReadTokens  *int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens,omitempty"`
}

type TaskProgress struct {
	Summary   string      `json:"summary,omitempty"`
	UpdatedAt metav1.Time `json:"updatedAt,omitempty"`
}

// +kubebuilder:object:root=true
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}
