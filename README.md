# Codemowers Agent

Always thought that having an agent run commands on your personal laptop was nuts?
Give it a workspace in Kubernetes.

`codemowers/agent` is a GitOps-centric, Kubernetes-native agent harness. A Go
operator runs Claude or Codex in worker Pods, with a persistent workspace per
Task for Git, tests, build artifacts and saved CLI sessions.

An agent running on your laptop stops working when the laptop suspends: it cannot
process GitHub events, email, or queued tasks until the laptop wakes. Run the
harness on an available Kubernetes cluster if you need it to keep working while
your laptop is closed. You can then open its web UI from multiple devices to
answer the agent's questions, send follow-up prompts, and get a stalled task
moving again. An authorized colleague can open the same task and pick up the
conversation when you hand it over.

The goal is to make it reasonably safe to use out of the box through confined
workers, scoped credentials, authenticated inputs and human review of repository
changes. The [resource kinds](#resource-kinds), [current permission
limits](#kubernetes-permissions) and [agent segmentation
example](#agent-segmentation-example) are documented below.

Kubernetes is the execution and deployment platform. Docker and Docker Compose
deployments will not be supported; workers receive no Docker daemon or socket.
Application builds use separate Kaniko Pods through Skaffold. The project is under
active development, with no backward-compatibility commitment at this stage.

Want to try it without setting up a cluster? [Get a sandbox at
trial.codemowers.cloud](https://trial.codemowers.cloud) and deploy the agent there.

## Features

- **Choose your agent.** Each Agent selects Claude or Codex and its provider
  credential. A Task can switch to another model discovered from that provider;
  the workspace is kept.
- **Keep working.** Follow progress in the browser and queue prompts. The worker
  stays warm for the Agent's idle timeout after its last reply. Suspend stops it
  and keeps the workspace, session and queued messages; Resume continues. Stop
  interrupts the current turn without stopping the worker.
- **Delegate from GitHub and Gitea.** Signed webhooks route issue and pull-request
  events to the Task that claims them. A TaskCreationPolicy creates a Task only
  when the item is currently assigned to the Agent's forge account; updates keep
  reaching that Task after unassignment.
- **Connect events.** Alertmanager, Slack and other senders post to
  authenticated webhook receivers. Bounded CEL turns each payload into a routing
  key; unmatched events reach a fallback Task. Bursts within a coalescing window
  wake a Task once more at the end.
- **Mail.** Each Mailbox runs its own Pod with its IMAP and SMTP credentials.
  New mail can wake Tasks through header-matching MailSubscriptions, and workers
  send, list and read mail through that Mailbox's MCP.
- **Remote devices.** Machines running `device-agent` connect over WebSocket.
  Workers run commands and read or write files on them through the Devices MCP.
- **See what a worker can use.** The Task header shows each MCP server's status,
  instructions and tools, and lets you disable one for that Task.

## Why choose codemowers/agent over OpenClaw or Hermes?

Choose `codemowers/agent` for the workflow from GitHub issue to tested code,
reviewed PR and Kubernetes deployment, using your cluster's storage and registry.

[OpenClaw](https://github.com/openclaw/openclaw) centers on an assistant across
messaging channels and devices; [Hermes](https://github.com/NousResearch/hermes-agent)
emphasizes memory, learned skills, and personal automation. Both offer sandboxing
options. Codemowers Agent focuses on persistent task workspaces, GitHub integration
and Kubernetes build/deploy access.

## Why choose codemowers/agent over google/ax?

[Google's AX](https://github.com/google/ax) runs on Agent Substrate.
[Substrate's architecture](https://github.com/agent-substrate/substrate/blob/main/docs/architecture.md)
includes PostgreSQL, sandbox scheduling and memory/filesystem checkpointing, with
gVisor and Kata/Cloud Hypervisor backends.

For GitOps work, a persistent directory usually holds the state that matters:
the checkout, uncommitted changes, artifacts and saved CLI conversation.
Codemowers Agent keeps that state on a PVC and Task state in Kubernetes custom
resources, with RabbitMQ carrying events and prompts. Suspend stops the worker;
Resume starts a new Pod with the same files and session. It requires no
PostgreSQL, process-memory snapshots or additional sandbox scheduler. Failed
worker Pods remain available for `kubectl logs` and status inspection.
Choose it when workspace persistence is enough for your workflow.

## Run on your cluster

**Minimum supported Kubernetes version: 1.36.** This is the baseline for stable
[OCI image volumes](https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/)
used to mount tool bundles such as kubectl. Worker nodes need a container
runtime that supports image volumes and Pod user namespaces. Older Kubernetes
releases and legacy compatibility paths will not be supported.

Kubernetes 1.36 also makes
[MutatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/mutating-admission-policy/)
stable, alongside
[ValidatingAdmissionPolicy](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)
(stable since 1.30). Together they let the platform apply workload defaults and
reject policy violations directly in the API server, without external admission
webhooks.

Use admission policies to enforce runtime, security context and host-access
restrictions on agent Pods and workloads deployed by humans alike. The platform
can require user namespaces or [Kata Containers](https://katacontainers.io/) and
reject bypasses. Kata adds a VM and separate guest kernel; it does not change API
authorization or hide credentials delivered into the guest. Confinement does not
require process-memory snapshots: tasks still resume from persistent workspaces.

The chart expects platform operators for RabbitMQ (an `agent-events` cluster),
OIDC client registration, generated Secrets and certificates. On another
cluster, install compatible operators or adapt those chart resources. Configure
Ingress, DNS and TLS for the operator and webhook hostnames. Workers run with
`hostUsers: false`.

Release images for `linux/amd64` and `linux/arm64` are published to ghcr.io by
[GitHub Actions](.github/workflows/container.yml). For development, install
Skaffold, kubectl and Helm, then populate the gitignored `skaffold.env` in the
project root from [skaffold.env.example](skaffold.env.example):

```dotenv
SKAFFOLD_KUBE_CONTEXT=your-kubeconfig-context
SKAFFOLD_NAMESPACE=your-agent-namespace
SKAFFOLD_DEFAULT_REPO=registry.example.com/your-project
AGENT_OPERATOR_API_RESOURCE=https://your-public-operator-host/api
AGENT_WEBHOOK_PUBLIC_ORIGIN=https://your-public-webhook-host
```

`AGENT_OPERATOR_API_RESOURCE` is the public operator origin followed by `/api`.
OIDC hostname admission expands short hosts in redirect and CORS URLs but not in
`allowedResources`, so a short value makes sign-in fail with `invalid_target`.
Skaffold reads `skaffold.env` itself, builds the images in cluster Kaniko Pods
and does not install CRDs. Apply them first with your administrator context; the
Agent CRD is too large for client-side apply:

```sh
kubectl apply --server-side -f crd/
skaffold dev
```

Then provision the [credentials](#credentials) and apply Agents, RBAC and
optional resources from [examples/](examples/README.md). The UI shows Task and
Agent status, copyable kubectl commands, webhook setup and Device connection
commands.

## Resource kinds

All kinds are in the `codemowers.cloud/v1alpha1` API group. Namespace
administrators are humans; no automation receives RBAC to create or change
Agents. Workers never read or write these resources through the Kubernetes API.

| Kind | Scope | Administered by | Read by | Mutated by |
| --- | --- | --- | --- | --- |
| Agent | Namespaced | Namespace administrators | Operator; UI users with Agent `get` | Administrators (spec); operator (status only) |
| Task | Namespaced | Namespace administrators; the operator creates event and UI Tasks | Operator; UI users with `get` on its Agent | Operator (derived spec fields and status, on behalf of UI users, events and the worker's Agent MCP calls); administrators (spec) |
| WebhookReceiver | Namespaced | Namespace administrators | Webhook receiver | Administrators |
| ClusterWebhookReceiver | Cluster | Cluster administrators | Webhook receiver | Cluster administrators |
| TaskCreationPolicy | Namespaced | Namespace administrators | Operator | Administrators |
| Mailbox | Namespaced | Namespace administrators | Operator | Administrators (spec); operator (status: IMAP cursor) |
| MailSubscription | Namespaced | Namespace administrators | Operator | Administrators |
| Device | Namespaced | Namespace administrators | Device bridge | Administrators (spec); Device bridge (status: connection) |

- **Agent** defines a worker: CLI and model, Pod template, workspace volume
  claims, Git providers, idle timeout and extra MCP servers.
- **Task** is one objective with its own PVC, conversation, routing keys and
  external references. UI actions such as prompts, Suspend and MCP toggles go
  through the operator, which checks the user's Agent access first.
- **ClusterWebhookReceiver** defines a sender protocol: signature verification,
  CEL extraction and the HTTP response. A **WebhookReceiver** binds one protocol
  to one Agent with its routing and fallback Task.
- **TaskCreationPolicy** decides when an unmatched event may create a Task.
- **Mailbox** runs one Pod holding that mailbox's IMAP and SMTP credentials.
  A **MailSubscription** wakes one Agent's Tasks for matching mail headers.
- **Device** registers a remote machine and its connection key.

Worker access to a Mailbox or Device is granted with custom RBAC verbs on that
object, checked by its MCP server on every call:

| Kind | Verb | Allows |
| --- | --- | --- |
| Mailbox | `send` | `send_email` |
| Mailbox | `read` | Listing folders and messages, reading messages |
| Device | `exec` | `device_shell` |
| Device | `read` | `device_file_read` |
| Device | `write` | `device_file_write` |

Standard verbs such as `get` grant none of these. A Device's `write` usually
allows code execution as well, by overwriting scripts or service files.

## MCP configuration

Each Agent lists its extra MCP servers in `spec.mcpEndpoints`, such as the
Codemowers advisory and the cluster's Driftmower. The operator adds three kinds:

- `agent_operator` for the worker's own Task: title, progress, results and
  same-Agent Task management.
- `devices` for remote Devices.
- `mailbox-<name>` for each Mailbox the worker's ServiceAccount holds `send` or
  `read` on, attached when the worker Pod starts.

Workers authenticate to these with projected, audience-bound ServiceAccount
tokens; no personal OAuth token reaches a worker. An endpoint with
`tokenFile: /var/run/agent-mcp/driftmower` receives a projected token for
Driftmower's in-cluster audience.

## Kubernetes permissions

| Identity | Permissions |
| --- | --- |
| `agent-operator` | Reads Agents, Mailboxes and policies; manages Tasks, worker Pods, PVCs and Mailbox Services; writes Agent, Task and Mailbox status; reads `agent-operator-provider-tokens` by name; creates TokenReviews and SubjectAccessReviews. |
| `agent-operator-webhook` | Reads receivers and `agent-operator-webhook-keys` by name. |
| `agent-devices` | Reads Devices and `agent-device-keys`; writes Device status; creates TokenReviews and SubjectAccessReviews. |
| `agent-mailbox` | Creates TokenReviews and SubjectAccessReviews. Only the Mailbox Pod's MCP container receives its token. |
| `agent-worker-<agent>` | Whatever the namespace administrator grants; see [examples/](examples/README.md) for the three profiles below. |

Review the chart's RBAC before granting worker access. Browser access is authorized separately, by
SubjectAccessReview of the signed-in user's Agent `get` permission.

### Namespace layout

Agents can each have their own namespace, share a namespace in groups, or all
live in the harness namespace. Which is safe depends on the cluster's admission
policies.

- **A namespace per Agent** gives each Agent its own Secrets, reads and network
  boundary with ordinary RBAC. It needs more namespaces, Secrets and
  per-namespace setup.
- **Several Agents per namespace**, grouped by trust level, shares that setup
  between Agents that may see each other's workloads.
- **All Agents in one namespace** is the simplest to run. It relies on admission
  policies to keep each Agent's writes, Pods and Secret references to its own
  resources, and every worker can still read the namespace's Pods, logs and
  ConfigMaps.

### Current limits

Assume external credentials are already confined: for example, a dedicated GitHub
account whose permissions and branch protection forbid pushing to `main`/`master`.
Token theft still permits abuse within those external grants.

When Agents share a namespace with the harness, an admission policy such as
`agent-worker-prefix` must confine each `agent-worker-<agent>` ServiceAccount to
resources named `<agent>-*`, including the ServiceAccounts, Secrets and PVCs its
Pods and workload templates reference. Known gaps remain:

- **Workers can manage broker topology.** Developer RBAC allows `rabbitmq.com`
  resources, so a worker sharing a namespace with the broker can declare a
  topology `User` or `Permission` against it. Deny that in admission or keep
  workers out of the broker's namespace.
- **Labels and selectors are not confined.** A worker can label its own Pods to
  join another Service, or point its NetworkPolicy, Ingress or autoscaler at
  harness workloads.
- **Reads are namespace-wide.** RBAC decides reads, which admission does not
  see. A developer worker can read every Pod spec, Pod log and ConfigMap in its
  namespace.
- **Agents may share Git credentials.** Separate review rights require a
  separate forge identity per Agent.
- **Namespace scope does not limit use of delivered credentials.** Malicious
  code can copy model keys, forge tokens or the registry push credential and use
  them elsewhere within their existing grants.

### Credential delivery

| Credential | Operator | Worker |
| --- | --- | --- |
| Model API keys | Reads the selected key to discover models | The Agent's selected key |
| Git provider tokens | Reads them to probe repositories and assignment | Each provider key the Agent names |
| Kaniko push credential | Not used | Mounted into Kaniko builder Pods by Skaffold |
| Webhook signing keys | Not injected; the webhook receiver reads them | Not injected |
| RabbitMQ credential | Operator connection | None; projected token for audience `agent-rabbitmq` |
| Mailbox IMAP/SMTP | Not held | Never; mail goes through the Mailbox MCP |
| Device keys | Not held; the Device bridge reads them | Never; commands go through the Devices MCP |
| Kubernetes API token | Operator identity | Worker identity, when the template mounts it |
| MCP tokens | Verifies Agent MCP tokens | Projected, audience-bound tokens per MCP |
| OIDC | User access tokens on API requests | Not injected |

## Agent segmentation example

This illustrative layout separates three agents by trust level, in any of the
[namespace layouts](#namespace-layout) above. [examples/](examples/README.md)
implements it:

| Boundary | Kube admin | Developer | Customer service |
| --- | --- | --- | --- |
| Worker Kubernetes authority | Cluster-admin | Cluster-wide operational reads; build/deploy in own namespace | No ServiceAccount token or API permissions |
| GitHub authority | Read reviewed `main`/`master` | Pull code, push topic branches, manage issues, open PRs | Issues in selected support repositories |
| Model | Local model (planned) | Claude / Codex | Cheaper model via Claude / Codex |
| Human interaction | DevOps-only web UI through OIDC groups | GitHub issues and PR feedback | Email, GitHub issues, Slack |
| Machine-to-machine | Critical Alertmanager alerts | GitHub events, development OCI registry, critical/warning alerts | GitHub issue events, external-system webhooks |
| Primary risk | Sensitive-data disclosure | Supply-chain attack and privilege escalation | Prompt injection |

The developer prepares changes on a topic branch. GitHub rules require human PR
review and reserve default-branch updates/merge for humans; agents have no bypass
rights. The admin reads the resulting reviewed branch with read-only GitHub
credentials. GitHub contents-read tokens do not restrict individual branches;
enforcing `main`/`master`-only reads requires a branch-only mirror rather than a
checkout instruction. GitHub review rules are independent of Kubernetes RBAC.

**Kube admin has the highest sensitive-data disclosure risk.** Cluster-admin can
read every namespace's Secrets, workload configuration and logs, and change their
permissions. Trust the admin harness accordingly. A local model keeps prompts
away from a hosted provider, but Kata and local execution cannot prevent leaks
through other channels. Human PR review does not gate arbitrary admin API writes.

**The developer has the highest supply-chain and privilege-escalation exposure
in this layout.** It executes repository code, dependencies and build scripts,
starts Kaniko Pods through Skaffold, pushes development images and deploys them.
Cluster-wide reads exclude Secrets; workload writes stay in its namespace and,
when sharing a namespace, under its name prefix. Admission policies must enforce
`hostUsers: false` or a supported [Kata Containers](https://katacontainers.io/)
runtime for worker, build and development Pods, and reject attempts to bypass
confinement through privileged containers or host access. Confinement limits
host compromise; permitted ServiceAccounts, Secret references and development
registry ACLs must also be restricted to contain credential theft, image
poisoning and API escalation.

**Customer support has the highest prompt-injection exposure in this layout.**
Email, tickets, webhook payloads and Slack messages carry externally supplied
text. Authenticating the source does not make that text trusted instructions.
This is why the support worker gets no workload API authority, source-code
writes or administrative credentials. Policy must constrain recipients, actions
and delegation; support-created developer issues remain untrusted input, and do
not authorize kube-admin actions. Isolation limits the impact of a fooled model,
but cannot prevent misuse of its permitted email/issue actions or disclosure of
customer data it can read.

Mailbox Pods and the webhook receiver hold the mail and webhook credentials,
authenticate each integration's events and route them to Tasks. Payloads cannot
select namespaces or grant permissions. The support worker gets issue and model
credentials and its Agent MCP token, with automatic ServiceAccount token
mounting disabled and no Driftmower token. It has no private-source, code-push
or PR-review access. The developer uses separate GitHub credentials and
receives no mail credentials.

Namespace separation prevents developer/support workers from mounting another
namespace's Secrets; it cannot constrain the trusted cluster-admin agent. Each worker's broker
token reads only its own ServiceAccount's queues and cannot publish.

## Credentials

Keep credential values out of `skaffold.env`, manifests, images, chat and shell
history. Each namespace holds fixed Secrets whose individual keys are selected by
reference:

| Secret | Holds | Selected by |
| --- | --- | --- |
| `agent-operator-provider-tokens` | Model API keys and Git provider tokens | `credentialKeyRef` in Agent model and Git provider entries |
| `agent-operator-webhook-keys` | Webhook signing keys | `secretKeyRef` in WebhookReceivers |
| `agent-device-keys` | Device connection keys | `connectionSecretRef` in Devices |

Mailbox credentials stay in Secrets named by each Mailbox's
`credentialsSecretRef`. A worker whose selected model key is missing records a
failure and exits; it never switches providers. Without a Git provider token,
prompt-only Tasks still run, but repository reads and assignment checks are
unavailable. A webhook signing key is not a Git API token; issue the API token
in the forge.

## Workspace and build workflow

Workers use Ubuntu 26.04 with Node.js, Git, ripgrep, Python with venv, build
tools, gh, tea, Helm and Skaffold, plus the Claude Code and Codex CLIs pinned in
[runner/package.json](runner/package.json). Downloaded tools are verified
against [runner/checksums.sha256](runner/checksums.sha256). Agents mount kubectl
from its upstream image as an image volume. Install Python packages into a
virtual environment in the workspace.

Each Task's workspace starts as a Git repository with a task specification under
`specs/` and workspace instructions. Its PVCs come from the Agent's
`volumeClaimTemplates`; an Agent without them runs stateless Tasks. New Task can
start from an editable product draft. Use Skaffold's cluster builder for Kaniko
builds and deployments; workers use their mounted kubeconfig and
`SKAFFOLD_KUBE_CONTEXT`.

See [AGENTS.md](AGENTS.md) for architecture, routing, operations and
development guidance.

## Roadmap

- **Per-Task broker credentials:** confine each worker to its own RabbitMQ
  queues.
- **Slack chat:** link threads to existing Tasks and forward their first
  message, starting with the customer-support profile.
- **Gitea:** validate routing end to end and add richer repository-creation
  discovery. Preserve human PR review and instance-scoped credentials.
- **Mail:** search, and binding outgoing messages to their Task so replies
  return to it.
- **Local llama.cpp:** add local-model execution with reliable tool calls,
  streaming and session resumption. The planned kube-admin local-model profile
  depends on this work; local-model execution is not implemented.

## Contributing

Contributions are welcome: bug reports, documentation improvements, integrations,
and pull requests. Target the supported Kubernetes baseline, include relevant
validation, and update [AGENTS.md](AGENTS.md) when changing
behavior or architecture.
