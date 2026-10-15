# Codemowers Agent

Always thought that having an agent run commands on your personal laptop was nuts?
Give it a workspace in Kubernetes.

`codemowers/agent` is a GitOps-centric, Kubernetes-native agent harness. It runs
Claude or Codex in worker Pods, with a persistent workspace per task for Git,
tests, build artifacts and saved CLI sessions.

An agent running on your laptop stops working when the laptop suspends: it cannot
process GitHub events, email, or queued tasks until the laptop wakes. Run the
harness on an available Kubernetes cluster if you need it to keep working while
your laptop is closed. You can then open its web UI from multiple devices to
answer the agent's questions, send follow-up prompts, and get a stalled task
moving again. An authorized colleague can open the same task and pick up the
conversation when you hand it over.

The goal is to make it reasonably safe to use out of the box through confined
workers, scoped credentials, authenticated inputs and human review of repository
changes. The [current permission limits](#kubernetes-permissions) and
[proposed agent boundaries](#agent-segmentation-example) are documented below.

Kubernetes is the execution and deployment platform. Docker and Docker Compose
deployments will not be supported; workers receive no Docker daemon or socket.
Application builds use separate Kaniko Pods through Skaffold. The project is under
active development, with no backward-compatibility commitment at this stage.

Want to try it without setting up a cluster? [Get a sandbox at
trial.codemowers.cloud](https://trial.codemowers.cloud) and deploy the agent there.
Note we will shortly add knob there to turn on the agent just like that.

## Features

- **Choose your agent.** Select hosted Claude or Codex and a model per task.
- **Keep working.** Follow progress in the browser, queue prompts, and pause or
  resume with the workspace intact. Waiting for confirmation keeps the worker
  warm for 60 minutes after its last response; explicit Pause stops it.
- **Delegate from GitHub.** Assign an issue or PR to the configured agent user to start
  work. Updates reuse the task even after unassignment; closing an issue completes
  it. Startup and periodic assignment discovery recover missed webhooks. GitHub
  notification email can discover assignments in repositories without a webhook.
- **Connect events.** Configure GitHub, Alertmanager, and authenticated webhooks
  in **Connections**.
- **Handle notifications promptly.** The first automatic wakeup starts immediately.
  Follow-up automatic notifications within three minutes do not start another turn,
  including a notification email for the same Git event. UI prompts and participant
  email replies also run immediately.
- **Follow linked pull requests.** PR merges wake the original task, including
  across repositories.
- **Check connectivity.** After sign-in, badges show startup checks for Codemowers,
  Driftmower and Agent MCPs, GitHub, mail and the artifact registry, including a
  real image push. Tooltips show results and check times. Mail activity updates
  mail status; badges move into the mobile menu.
- **Keep email threads connected.** Send authorized task email through Agent MCP;
  IMAP replies wake the same task. Thread mappings survive restarts, delivery
  status stays visible, and Sent-folder archiving is optional. For issue-backed
  tasks, the GitHub issue is the main thread for results, blockers and confirmation
  questions; mail and chat are secondary.
- **Report harness failures.** Controller and worker exceptions create attributed
  repair tasks for `codemowers/agent`. One active repair Task per component collects
  keyed exception fingerprints; a new fingerprint queues a follow-up, while repeats
  only update the occurrence count. Fixes follow normal review and deployment.

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
Codemowers Agent keeps that state on a PVC and task/event state in Redis. Pause
stops the worker; resume starts a new Pod with the same files and session. It
requires no PostgreSQL, process-memory snapshots or additional sandbox scheduler.
Choose it when workspace persistence is enough for your workflow.

## Run on your cluster

**Minimum supported Kubernetes version: 1.36.** This is the baseline for stable
[OCI image volumes](https://kubernetes.io/docs/tasks/configure-pod-container/image-volumes/)
used to mount tool bundles. Worker nodes need a container runtime that supports
image volumes and Pod user namespaces. Older Kubernetes releases and legacy
compatibility paths will not be supported.

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

For source builds with the bundled development configuration, install Skaffold,
kubectl, Helm, and Docker, and configure your kubeconfig and registry push
credentials. Docker builds the harness images locally during development; it is
not an agent runtime or deployment target. Create a namespace for the agent and
its workloads.
Populate the local `skaffold.env` in the project root:

```dotenv
SKAFFOLD_KUBE_CONTEXT=your-kubeconfig-context
SKAFFOLD_NAMESPACE=your-agent-namespace
AGENT_KUBE_CONTEXT=your-cluster-name/your-agent-namespace
AGENT_API_RESOURCE=https://your-public-controller-host/api
SKAFFOLD_DEFAULT_REPO=registry.example.com/your-project
SKAFFOLD_CLEANUP=false
```

Skaffold reads this file automatically; it is gitignored. `SKAFFOLD_CLEANUP=false`
keeps deployed resources when you stop the development loop.

Helm values configure `host` (default `controller`), `allowedGroups`,
`allowedUsers` and the required `apiResource`. The controller reads the generated
OIDC `allowedUsers` list as its subject allowlist, so provider and API checks
agree. Set `AGENT_API_RESOURCE` to the public controller origin
followed by `/api`, matching the OIDC owner's `OIDC_CLIENT_ORIGIN` plus `/api`.
OIDC hostname admission expands the short `host` in redirect and CORS URLs, but
does not expand `allowedResources`; a short API resource makes sign-in fail with
`invalid_target`. Skaffold injects images and runtime settings; deployment
defaults live in the chart. The
deployment namespace comes from local configuration and is supplied through the
Pod namespace fieldRef. `AGENT_KUBE_CONTEXT` names the prepared controller/worker
context; it uses the in-cluster API and ServiceAccount authentication rather than
the human credentials selected by `SKAFFOLD_KUBE_CONTEXT`.

The `dev-test` Skaffold profile activates for the `dev-test` kube context. It
allows only the `laurivosandi` account and sets the verified public API resource.

The application needs Redis, persistent volumes, and an OIDC public client using
Authorization Code with PKCE. The bundled [chart](chart/) integrates with
platform operators for Redis provisioning, OIDC registration, Secret generation,
and certificates. On another cluster, install compatible operators or adapt those
chart resources to your existing services. Configure Ingress, DNS, and TLS for
your hostname. Workers currently use `hostUsers: false`, so the cluster runtime
must support Pod user namespaces.
The browser stores its OIDC session in localStorage and renews access tokens with
Passmower refresh tokens; signing out clears the stored session.

Provision the [credentials](#provider-credentials), then start the build, deploy
and log-streaming loop:

```sh
skaffold dev
```

## MCP configuration

External MCPs come from the optional `agent-mcp` ConfigMap, managed separately from
Helm. Create a local `mcp.json` with an `mcpServers` map using the `codemowers` and
`driftmower` names. Each HTTP entry supplies `type: "http"` and `url`; optional
`siteURL` sets its browser link. The local file is excluded from Git and images.

```sh
kubectl create configmap agent-mcp --from-file=mcp.json
```

The controller and workers read it at `/mcp/mcp.json`. A missing ConfigMap disables
external MCPs; built-in Agent MCP remains available. Driftmower uses
`http://driftmower-mcp.driftmower.svc.cluster.local:3000/mcp` in-cluster, with
`tokenFile: "/var/run/agent-mcp/driftmower"`. Controller and worker Pods always
project their own rotating ServiceAccount token for that internal audience.
They require no token-creation RBAC. Browser OAuth retains Driftmower's public
audience. Restart the controller and workers after changing MCP configuration.

## Kubernetes permissions

The chart creates two ServiceAccounts and binds each to its own **Role in the
release namespace**. For the default release name `agent`:

| Identity | Permissions |
| --- | --- |
| `agent-controller` | Manages worker Pods, logs, task PVCs, and ConfigMaps; uses Leases for leader election. |
| `agent-worker` | Builds and deploys namespace workloads: Pods, Services, PVCs, ConfigMaps, Deployments, StatefulSets, Jobs, CronJobs, and Ingresses. Can exec/attach to Pods and manage the namespaced operator resources listed in the chart. |

Both Roles can read `zot-registry-cluster-push-kaniko`, but grant no general Secret
API reads, RBAC management or cluster-admin binding. Workers use their own
ServiceAccount tokens for kubectl/Skaffold. Review [the Role rules](chart/templates/resources.yaml)
for your workloads; OIDC group/subject allowlists authorize browser access separately.

### Current limits

Assume external credentials are already confined: for example, a dedicated GitHub
account whose permissions and branch protection forbid pushing to `main`/`master`.
Token theft still permits abuse within those external grants. The main Kubernetes
concern is worker authority over the namespace containing its controller. Current
workers use `hostUsers: false`; enforcing Kata would leave these API access paths
unchanged:

- **Secret-read denial does not prevent Secret use.** A worker can create a Pod
  that mounts a namespace Secret or injects it through environment references.
  The inspected admission policies do not restrict those names, making controller
  credentials indirectly accessible. Kubernetes documents this implication of
  [workload creation permissions](https://kubernetes.io/docs/concepts/security/rbac-good-practices/#workload-creation).
- **ServiceAccount selection is unrestricted.** A worker can request another
  ServiceAccount in the same namespace and obtain its mounted token. The separate
  controller Role needs admission checks on identity selection and token
  projections to provide a meaningful boundary.
- **The controller's workloads are writable.** Worker permissions cover the
  controller Deployment, all namespace Pods, and exec/attach. They also cover its
  Dragonfly and StringSecret resources. A worker could replace its controller,
  read its environment through exec, disrupt Redis or alter credential generation
  without escaping its container.
- **Tasks share a trust domain.** All workers use the same ServiceAccount. They
  can read other tasks' input ConfigMaps, access their Pods and request mounts of
  their PVCs, subject to storage attachment constraints. `AGENT_REPORT_TOKEN` is
  a literal Pod environment value, exposing active execution tokens to Pod
  readers. Separate checkouts and sessions do not enforce tenant isolation.
- **Namespace scope does not limit use of delivered credentials.** Malicious
  code can copy GitHub/model keys or the registry credential and use them elsewhere
  within their existing grants. Kaniko commands can read mounted build credentials;
  instructions, registry prefixes and Kata do not reduce external permissions.

Treat the current installation as one trusted sandbox. Tenant isolation requires
separate worker namespaces and credentials, with controller/state-store credentials
outside those namespaces. Within one namespace, constrain ServiceAccount/Secret
usage and cross-worker exec/workload modification. Namespace compromise alone
does not establish cluster-wide administrative access.

### Credential delivery

The table below describes **normal credential delivery**, not the indirect access
enabled by those workload permissions. Secret references expose names and keys,
not values, to the controller when it prepares a worker Pod.

| Credential | Controller | Worker |
| --- | --- | --- |
| GitHub token | `GH_TOKEN` | Same `GH_TOKEN` |
| Anthropic / OpenAI keys | Secret references only | Selected hosted provider's key |
| Zot push credential | Reads value for discovery/push probe | Can read value; Skaffold mounts it into Kaniko |
| IMAP | Inbound credentials, when configured | Task mail through Agent MCP; no mailbox credentials |
| SMTP | Outbound credentials, when configured | Task mail through Agent MCP; no mailbox credentials |
| Redis password | `REDIS_PASSWORD` | Not injected |
| Generic webhook token | `WEBHOOK_TOKEN` | Not injected |
| Alertmanager token | `ALERTMANAGER_TOKEN` | Not injected |
| GitHub webhook signing secret | `GITHUB_WEBHOOK_SECRET` | Not injected |
| Transcript-sharing signing key | Explicit key or derived from `WEBHOOK_TOKEN` | Not injected |
| Kubernetes API ServiceAccount token | Controller identity | Worker identity |
| Driftmower MCP token | Projected controller token with internal audience | Projected worker token with internal audience |
| Agent MCP / execution token | Creates and stores tokens for active executions | Receives its own `AGENT_REPORT_TOKEN` |
| OIDC | Public PKCE configuration; user access tokens on authenticated requests | Not injected |

The [agent segmentation example](#agent-segmentation-example) below illustrates
how different trust levels can be separated.

## Agent segmentation example

This illustrative layout separates three agents into three namespaces, each with
its own controller, worker identity, state store and credentials. These profiles
describe a proposed configuration rather than the current chart defaults:

| Boundary | Kube admin | Developer | Customer service |
| --- | --- | --- | --- |
| Namespace | `agent-kube-admin` | `agent-developer` | `agent-customer-service` |
| Worker Kubernetes authority | Cluster-admin | Cluster-wide operational reads; build/deploy in own namespace | No ServiceAccount token or API permissions |
| GitHub authority | Read reviewed `main`/`master` | Pull code, push topic branches, manage issues, open PRs | Issues in selected support repositories |
| Model | Local model (planned) | Claude / Codex | Cheaper model via Claude / Codex |
| Human interaction | DevOps-only web UI through OIDC groups | GitHub issues and PR feedback | Email, GitHub issues; Slack/chat planned |
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
starts Kaniko Pods through Skaffold, pushes development images and deploys them
in `agent-developer`. Cluster-wide reads exclude Secrets; workload writes stay in
its namespace. Admission policies must enforce `hostUsers: false` or a supported
[Kata Containers](https://katacontainers.io/) runtime for worker, build and
development Pods, and reject attempts to bypass confinement through privileged
containers or host access. Confinement limits host compromise; permitted
ServiceAccounts, Secret references and development registry ACLs must also be
restricted to contain credential theft, image poisoning and API escalation.

**Customer support has the highest prompt-injection exposure in this layout.**
Email, tickets, webhook payloads and future Slack/chat messages carry externally
supplied text. Authenticating the source does not make that text trusted
instructions. This is why the support worker gets no workload API authority,
source-code writes or administrative credentials. Later Slack and other chat
connectors belong at this boundary, with source/workspace/channel/thread
bindings to support tasks. Controller policy must constrain recipients, actions
and delegation; support-created developer issues remain untrusted input, and do
not authorize kube-admin actions. Isolation limits the impact of a fooled model,
but cannot prevent misuse of its permitted email/issue actions or disclosure of
customer data it can read.

The support controller holds mailbox and webhook credentials, authenticates and
deduplicates each integration's events, and routes them to support tasks. Payloads
cannot select namespaces or grant permissions. Its worker gets issue/model
credentials and an execution token for Agent MCP, with automatic ServiceAccount
token mounting disabled and no explicit projections, including Driftmower tokens.
It has no private-source, code-push or PR-review access. Developer uses separate
GitHub credentials and receives no mail credentials.

Namespace separation prevents developer/support workers from mounting another
namespace's Secrets; it cannot constrain the trusted cluster-admin agent.

Cluster-wide reads need care too: the current literal Pod execution tokens and
potentially sensitive logs remain visible to a Pod/log reader. Moving tokens out
of Pod specifications and narrowing confidential diagnostic access are necessary
before claiming isolation from customer-service task data. Implementing these
profiles also requires the chart's broad worker grants, mandatory mail Secret
wiring and private-repository cloning to become profile-specific.

## Provider credentials

Create the selected model-provider credentials using your current kubectl context.
Worker model Secret references are optional at Pod creation. The worker exits
with a recorded failure if its selected Claude or Codex API key is unavailable;
it does not switch providers. Provision the matching Secret before retrying.
Create the Git provider token when Git integration is needed. Without it, prompt-only
Tasks still run, while Git discovery, issue/PR routing and authenticated repository
operations are unavailable. The controller and workers can start without
`agent-worker-github-token`.

```sh
kubectl create secret generic agent-worker-github-token \
  --from-literal=token=...
kubectl create secret generic agent-worker-anthropic-api-token \
  --from-literal=token=...
kubectl create secret generic agent-worker-openai-api-token \
  --from-literal=token=...
```

The chart declares a separate `StringSecret` for each controller credential. The
platform's secret-generator operator creates the corresponding Kubernetes Secrets
with random values:

| Secret | Key | Controller environment |
| --- | --- | --- |
| `agent-controller-redis-password` | `password` | `REDIS_PASSWORD` |
| `agent-controller-webhook-token` | `token` | `WEBHOOK_TOKEN` |
| `agent-controller-alertmanager-token` | `token` | `ALERTMANAGER_TOKEN` |
| `agent-controller-github-webhook-secret` | `secret` | `GITHUB_WEBHOOK_SECRET` |

Redis and the controller reference the same password Secret. Provision GitHub,
model-provider and mailbox credentials separately. Keep values out of
`skaffold.env`, manifests and images.

## Agent identity and email

Agent identity comes from an optional `agent-identity` ConfigMap. Without it, the
agent is **Agent Smith**, username `agent-smith`, email `agent.smith@system.gov`.
Set `AGENT_USERNAME` to the GitHub bot account and configure its display/Git name
and email. The controller passes this identity to workers and the UI.

The same ConfigMap can select a Git server with `AGENT_FORGE_PROVIDER` (`github`
or `gitea`) and `AGENT_FORGE_URL`. GitHub Enterprise defaults to the server's
`/api/v3`; Gitea defaults to `/api/v1`. Set `AGENT_FORGE_API_URL` only when a
different API endpoint is needed. When configured, the Git token Secret must contain
a token for that server. Public GitHub remains the default. Gitea routing has fixture
coverage; live Gitea validation and notification-email parsing remain future work.

Issue and PR keys include provider, server, repository, number and kind. Webhooks
and notification mail reuse the same task. Assignment gates creation; existing
tasks keep receiving updates after unassignment, with no automatic deletion or
tracking of which comments the model has read. Self-authored comments are ignored
to prevent loops. Pause keeps updates queued; Cancel prevents reactivation.
Assignment discovery runs at startup and every five minutes. Deleting a task for
an open, still-assigned item can recreate it on the next scan; close/unassign the
item or cancel its task to stop processing.

```sh
kubectl create configmap agent-identity \
  --from-literal=AGENT_USERNAME=agent-smith \
  --from-literal=AGENT_NAME='Agent Smith' \
  --from-literal=AGENT_EMAIL=agent.smith@system.gov
kubectl create configmap agent-controller-mail \
  --from-literal=IMAP_HOST=imap.example.org \
  --from-literal=SMTP_HOST=smtp.example.org
kubectl create secret generic agent-controller-imap-credentials \
  --from-literal=IMAP_USERNAME=... \
  --from-literal=IMAP_PASSWORD=...
kubectl create secret generic agent-controller-smtp-credentials \
  --from-literal=SMTP_USERNAME=... \
  --from-literal=SMTP_PASSWORD=...
```

Each hostname configures its protocol independently, without an enable switch.
A missing hostname disables that connector; missing credentials with a configured
hostname show an incomplete configuration. All ConfigMap and mail Secret references
are optional. Username defaults to `AGENT_USERNAME`; SMTP From defaults to
`AGENT_EMAIL`. Override them with the corresponding protocol's username or
`SMTP_FROM` / `SMTP_REPLY_TO`. Workers receive no mail credentials.

IMAP defaults to TLS/993 and `IMAP_MAILBOX=INBOX`; SMTP defaults to TLS/465.
`IMAP_PORT` / `SMTP_PORT` and `IMAP_TLS` / `SMTP_TLS` override transport settings.
`IMAP_ALLOWED_SENDERS` extends the allowed reply participants.
`SMTP_SAVE_SENT=true` enables Sent-folder copies through configured IMAP;
`IMAP_SENT_MAILBOX` overrides automatic Sent-folder discovery. Archive retries do
not resend SMTP. Restart the controller after changing ConfigMap/Secret environment
settings; new workers receive its resolved identity. See [AGENTS.md](AGENTS.md) for
thread routing, GitHub notification recovery and delivery guarantees.

## Workspace and build workflow

The controller uses Node.js and Redis. Workers use Ubuntu 24.04 with Node.js,
Git, ripgrep and both pinned npm-installed Claude and Codex CLIs. Skaffold comes
from its release binary. Both Pod types mount kubectl from an upstream OCI image
volume and prepend its `bin` directory to PATH. Helm selects the controller version
from `.Capabilities.KubeVersion.Version`; Skaffold supplies 1.36.3 for rendering.
The controller reads the live API server version before spawning each worker and
selects a matching kubectl image for it.
Workers also mount a pinned Go toolchain image at `/opt/tools/go`. Go build caches
and module downloads stay on the Task PVC. Python 3 includes pip and venv support;
install project packages into a virtual environment under `/workspace`.
Use Skaffold's cluster builder for Kaniko builds and deployments.

New Task opens an inline wizard: choose Start from scratch or an editable draft,
then describe the work. Drafts are packaged into `agent-drafts`, mounted in the
controller at `/drafts` and served before login. Every draft exposes separate
Test, Build and Deploy toggles; these guide worker behavior, rather than enforce
Kubernetes permissions.

Each workspace starts as a Git repository and receives a specification under
`specs/`, an `AGENTS.md` reference and Skaffold defaults. `skaffold.env.example`
documents them; populated `skaffold.env` is gitignored. User edits are preserved.
Git uses `AGENT_NAME`/`AGENT_EMAIL` and `pull.rebase=true`.

The configured USD cap applies to Claude; use provider billing limits for Codex.
Set `env.AGENT_MCP_PAYLOADS_ENABLED: "true"` in Helm values to enable the
authenticated MCP request/response JSON viewer. It is off by default. The worker
records bounded, redacted payloads in Task history, and the UI fetches them only
when a tool call is expanded. Shared Task links never include these payloads.
Restart the controller and workers after changing this setting; existing calls
made while it was off have no payloads to display. Payloads can still contain
sensitive task data, so enable this only where Task API readers may see it.
Task files and CLI sessions persist on PVCs; configure Redis persistence for task
and event recovery.

Worker builds use the namespace push Secret's registry and agent username as a
repository prefix. Set `AGENT_REGISTRY_PREFIX` for another path, or empty for a
per-user registry. Registry ACLs must enforce scope; the prefix is only a default.
Workers use mounted `KUBECONFIG` and `SKAFFOLD_KUBE_CONTEXT`; do not duplicate
context selection in `skaffold.yaml`.

See [AGENTS.md](AGENTS.md) for architecture, operations, development guidance,
sharing, drafts and future operator design.

## Roadmap

- **Slack chat:** route conversations and follow-up messages to persistent tasks,
  with authenticated events and workspace/channel/thread identity. Start with the
  customer-support profile and its restricted permissions.
- **Gitea:** validate the configured-server routing end to end and add notification
  email parsing and richer repository-creation discovery. Preserve human PR
  review and instance-scoped credentials.
- **Local llama.cpp:** add local-model execution with reliable tool calls,
  streaming and session resumption. The planned kube-admin local-model profile
  depends on this work; local-model execution is not implemented.

## Contributing

Contributions are welcome: bug reports, documentation improvements, integrations,
and pull requests. Target the supported Kubernetes baseline, include relevant
validation, and update [AGENTS.md](AGENTS.md) when changing
behavior or architecture.
