// Package v1alpha1 contains the Agent and Task CRD types for codemowers.cloud.
package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "codemowers.cloud", Version: "v1alpha1"}
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=".spec.model.backend"
// +kubebuilder:printcolumn:name="Tasks",type=integer,JSONPath=".status.taskCount"
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
	// Suspended makes newly created Tasks start suspended. Existing Tasks keep
	// their own suspend state.
	Suspended bool `json:"suspended,omitempty"`
	// IdleSeconds is copied into new Tasks. Zero shuts the worker down as soon
	// as its queue is empty after a completed turn; nil defaults to one hour.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	IdleSeconds *int32 `json:"idleSeconds,omitempty"`
	// ResourcePrefix confines the worker ServiceAccount, through the
	// agent-resource-prefix admission policy, to resources named with it and
	// starts the names of worker Pods and Task PVCs. Empty leaves the worker to
	// plain RBAC and names Pods and PVCs "<Agent name>-".
	// +kubebuilder:validation:MaxLength=30
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?-$`
	ResourcePrefix string `json:"resourcePrefix,omitempty"`
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

	// MCPEndpoints lists MCP servers available to workers, keyed by name.
	// +listType=map
	// +listMapKey=name
	MCPEndpoints []MCPEndpoint `json:"mcpEndpoints,omitempty"`
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

type ModelSpec struct {
	// Backend is claude or codex.
	// +kubebuilder:validation:Enum=claude;codex
	Backend string `json:"backend"`
	// Name is the provider model identifier. Empty uses the CLI default.
	Name string `json:"name,omitempty"`

	// CredentialKeyRef selects an API key in the namespace's provider-token Secret.
	CredentialKeyRef string `json:"credentialKeyRef"`
}

type GitProvider struct {
	// +kubebuilder:validation:Enum=github;gitea
	Type      string `json:"type"`
	ServerURL string `json:"serverURL"`
	// CredentialKeyRef selects a token in the namespace's provider-token Secret.
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

type MCPEndpoint struct {
	// Name identifies the server in CLI tool names, probe results and
	// Task.spec.disabledMCPs. agent_operator and devices are reserved.
	Name      string `json:"name"`
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile,omitempty"`
	SiteURL   string `json:"siteURL,omitempty"`
}

// WorkerMCPEndpoints returns the Agent's endpoints followed by the
// operator-provided agent_operator and devices endpoints. Agent entries with
// a reserved name are ignored.
//
// mailboxes names the Mailboxes the worker ServiceAccount may send or read.
// The operator and Device bridge run in the harness namespace; Mailboxes run
// in the Task's namespace.
func WorkerMCPEndpoints(agent *Agent, harnessNamespace, namespace string, mailboxes []string) []MCPEndpoint {
	endpoints := make([]MCPEndpoint, 0, len(agent.Spec.MCPEndpoints)+2)
	for _, endpoint := range agent.Spec.MCPEndpoints {
		if endpoint.Name != "agent_operator" && endpoint.Name != "devices" {
			endpoints = append(endpoints, endpoint)
		}
	}
	endpoints = append(endpoints, MCPEndpoint{Name: "agent_operator",
		URL:       OperatorMCPURL(harnessNamespace),
		TokenFile: "/var/run/agent-operator-mcp/token"})
	// Kubernetes RBAC on each Device decides access; the bridge checks it per call.
	endpoints = append(endpoints, MCPEndpoint{Name: "devices",
		URL:       "http://agent-devices." + harnessNamespace + ".svc.cluster.local:8080/mcp",
		TokenFile: "/var/run/agent-devices/token"})
	// Each Mailbox Pod serves its own MCP and still checks RBAC per call.
	for _, mailbox := range mailboxes {
		endpoints = append(endpoints, MCPEndpoint{Name: "mailbox-" + mailbox,
			URL:       "http://" + MailboxResourceName(mailbox) + "." + namespace + ".svc.cluster.local:8080/mcp",
			TokenFile: "/var/run/agent-mailbox/token"})
	}
	return endpoints
}

// OperatorMCPURL is the execution-bound Agent MCP endpoint of the operator.
func OperatorMCPURL(harnessNamespace string) string {
	return "http://agent-operator." + harnessNamespace + ".svc.cluster.local:8083/mcp"
}

type AgentStatus struct {
	TaskCount                int                `json:"taskCount,omitempty"`
	Conditions               []metav1.Condition `json:"conditions,omitempty"`
	Forges                   []ForgeCapability  `json:"forges,omitempty"`
	ForgesCheckedAt          *metav1.Time       `json:"forgesCheckedAt,omitempty"`
	ForgesObservedGeneration int64              `json:"forgesObservedGeneration,omitempty"`
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

type TaskStatus struct {
	Phase TaskPhase `json:"phase,omitempty"`
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
	Progress       *TaskProgress     `json:"progress,omitempty"`
	MCPProbe       *MCPProbeStatus   `json:"mcpProbe,omitempty"`
	Conversation   []TaskMessage     `json:"conversation,omitempty"`
	// LastMessageAt survives bounded conversation history and changes only for
	// user, external, and agent messages, never lifecycle status entries.
	LastMessageAt *metav1.Time `json:"lastMessageAt,omitempty"`
	// ExternalReferences are source links collected after Task creation.
	ExternalReferences []ExternalReference `json:"externalReferences,omitempty"`
	LastResultAt       *metav1.Time        `json:"lastResultAt,omitempty"`
	Outputs            map[string]string   `json:"outputs,omitempty"`
	Conditions         []metav1.Condition  `json:"conditions,omitempty"`
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
	ID        string       `json:"id"`
	Role      string       `json:"role"`
	Source    string       `json:"source,omitempty"`
	Actor     string       `json:"actor,omitempty"`
	Backend   string       `json:"backend,omitempty"`
	Model     string       `json:"model,omitempty"`
	Metrics   *TurnMetrics `json:"metrics,omitempty"`
	Content   string       `json:"content"`
	URL       string       `json:"url,omitempty"`
	CreatedAt metav1.Time  `json:"createdAt"`
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
