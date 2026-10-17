// Package v1alpha1 contains the Agent and Task CRD types for agents.codemowers.cloud.
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion = schema.GroupVersion{Group: "agents.codemowers.cloud", Version: "v1alpha1"}
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=".spec.model.backend"
// +kubebuilder:printcolumn:name="Tasks",type=integer,JSONPath=".status.taskCount"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// Agent defines a privilege domain: which credentials and MCP endpoints worker
// pods may access. Task CRs are owned by an Agent via ownerReferences; deleting
// the Agent cascades to all its Tasks and their worker Pods and PVCs.
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

	// Template is the pod template for worker pods, identical in structure to
	// Deployment/StatefulSet spec.template. template.spec is corev1.PodSpec;
	// template.metadata carries pod-level labels and annotations. The operator:
	//   - deep-copies template and appends workspace PVC and credential volumes
	//   - supplies task, model and broker environment to the container named worker
	//   - sets the worker image from the operator deployment's AGENT_WORKER_IMAGE
	//   - enforces restartPolicy=Never and hostUsers=false
	//   - defaults serviceAccountName to "agent-worker" when empty
	Template corev1.PodTemplateSpec `json:"template"`

	// Access is reserved for future use. Authorization currently ignores it and
	// checks Kubernetes Agent get rights instead.
	Access AgentAccess `json:"access,omitempty"`

	// Model selects the AI backend and its credential secret.
	Model ModelSpec `json:"model"`

	// VolumeClaimTemplates is analogous to StatefulSet's volumeClaimTemplates.
	// The operator stamps one PVC per Task per entry, using metadata.name as the
	// volume name and PVC name suffix. Mount by name from podSpec.containers[*].volumeMounts.
	VolumeClaimTemplates []VolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`

	// GitProviders lists git server credentials available to workers.
	GitProviders []GitProvider `json:"gitProviders,omitempty"`

	// GitHubWebhooks subscribes this Agent to GitHub hook IDs. Each signing
	// Secret lives in the Agent's namespace and has a token key.
	GitHubWebhooks []GitHubWebhook `json:"githubWebhooks,omitempty"`

	// SlackWorkspaces lists Slack workspaces the controller can read/write.
	// Credentials are controller-only (agent-ctrl-* prefix).
	SlackWorkspaces []SlackWorkspace `json:"slackWorkspaces,omitempty"`

	// MCPEndpoints lists MCP servers available to workers.
	MCPEndpoints []MCPEndpoint `json:"mcpEndpoints,omitempty"`
}

type GitHubWebhook struct {
	ServerURL string `json:"serverURL"`
	HookID    string `json:"hookID"`
	SecretRef string `json:"secretRef"`
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

// AgentAccess is retained in the schema but currently ignored.
type AgentAccess struct {
	// Groups is a list of OIDC group names (e.g. "github.com:codemowers:admins").
	Groups []string `json:"groups,omitempty"`
	// Subjects is a list of individual OIDC subjects, typically email addresses.
	Subjects []string `json:"subjects,omitempty"`
}

type ModelSpec struct {
	// Backend is claude or codex.
	// +kubebuilder:validation:Enum=claude;codex
	Backend string `json:"backend"`
	// Name is the provider model identifier. Empty uses the CLI default.
	Name string `json:"name,omitempty"`

	// CredentialSecretRef is the secret name (agent-cred-* prefix) holding the API key.
	CredentialSecretRef string `json:"credentialSecretRef"`
}

type GitProvider struct {
	// +kubebuilder:validation:Enum=github;gitea
	Type      string `json:"type"`
	ServerURL string `json:"serverURL"`
	// CredentialSecretRef names a secret (agent-cred-* prefix) with a `token` key.
	CredentialSecretRef string `json:"credentialSecretRef"`
}

type SlackWorkspace struct {
	WorkspaceID string `json:"workspaceId"`
	// BotTokenSecretRef names an agent-ctrl-* secret with the bot OAuth token.
	BotTokenSecretRef string `json:"botTokenSecretRef"`
	// SigningSecretRef names an agent-hook-* secret for request verification.
	SigningSecretRef string `json:"signingSecretRef"`
}

type MCPEndpoint struct {
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	TokenFile string `json:"tokenFile,omitempty"`
	SiteURL   string `json:"siteURL,omitempty"`
}

type AgentStatus struct {
	TaskCount  int                `json:"taskCount,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
	// Objective is the human-readable goal for the worker.
	Objective string `json:"objective"`

	// Suspend stops the current worker and prevents a new one from starting.
	// Analogous to CronJob suspend.
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`

	// ExternalReferences maps this Task to external anchors for event routing.
	ExternalReferences []ExternalReference `json:"externalReferences,omitempty"`

	// CompletionPolicy defines when the controller considers this Task done.
	CompletionPolicy *CompletionPolicy `json:"completionPolicy,omitempty"`

	// Model overrides the Agent's default model for this Task.
	Model *ModelSpec `json:"model,omitempty"`
}

type ExternalReference struct {
	// +kubebuilder:validation:Enum=github;gitea;slack;alertmanager
	Source          string `json:"source"`
	ServerURL       string `json:"serverURL,omitempty"`
	RepositoryOwner string `json:"repositoryOwner,omitempty"`
	RepositoryName  string `json:"repositoryName,omitempty"`
	WorkspaceID     string `json:"workspaceId,omitempty"`
	ChannelID       string `json:"channelId,omitempty"`
	// +kubebuilder:validation:Enum=issue;pull_request;thread;alert
	Kind string `json:"kind"`
	// ID is the issue number, PR number, Slack thread_ts or Alertmanager fingerprint.
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
	Phase          TaskPhase          `json:"phase,omitempty"`
	Reason         string             `json:"reason,omitempty"`
	WorkerPodName  string             `json:"workerPodName,omitempty"`
	WorkerPodUID   string             `json:"workerPodUID,omitempty"`
	WorkspacePVCs  map[string]string  `json:"workspacePVCs,omitempty"`
	SessionEpoch   int                `json:"sessionEpoch,omitempty"`
	QueueMessageID string             `json:"queueMessageId,omitempty"`
	Progress       *TaskProgress      `json:"progress,omitempty"`
	Outputs        map[string]string  `json:"outputs,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
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
