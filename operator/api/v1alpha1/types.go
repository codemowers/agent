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
	// Template is the pod template for worker pods, identical in structure to
	// Deployment/StatefulSet spec.template. template.spec is corev1.PodSpec;
	// template.metadata carries pod-level labels and annotations. The operator:
	//   - deep-copies template and appends workspace PVC and credential volumes
	//   - prepends TASK_NAME, TASK_NAMESPACE and AGENT_NAME to every container's env
	//   - enforces restartPolicy=Never and hostUsers=false
	//   - defaults serviceAccountName to "agent-worker" when empty
	Template corev1.PodTemplateSpec `json:"template"`

	// Access declares which OIDC groups and subjects may manage this Agent and its Tasks.
	// The Agent reconciler creates a namespaced RoleBinding for these principals
	// against the cluster-wide agent-manager ClusterRole.
	Access AgentAccess `json:"access,omitempty"`

	// Model selects the AI backend and its credential secret.
	Model ModelSpec `json:"model"`

	// VolumeClaimTemplates is analogous to StatefulSet's volumeClaimTemplates.
	// The operator stamps one PVC per Task per entry, using metadata.name as the
	// volume name and PVC name suffix. Mount by name from podSpec.containers[*].volumeMounts.
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`

	// GitProviders lists git server credentials available to workers.
	GitProviders []GitProvider `json:"gitProviders,omitempty"`

	// SlackWorkspaces lists Slack workspaces the controller can read/write.
	// Credentials are controller-only (agent-ctrl-* prefix).
	SlackWorkspaces []SlackWorkspace `json:"slackWorkspaces,omitempty"`

	// WebhookSecretRef is the secret (agent-hook-* prefix) for GitHub/Gitea HMAC.
	// Read by the webhook handler, not mounted in worker pods.
	WebhookSecretRef string `json:"webhookSecretRef,omitempty"`

	// CredentialRefs is the exhaustive allowlist of agent-cred-* secrets that
	// worker pods may mount. The operator adds each as a pod volume named cred-<secret>;
	// mount explicitly from podSpec.containers[*].volumeMounts.
	CredentialRefs []string `json:"credentialRefs,omitempty"`

	// MCPEndpoints lists MCP servers available to workers.
	MCPEndpoints []MCPEndpoint `json:"mcpEndpoints,omitempty"`
}

// AgentAccess declares which OIDC principals may manage this Agent and its Tasks.
// The operator creates/updates a RoleBinding in the Agent's namespace to enforce this.
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
	Milestone     string   `json:"milestone"`
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
