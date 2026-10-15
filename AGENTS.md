# codemowers/agent: GitOps agent harness

This file is the consolidated implementation contract and repository guidance.
Keep it updated when behavior or architecture changes. README.md covers user
setup and operation; the future operator design below is not implemented.

The project goal is a GitOps-centric, Kubernetes-native agent harness that is
reasonably safe to use out of the box through confinement, scoped credentials,
authenticated inputs and human review. Kubernetes is the execution/deployment
target; Docker/Compose runtime deployments are outside scope. Local Docker is
used only to build this repository's harness images during development. Workers
build application images in separate Kaniko Pods through Skaffold.

In the README segmentation example, customer support has the greatest external
prompt-injection exposure, developer has the greatest supply-chain and
privilege-escalation exposure, and kube admin has the greatest sensitive-data
disclosure risk due to cluster-wide access. Developer builds with Kaniko and
deploys development workloads only in its own namespace, alongside cluster-wide
operational reads. Platform admission must enforce user namespace isolation
(`hostUsers: false`) or a supported Kata runtime for those Pods and reject host
access/confinement bypasses. Kata and local models do not remove API privileges
or prevent abuse of credentials and data available to an agent.

## Working in this repository

- Backward compatibility is not a goal at this stage. Make breaking changes
  directly; do not add migration paths or existing-installation instructions.
- Use single quotes for JavaScript strings and template literals for interpolation.
  Quotes embedded in JSON, selectors and CLI configuration are data.
- Build this harness with local Docker through Skaffold and the configured Zot
  repository. Worker application builds use the separate sandbox workflow below.
- Keep tooling and dependency Docker stages independent of source/frontend stages.
  Pin dependencies and coding CLIs; production images receive pruned dependencies.
  Exclude credentials, local configuration, generated output and editor files from
  Git and image build context.
- Keep CSS organized by component, shared defaults and one block per responsive
  breakpoint. Remove superseded rules and unused selectors. Preserve desktop,
  mobile drawer, composer, dialog and read-only layouts.
- Run focused verification appropriate to the change. Do not add redundant tests
  or repeat broad checks after relevant checks pass without a new concern.
- Product drafts describe requirements, application design and acceptance criteria.
  Platform recipes and cluster findings belong in live MCP guidance and shared
  worker instructions. Preserve product-specific ownership, storage and recovery
  requirements when trimming drafts. Every product draft declares separate Test,
  Build and Deploy controls in agentControls front matter, with explicit off-state
  behavior; do not combine image builds and deployments into one control.
- Do not reintroduce obsolete namespace, secret, provider or UI compatibility
  paths. Preserve existing PVCs and their contents during cleanup.

### Infrastructure discovery and diagnostics

Use Codemowers MCP for global engineering guidance and the target cluster's
Driftmower MCP for capabilities, inventory, admission, schemas, access, logs and
metrics. Use the configured Codemowers advisory connection and the Driftmower
connection whose `get_cluster_identity` matches the configured target cluster. Deployment-specific
MCP URLs belong in the separately provisioned ConfigMap, not repository configuration.

The generic Driftmower alias can point to a different cluster. If native tools
are absent, use the authenticated MCP transport with its scoped ServiceAccount
flow. Read `structuredContent` and warnings; workload text can be only a summary.
Use scoped inventory and exact Pod/container names for bounded log queries.
Reuse relevant discovery within a turn rather than repeatedly scanning everything.

Before infrastructure recommendations or actions, including container smoke-test
advice, read relevant Codemowers advisory sections and query Driftmower cluster
identity, provisioning capabilities and namespace readiness. For platform manifests,
read applicable admission policies/bindings, CRD schemas and exact operation access.
Schema discovery does not establish write permission. Short-hostname conventions
must come from policy discovery, not guessed domain suffixes or existing Ingresses.

Use Driftmower `query_logs` before `kubectl logs`. Empty Loki results do not prove
success or absence of Kubernetes logs. Check scope/time/source; use a bounded
kubectl fallback only for a missing/unavailable capability or an evidenced missing
result, explaining the concrete reason. Apply manifests and wait for rollouts
through kubectl/Skaffold when MCP has no matching operation. Never exec into Pods
for diagnostics. Do not expand RBAC to silence inventory warnings.

## Current architecture and deployment

The controller is ordinary Node.js reconciliation/API code. It runs the OIDC SPA,
Task API, authenticated receivers, IMAP/SMTP, execution-bound Agent MCP and worker
orchestration. Claude and Codex run in separate coding worker Pods; the controller
does not run a coding CLI or delegate routing authority to an LLM.

| Component | Responsibility |
| --- | --- |
| `codemowers/agent-controller` | Smaller controller image, SPA, integrations, scheduling and outcome reconciliation |
| `codemowers/agent` | Ubuntu coding runner, native CLIs, workspace/session adapter and project tools |
| Redis-compatible store | Tasks, bounded timeline, queues, receipts, external mappings, execution/session identity and mail state |
| Task PVC | Git checkout, edits, artifacts, caches, progress notes and native CLI session files |
| Existing platform | Namespace ordering, tenancy, quotas, isolation, workload admission, storage and registry |

There are no Agent, Task, Trigger or Workspace CRDs in this release. Redis/
Dragonfly is the Task authority. PostgreSQL, memory checkpoint/restore, Agent
Substrate and a new sandbox management system are unnecessary for the harness.
Existing platform offerings remain available to applications built by workers.

The supported baseline is Kubernetes 1.36+ with OCI image volumes, Pod user
namespaces, persistent storage, Redis and OIDC. The chart uses platform operators;
other clusters must provide compatible operators or adapt those resources to
existing services. Codemowers.cloud is an optional ready-configured environment.
Do not add legacy tool-copying fallbacks for unsupported runtimes.

Read the deployment context and namespace from the ignored local `skaffold.env`.
The short host defaults to `controller` and admission expands it. Keep unrelated
workloads untouched. Browser TLS terminates at Ingress; Ingress-to-controller traffic
uses HTTP for now. OIDCClient registers explicit short source URLs; Passmower
has no Ingress discovery. Read issuer, client ID, internal URI, external origin,
redirect URIs and allowed groups from the provisioned owner Secret.

Use only the Pod `metadata.namespace` fieldRef for namespace context. Redis keys
and signed-link scope use namespace name and release. No namespace UID field,
lookup, labels or environment variable, and no additional Namespace RBAC.
Pod UID remains necessary for execution identity and safe adoption.

`chart/values.yaml` exposes generic `host`, `allowedGroups` and `allowedUsers`
defaults and
requires an explicit public `apiResource` for OIDC `allowedResources`.
`skaffold.yaml` reads it from `AGENT_API_RESOURCE` in local `skaffold.env`.
The `dev-test` profile binds `laurivosandi` as its sole allowed account and
sets that cluster's verified public API resource. Map the generated
`OIDC_ALLOWED_USERS` owner Secret key to controller `OIDC_ALLOWED_SUBJECTS` so
both Passmower and the Task API enforce the individual allowlist.
Do not use the short host for this resource: OIDC admission expands URI,
redirect and CORS fields but does not expand `allowedResources`.
`skaffold.yaml` injects image references/runtime env. `AGENT_KUBE_CONTEXT` comes
from local `skaffold.env`; there is no separate cluster values file. Storage,
operator and tool-image defaults live in the chart. Controller updates use
RollingUpdate with maxSurge 1 and maxUnavailable 0; a Lease fences reconciliation
while replicas overlap.

### Configuration and identity

| Configuration | Contract |
| --- | --- |
| `AGENT_USERNAME`, `AGENT_NAME`, `AGENT_EMAIL` | Git bot, displayed worker identity, Git and default mail identity |
| `AGENT_FORGE_PROVIDER`, `AGENT_FORGE_URL`, optional `AGENT_FORGE_API_URL` | GitHub/GHE or Gitea server and API endpoints |
| `AGENT_NAMESPACE`, `AGENT_RELEASE` | Installation scope, supplied by deployment |
| `AGENT_WORKER_IMAGE`, provider Secret references | Approved worker image and selected CLI credentials; Git token Secret may be absent for prompt-only Tasks |
| `AGENT_KUBE_CONTEXT`, `AGENT_TOOL_IMAGES` | Prepared kubeconfig context and image-volume tools |
| `AGENT_STORAGE_CLASS`, `AGENT_WORKSPACE_SIZE` | Workspace provisioning defaults under platform policy |
| `AGENT_MODEL`, `AGENT_CODEX_MODEL`, `AGENT_BUDGET_USD` | Hosted model defaults; USD cap currently applies to Claude |
| `OIDC_IDP_URI`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_ORIGIN`, `OIDC_CLIENT_URI`, `OIDC_REDIRECT_URIS` | Provisioned OIDC configuration; API audience derives from origin plus `/api` |
| `OIDC_ALLOWED_GROUPS`, `OIDC_ALLOWED_SUBJECTS` | API authorization allowlists |
| `REDIS_URL`, `REDIS_PASSWORD` | Controller-only Task store connection |
| `WEBHOOK_TOKEN`, `ALERTMANAGER_TOKEN` | Authenticated receiver bindings; GitHub uses its signing credential |
| `IMAP_*`, `SMTP_*` | Independent controller-owned inbound and outbound mail connectivity |
| `AGENT_REGISTRY_PREFIX` | Optional repository path; empty means registry host alone |
| `AGENT_SHARING_MODE`, optional signing key | `disabled` (default), `signed` or `public` |

Identity comes from optional, separately provisioned `<release>-identity` ConfigMap
keys `AGENT_USERNAME`, `AGENT_NAME` and `AGENT_EMAIL`, imported into the controller
environment. Defaults are `agent-smith`, `Agent Smith` and `agent.smith@system.gov`.
The controller supplies the resolved identity to worker environments, Git setup,
Task context and UI attribution. Restart consumers after configuration changes.
Deployment-specific identities do not belong in Helm/Skaffold or source fixtures.

Optional `<release>-controller-mail` ConfigMap supplies non-secret `IMAP_*` and
`SMTP_*` settings. Controller-only `<release>-controller-imap-credentials` and
`<release>-controller-smtp-credentials` Secrets supply their respective protocol's
username and password keys. `IMAP_HOST` and `SMTP_HOST` independently configure the
connectors; no enable switches or shared `MAIL_*` credentials. Missing host disables
that protocol; present host with missing credentials reports incomplete_configuration.
Secret values stay out of Helm values, images, source, logs, prompts and workspace
files. Secrets also consumed by workers use `agent-worker-*`. Provider Secrets use
a `token` key. Provisioned OIDC owner Secrets retain their operator name.

Generate controller credentials in separate StringSecret resources:
`agent-controller-redis-password` (`password`), `agent-controller-webhook-token`
(`token`), `agent-controller-alertmanager-token` (`token`) and
`agent-controller-github-webhook-secret` (`secret`). Controller environment names
remain REDIS_PASSWORD, WEBHOOK_TOKEN, ALERTMANAGER_TOKEN and GITHUB_WEBHOOK_SECRET;
Dragonfly references the same Redis password Secret.

## Task identity, state and execution

Task IDs are opaque, immutable and independent of repository, issue, PR, chat
thread or native session. A Task can start from a prompt without a repository.
External references are aliases, not primary keys. Each Task owns one scratch PVC
and has at most one active execution; different Tasks run as sandbox capacity
permits. Never share a mutable checkout or implicit last CLI session between Tasks.

Every accepted prompt/action records server-assigned identity, timestamp, actor,
source and causation. User actors come from verified OIDC issuer/subject;
controller prompts and recovery actions are visibly attributed to Controller;
external actors retain connector identity; worker messages are execution-bound.
Clients/workers cannot forge user or controller attribution. Preserve creator
identity separately from later prompt authors and preserve original source events.

Task creation and mutations persist state, queue changes and idempotency receipts
atomically. The controller Lease serializes reconciliation; Redis revision checks
handle concurrent API updates. Delivery is at least once. Reconcile Kubernetes
and external effects after partial operations; there is no transaction spanning
Redis, Kubernetes and GitHub. Check existing branches/PRs/workloads before
repeating creation after uncertain outcomes.

Dragonfly snapshots run every minute on operator-managed PVCs. Acknowledging an
HTTP event does not promise zero-loss disk persistence: total store loss can lose
changes since the last completed snapshot. Retain usage/attempt/outcome metadata
for future diagnostics/billing and distinguish turn usage from session totals.

### Worker lifecycle and controls

Workers are directly managed Pods with `restartPolicy: Never`, names
`agent-worker-<task-prefix>-<execution-prefix>`, `hostUsers: false` and no
`runtimeClassName`. Use the regular runtime with platform user namespace isolation.
Kata is available as a platform option, but this instance does not use it; verify
runtime support before combining confinement mechanisms.

Persist the execution/envelope before creating its Pod, then record the actual
Pod UID. On controller startup reconcile all unfinished durable Tasks and adopt
matching live workers. Persist terminal results before releasing the Pod
finalizer and deleting Pod/input ConfigMap. Actual container exit code overrides
a worker claim. A disappeared or reused Pod pauses the Task for review. Do not
replace an unreachable worker against the same PVC until termination/fencing is
confirmed. A deletion request or expired Lease alone is insufficient.

Workers emit sequenced newline-delimited `agent_event` JSON to stdout. The
controller ingests bounded Kubernetes logs and guards both receipts and
`active.logSequence` before Redis mutations. Full native JSONL, stderr and result
files remain privately on the PVC. This internal transport is separate from
worker application diagnostics through Driftmower; Loki is optional retention.

| Action/state | Behavior |
| --- | --- |
| Prompt while busy | Persist in the queue for the next turn; do not start another execution |
| Remove queued prompt | Atomic removal; original prompt shows Removed, without another audit bubble |
| Stop | Interrupt only the current CLI turn through execution-bound HTTP control; retain Pod/PVC/session |
| Pause | Persist intent, stop the Pod, retain session, PVC and queued inputs |
| Resume | Reconcile the same Task/PVC; restart or resume its recorded native session |
| Cancel | Terminal intent; later notifications do not implicitly reactivate it |
| Accept completion | User-authorized business completion; stop remaining worker |
| Delete | Stop worker, remove scratch PVC, mail mappings/outbox and Task state through controller cleanup |

Stop matches execution and prompt IDs so stale requests cannot stop a later turn.
Pause/Cancel do not require a stale UI revision; Resume/Accept retain revision and
terminal guards. Signals try the native process group, then the child PID on
EACCES/EPERM. Expected denial/disappearance must not escape signal handlers.

Web UI and Git issue/PR Tasks keep workers warm for 60 minutes after the last
completed CLI response, including issue-comment replies. Active turns do not
consume idle time. Worker/controller use the same completedAt-based deadline.
`report_blocked` sets Waiting/AwaitingConfirmation without changing desired state
to Paused or requesting Pod deletion. The CLI ends its turn; retain the worker
for 60 minutes after that response, including ordinarily short-lived event workers.
The worker rereads the execution idle timeout after each CLI response. Idle expiry
keeps the blocker visible and the PVC/session intact. A new accepted prompt starts
the next turn and clears the displayed blocker; it does not itself grant approval
for the action requested in the previous turn. Manual Pause/Cancel still stop Pods.
Resident workers poll `/executions/<task-id>/next` for queued follow-ups with
execution credentials and the last completed sequence; claims/receipts survive
lost responses. Other event-driven runs stop after their work; a UI Stop retains
that worker for follow-ups. After idle expiry, the next event creates a Pod using
the same PVC/session. CLI success alone never proves the Task objective complete. Underlying retained
volume and backup cleanup follows platform storage policy.

Quota/admission/scheduling blocks stay Pending with a visible reason and retry
through reconciliation. Reuse sandbox resource sizing, quotas and isolation;
do not add a fixed Agent concurrency cap or duplicate sandbox limits. Model
spend/retry/deadline policies address separate control-loop costs.

### Workspace and native sessions

Always initialize Git: clone the selected authorized repository or `git init`
for a scratch Task. Configure global Git user.name/email from agent identity and
`pull.rebase=true`. Work on topic branches; do not force-push or push directly
to main/master. Creating a local checkout does not create a remote; ask before
selecting/creating one unless the Task already authorizes it.
If direct push to `codemowers/agent` is denied, advise a fork under the configured
Git account and a pull request from that fork; ask before creating the fork or
remote. Do not change repository visibility as a permission workaround.

Seed `specs/<draft-or-task>-<task-prefix>.md`, workspace `AGENTS.md` with relevant
specification/MCP references, and safe `skaffold.env.example`. Populate actual
`skaffold.env` from the same CLI defaults, refresh only owned variables each turn,
preserve unrelated settings and add it to workspace `.gitignore`. Preserve user
edits and avoid duplicate Task references. Attached draft/reference text is an
immutable execution ConfigMap file, not an oversized CLI prompt argument.

Retain native Claude/Codex session files on the PVC at stable paths and record the
session ID early. Redis transcript/final JSON alone cannot restore a CLI session.
Missing/incompatible state starts a fresh session from objective, checkout and
prior outcomes, increments the epoch and records why. Changing CLI/model requires
a paused Task with no active worker and a new conversation epoch; retain workspace
and history. Full process-memory restore and filesystem snapshots are not needed.

## Runner, models and project builds

The runner uses Ubuntu 24.04 with Node 24.18.0, Git, SSH, Bash, coreutils, find,
tar, unzip, ripgrep, jq, curl, gh, Python with venv/pip, make and C++ build tools. Preinstall
both pinned npm CLIs: Claude Code 2.1.296 and Codex 0.162.1. Install Skaffold
v2.18.0 from its binary release. Workers do not apt-install tools at startup.
The controller image contains Node, application code, schema and SPA; kubectl is
provided by an OCI image volume.

Kubectl and Go come from OCI image volumes. Kubectl uses upstream
`registry.k8s.io/kubectl:v<server-version>`. The controller Deployment uses Helm
`.Capabilities.KubeVersion.Version`; Skaffold explicitly renders with 1.36.3.
Immediately before creating a worker Pod, query the Kubernetes API server version
and select the matching upstream kubectl image, preserving additional tool images.
Mount its whole `bin` directory and prepend `/opt/tools/kubectl/bin` to PATH in
both Pod types; image subpaths must be directories. Neither image embeds kubectl.
Workers also mount the official Go image's `/usr/local/go` tree at `/opt/tools/go`;
set GOROOT and PATH for that tree, keep Go caches under the Task PVC and disable
automatic toolchain downloads. Python project dependencies belong in workspace
virtual environments, not the read-only runner root filesystem.

Tasks select hosted Claude or Codex in the UI. Freeze choices per execution.
Use Anthropic credentials for Claude and OpenAI credentials for Codex; provider
and CLI selection must agree.
Selected provider Secret references are optional for Pod creation. A worker
without a usable API key for its selected CLI records a failure and exits
nonzero before workspace setup; it never switches to the other provider.
Local-model execution is a roadmap item only;
there is no discovery, API adapter or compatibility proxy in this release.

### Namespace build contract

Before builds, obtain the execution-bound Agent MCP namespace context. It reads
only `zot-registry-cluster-push-kaniko`, using get-only named-Secret RBAC. Return
namespace, the single verified registry host, Secret name/key and safe Skaffold
settings; never credential data. Missing/malformed/ambiguous config is unavailable.
Do not generate Zot users, mutate credentials or use the reflected pull Secret
for pushing. Discovery is configuration verification, not push authorization.

`SKAFFOLD_DEFAULT_REPO` defaults to `<registry>/<AGENT_USERNAME>`.
`AGENT_REGISTRY_PREFIX` overrides the path; an explicit empty value supports a
per-user registry. Use the canonical defaultRepo consistently in Pod env,
refreshed CLI env, workspace files and MCP examples. Also supply
`SKAFFOLD_NAMESPACE`, `SKAFFOLD_KUBE_CONTEXT` and `KUBECONFIG=/config/kubeconfig`.
The prepared context is `clusterName/namespace`; retain in-cluster endpoint, CA
and rotating ServiceAccount tokenFile. Do not duplicate kubeconfig/context in
Skaffold YAML, rewrite endpoints or perform human OIDC bootstrap in a worker.

Prefer Kaniko through `skaffold build`/`skaffold run`. Prepare missing configuration
from the controller MCP schema/example and validate it offline first.
For v4beta11 use `manifests.rawYaml`, not invented deploy.kubectl manifest fields.
Schemas are generated from the pinned runner CLI at image build time. Validation
executes no commands/hooks and does not establish RBAC or admission success.

Check exact get access to the push Secret through Driftmower. Skaffold Kaniko Pod
templates carry `skaffold-kaniko` and name the executor container `kaniko`;
platform admission supplies the executor image, OCI format and builder isolation/
capabilities. Direct Kaniko requires an explicitly requested verified workflow.
Failed or unclear infrastructure is a blocker: report it and stop. Do not switch
builders, registries, namespaces, privileges or mounts as a workaround. Do not
recommend Docker/Compose inside workers; they have no Docker daemon/socket.
Never change repository/package visibility or publish to personal/ephemeral
registries to make deployment work. New remotes default private unless explicitly
requested public. Forward credentials only to their verified service.

## MCP and outcome ownership

MCP scopes are layered: Codemowers is global advisory, Driftmower is cluster
capabilities/readiness, Agent is this execution's namespace facts and reporting.
Remote machine access uses projected ServiceAccount tokens verified through
TokenReview. Driftmower's stable machine audience is
`http://driftmower-mcp.driftmower.svc.cluster.local:3000/mcp`, independent of its
public browser OAuth resource. Controller probes use controller
identity; worker bridges reread rotating tokens per request and preserve sessions.
Never copy personal OAuth credentials or tokens into CLI configuration.

Provision MCP connections in the optional `<release>-mcp` ConfigMap with a
`mcp.json` key, mounted at `/mcp/mcp.json` in controller and worker Pods. Do not
add MCP URLs or configuration Helm values. Local `mcp.json` is excluded from Git
and images. Missing configuration disables remote MCPs; built-in Agent MCP remains.
Both Pod types always project their own Driftmower token at
`/var/run/agent-mcp/driftmower`, without token-creation RBAC. HTTP entries specify
`type`, `url`, optional `tokenFile` and optional `siteURL` for browser links;
worker CLI configuration excludes display metadata. Restart consumers to apply
MCP configuration changes and refresh the startup connectivity snapshot.
Harness-owned HTTP requests identify as `codemowers-agent (+https://github.com/codemowers/agent)`;
native Claude/Codex and Kubernetes API requests retain their own user agents.

| Implemented Agent MCP tool | Purpose |
| --- | --- |
| `get_task_context`, `declare_context_controls` | Current controls, included device IDs/status, primary communication channel, reference draft and repair context; add user-requested controls without overriding choices |
| `get_namespace_context` | Namespace/build references fixed by execution binding |
| `get_skaffold_schema`, `validate_skaffold_config`, `get_skaffold_example` | Pinned schema, offline validation and namespace-specific example |
| `report_progress` | Bounded semantic progress/question, not completion |
| `report_blocked` | Record a blocker/question after notifying the primary issue; end the turn and retain the warm worker |
| `register_github_pull_request` | Verify and associate a PR, including another repository |
| `send_email`, `get_email_status`, `get_email_messages` | Authorized Task-bound mail, delivery state and routed messages only |

Device shell and file tools appear in Agent MCP only for Tasks with included
devices. Supply selected IDs and connection status in each worker turn and in
`get_task_context`; exclude device URLs and unrelated registered devices.
Exclusion rejects subsequent device calls immediately. Including a device does
not enqueue a prompt by itself. The Devices dialog shows a per-device systemd
user service setup snippet using that device's WebSocket endpoint.
The navbar shows registered devices as Offline, Online or Included, and keeps
inclusion visible when a device disconnects. Prefix displayed hostnames with
`Device`. Connected device tooltips show bounded OS details from the hello
message; do not put those details into worker context unless requested.

Workers authenticate through execution-scoped controller HTTP, never direct Redis.
Reject stale/cross-Task reporting; namespace and Task identity come from the
binding, not model arguments. The controller verifies actual worker termination,
results and external evidence. MCP progress or a model completion claim does not
own lifecycle. Missing final reports must remain recoverable from worker output
and observed exit state; successful exit does not establish acceptance criteria.

Supporting worker RBAC covers approved namespaced workloads/platform offerings
(OIDC, S3, CNPG, Dragonfly, RabbitMQ, Redpanda, OpenSearch, Elasticsearch, EMQX,
MongoDB), certificates, generated Secrets, monitoring and snapshots. There is no
cluster-wide CRD/Namespace access or general Secret permission for build discovery.
Discover schemas through Driftmower and separately check intended operations.
Do not add RBAC without explicit user direction. Existing workload exec/attach
permission is not permission to introduce Pod-exec diagnostic endpoints.

## Event routing and communication

Authenticate receivers, bind them to installation/namespace identity and persist
accepted events before acknowledgement. Payloads cannot choose tenant, credentials
or arbitrary repositories. Exact references route deterministically to canonical
Tasks; do not run a routing LLM in the current controller.

The Git token Secret is optional. Without `GH_TOKEN`, skip Git discovery, issue/PR
reconciliation and GitHub notification-mail routing; allow prompt-only Tasks and
workers to start without Git authentication. Git API actions report unavailable.

### Git issues, pull requests, basic webhooks and alerts

The Git receiver supports issue updates, timeline comments (including PR comments),
pull request updates and review events. Verify signing credentials, the configured
server's API, repository access and allowlists. Routing has three rules:

- An existing Task receives updates even after the agent is unassigned.
- Without an existing Task, create one only for an open issue/PR currently assigned
  to the authenticated token's account. Self-assignment starts work.
- Ignore the agent's own comments/reviews to prevent reply loops.

Mail and webhooks use the same reference key: provider, server URL, kind (issue
or PR), repository and number. Issue and PR identities are distinct, as are
identical repository/number pairs on different servers. Missing indexes rebuild
from durable Task references. Deliveries retain transport idempotency, without
model-read cursors, comment catch-up state or updatedAt-based prompt suppression.
The first automatic wakeup starts immediately. Store a fixed three-minute
notificationSuppressUntil deadline per Task from that wakeup so controller
restarts retain it. Git notifications, basic webhook deliveries and Alertmanager
events share the deadline. Follow-up automatic notifications during the window
record their source events but do not queue another prompt, including notification
email for the same Git event. Later arrivals do not extend the deadline. Direct UI
prompts and participant email replies dispatch immediately. Pause/Cancel/Delete
retain their normal authority.
Queued controller notifications for the same thread may fold into one pending
prompt; preserve each source event and link it through the prompt's causedBy list,
bounded to retained history. Coalescing uses the same
provider/server/kind/repository/number key and persists inside the Task mutation.
Never alter claimed/running prompts or fold human messages. Once the deadline
expires, the next automatic notification starts a new immediate turn.
First delivery stores one source event and controller prompt; later updates reuse
Task/PVC. Unassignment never deletes the Task automatically. The model assesses
updates; Pause queues them, and Cancel/Delete prevents automatic reactivation.
Titles use issue/PR titles, with body/details kept in the original source,
objective and workspace specification rather than duplicated controller bubbles.

The elected controller discovers assigned open issues and PRs on startup and
every five minutes, using the configured server's assigned-issue search. API-verify
missing assignments before creating Tasks; process at most 20 new candidates per
pass, with at most ten pages of 100 search results and a logged incomplete flag.
Existing Tasks do not gain prompts merely because they appear in assignment search.
Deleting an open, still-assigned remote item can cause discovery to recreate its
Task; close/unassign it or retain a cancelled Task to stop processing.

Optional identity ConfigMap keys select `AGENT_FORGE_PROVIDER` (`github` default
or `gitea`) and `AGENT_FORGE_URL` (default `https://github.com`). GitHub Enterprise
uses `/api/v3`; Gitea uses `/api/v1`; `AGENT_FORGE_API_URL` can override the API.
Pass resolved endpoints to workers and UI; authenticated requests stay on that
configured API and reject redirects. `GH_TOKEN` and the existing Git credential
Secret supply the selected server's token. GitHub/GHE workers use gh, Gitea workers
use its API and a server-scoped Git authorization header held in the environment.
The receiver keeps `/hooks/github` for both providers, validating GitHub's prefixed
signature or Gitea's hexadecimal signature and provider-specific delivery headers.
Gitea routing/API behavior is covered by fixtures; live end-to-end validation remains
necessary before claiming production support.

Issue/comment/mail/PR-merge prompts contain just the event type, thread URL and
an instruction to read the latest updates and decide what to do. The model uses
its judgment about action and communication; do not add long response restrictions
or copy issue/comment bodies into these prompts. Mounted instructions use the
same brief guidance.

Assume collaborators are senior engineers. Keep communication brief and direct,
focused on outcomes, blockers and decisions requiring input. Give detailed plans,
explanations and logs when requested. Progress updates cover meaningful changes
or blockers; skip routine narration, acknowledgements and repeated summaries.
Final reports briefly state the result, relevant validation and material limitations.

For issue-backed Tasks, the original Git issue is the primary communication
anchor. For standalone PR Tasks, that PR is the primary thread. Mail and chat are
secondary, even when they trigger the next turn.
Workers post meaningful results, blockers and confirmation questions to that
issue. Post the blocker and specific question before `report_blocked` or ending
a turn awaiting confirmation; read existing comments to avoid duplicates. A
failed GitHub comment is included in the blocker report. `report_progress` and
`report_blocked` record UI state and do not publish GitHub comments themselves.
Task context and mail tools expose this priority; execution envelopes retain
references so resident follow-ups preserve the anchor if context fetching fails.
Summarize relevant mail/chat decisions on the issue without copying private
messages or credentials. Incoming mail remains attributed to its sender.

Verified issue closure completes the Task, blocks further prompts and wins over
late results. The elected controller reconciles unfinished issues on startup and
every 60 seconds; do not manually reconcile user Tasks. Register linked PRs
immediately. Verify bot-authored legacy transcript links if recovering references;
merges wake the same Task across repositories. PR assignments create missing
standalone Tasks; linked PRs continue to reuse the original issue Task. Poll at startup/
60 seconds and process signed merge hooks, deduplicate notifications, avoid replaying
historical merges and preserve pauses. Issue closure still completes work.

An issue lookup returning 404/410 marks the reference unavailable and stops its
automatic polling, with one durable task notice. Check repository access to
distinguish a missing issue from an unavailable repository; in the latter case
also stop related PR polling in that repository. Unavailability does not prove
task success or deletion rather than lost access. Retain the task/PVC and existing
worker lifecycle. Resume or a new API-verified issue event clears the unavailable
markers. Other lookup failures log bounded HTTP status without upstream bodies.

Basic webhook input requires a stable deliveryId and an authorized Task ID or
verified issue URL. Unassociated-event triage is planned, not implemented here.
Alertmanager receivers authenticate batches and correlate repeated fingerprints
to the same Task. Resolved alerts update context but do not prove code completion.
An issue is optional; never guess a repository for an unmatched event.

Log every `/hooks` hit before parsing and its completed HTTP/auth/routing outcome,
including rejected/malformed calls. Allowlist bounded delivery/event/action,
verified repository/issue/actor IDs and Task links; alerts expose count/status only.
Never log raw payloads, submitted body text, signatures/auth headers, query strings,
URLs, alert labels/annotations or credentials.

### IMAP and SMTP

Mail connectivity belongs to the elected controller. Keep IMAP IDLE independently
of workers; send authorized plain-text mail through SMTP. Workers get no mail
credentials. Outgoing messages bind deterministic Message-IDs, recipient participants
and stable idempotency keys to the Task. Replies use In-Reply-To/References to wake
that Task, including issue Tasks, with external sender attribution. Deduplicate
Message-IDs and UIDVALIDITY/UID cursors. Preserve pauses; ambiguous, unmatched,
self-authored, automated or non-participant mail does not start work. Optional
`IMAP_ALLOWED_SENDERS` extends the participant set. Do not automatically broadcast
GitHub events or mirror incoming mail as issue comments.

GitHub notification mail resolves issue/PR identity from Message-ID/reply headers,
then checks authoritative API state. Support singular/plural issue paths, issue_event
and numeric comment IDs. Create missing Tasks only for assigned open issues/PRs; existing Tasks receive
updates after unassignment. Closed issues converge to completion. GitHub Enterprise
notification Message-IDs and senders use the configured server hostname. Gitea
notification-email identity parsing remains deferred; task-bound mail replies work
independently of the Git provider. A missing number or direct 404 falls back to assigned
open issues in that repository only, bounded to ten pages of 100; authorization
denial does not broaden a scan. Honor allowlists and webhook/mail deduplication.
Email bodies are never routing authority or automatically executable instructions.
First routing-enabled startup checks the latest 200 inbox headers to create missing
assignment Tasks, without waking existing Tasks from old mail.

`IMAP_HOST` and `SMTP_HOST` are independent: setting a protocol hostname means
that connector is intended to run. Its Secret supplies `IMAP_USERNAME`/`IMAP_PASSWORD`
or `SMTP_USERNAME`/`SMTP_PASSWORD`. Login defaults to agent username and SMTP From
to agent email; `SMTP_FROM` and `SMTP_REPLY_TO` override sending identity. IMAP
defaults to TLS/993 and INBOX; SMTP to TLS/465 with required STARTTLS on other ports
when TLS is enabled. Endpoint settings and credentials are never copied to workers.
Fetch live STATUS
UIDNEXT/UIDVALIDITY before drains; cached UIDNEXT after EXISTS is insufficient.
Keep IDLE plus a leader-only 30-second catch-up, reconnect with backoff and retain
cursors on error. Read without changing Seen; bound message intake to 256 KB.

Queue mail before sending and retain sent/partially_sent/failed/uncertain state.
SMTP uncertainty is not automatically retried. `SMTP_SAVE_SENT` defaults off; enabling archival requires configured inbound IMAP.
Discover the unique Sent-role mailbox or use `IMAP_SENT_MAILBOX`,
APPEND a Seen copy with the same Message-ID and track archive retries separately.
Search Message-ID before uncertain APPEND retries; enabling archival can process
retained sent outbox messages without resending SMTP. Restore INBOX for IDLE.

### Process fault recovery

Install a process-wide exception/rejection handler that reports at most once per
process lifetime, regardless of exception kind, and keeps the application running.
Create a controller-attributed repair Task associated with codemowers/agent.
Bound/redact diagnostics and reporting; workers include original Task/execution
and use existing execution HTTP credentials. The controller durably gates reports
by source process/execution and uses one active repair Task per component across
workers/restarts. Record distinct normalized fingerprints and queue one follow-up
for a new fingerprint in that Task. Repeated fingerprints add bounded occurrences
without another prompt. A terminal repair Task permits a new Task for later
reports. Repair-worker failures attach
bounded occurrences to its root repair Task without new prompts or task chains.

Caught IMAP/reconcile failures use the same guard and continue recovery on the next
tick. Worker turn exceptions abort that turn and retain the resident Pod. Reporting
failure must not recurse; unavailable storage/controller or pre-reporter startup
errors can prevent durable reporting. Include safe IMAP operation/UID/status data,
not commands/bodies. Applying a fix requires normal review and process restart;
do not auto-merge or alter unrelated Tasks.

## Web UI, authorization and sharing

Use a public-client OIDC Authorization Code PKCE S256 SPA with no browser client
secret or BFF. Use the provisioned issuer/client/origin/redirect configuration,
exact redirects and API-scoped access tokens. Validate signature, issuer, audience,
expiry and `at+jwt` type on API requests; ID tokens are not API credentials.
Identify users by verified issuer/subject and apply authorization on each request.
Persist the OIDC client's user record, including its refresh token, in browser
localStorage so a signed-in session survives reloads. Keep redirect transaction
state in sessionStorage, clear the stored user on sign-out, and renew API-scoped
access tokens with the configured resource. Never put tokens in URLs, history or
workers. The API still validates every bearer token and its current authorization.

This instance permits `github.com:codemowers:admins` in OIDCClient and API. Explicit
subject/group allowlists override namespace membership; namespace membership is
a fallback only when no explicit allowlist exists. UI gets no Kubernetes or bot
credentials. Anonymous public draft/shared views are separately scoped below.

### Task screens and provenance

- New Task is an inline two-step wizard: Start from scratch/draft, then editable
  prompt/reference, CLI/model and optional repository. No modal or redundant
  landing-page New button. Repository selection is API/allowlist validated.
- List at most ten accessible GitHub repositories with counts, external links and
  creation destinations. Distinguish organization policy/token uncertainty; do
  not probe by creating repositories. Creation links open GitHub.
- Task URLs select the exact Task. List labels are `org/repo #123 Title`, ellipsized
  with a full tooltip. Desktop sidebar is at least 30em and resizable by pointer/
  keyboard with locally retained width; below 900px it is a burger drawer.
- Use compact title/status/actions and optional repository link. Pod/draft/model
  details belong in settings/tooltips, not duplicate heading/status rows or UUIDs.
  Show worker dots, actual Pod names in lifecycle/countdown, clear busy/queued/running
  prompts and idle time. Use Awaiting input after a completed response.
- Human bubbles are left, agent bubbles right. Display configured agent identity
  and author/time once, without IdP URLs. GitHub names link in a new tab; GitHub
  avatars fall back to verified-email Gravatars/stable defaults. These display
  identities do not establish authorization. Shared views retain explicit public
  GitHub names, not OIDC subjects.
- Merge consecutive progress/result chunks for one execution, with sanitized
  Markdown. Links/bare URLs/URL-only inline code open new tabs with noopener/noreferrer;
  fenced code stays code. Show bounded tool routing/file/command metadata and states,
  never raw results or credential commands in normal timelines or shared views.
  When `AGENT_MCP_PAYLOADS_ENABLED=true`, authorized API users may expand
  bounded, redacted MCP request/response JSON through a separate Task API.
  Keep the payload API and UI hidden when disabled; never expose payloads in
  shared links. Preserve expanded boxes across refreshes.
- Render controller lifecycle/control events as compact icon lines with metadata
  tooltips. Removed prompts show their disposition without duplicate audit bubbles.
- Keep Pause/Resume mutually exclusive; icon actions share a row. Enter submits,
  Shift-Enter is multiline and IME composition does not submit.
- Report elapsed/API time, input/output/cache tokens and whole-turn output tokens/s;
  this is not model decode speed. Show readable transient proxy errors and clear
  them on recovery.
- Use restrained Matrix greens, red errors and amber pending states. Preserve full
  wrapping taglines, automatic header height and a bottom composer with a scrolling
  timeline filling the remaining viewport. Link the brand to codemowers/agent on GitHub.

### Drafts, refinement and context controls

Markdown sources in `chart/drafts/*.md` render into the mounted drafts ConfigMap.
Catalog/content and editable draft composition are public without OIDC, independent
of sharing mode. Task creation still requires admin authentication. Paint, File drop
and Pad retain product-specific requirements, not copied MCP provisioning recipes.
Selection loads editable text; creation snapshots it into the immutable execution
ConfigMap, bounded to 128 KB. Preserve composer edits through PKCE login.

Refine this prepares an editable new Task targeting `codemowers/agent` with a bounded
transcript reference; it does not mutate the source Task. Read-only shared headers
contain only the title and Refine this. Authentication is required to start work.

Context knobs are generic named booleans with labels/descriptions. They come from
an `agent-controls` fenced YAML block, draft `agentControls` front matter, API or
user-requested MCP declaration. Render dynamic switches and preserve user choices.
Pass current values every turn and read `get_task_context` before governed actions.
They guide agent behavior; toggling a knob does not enforce external CI/workload state.

### Connectivity

After verified OIDC authentication, show Codemowers MCP, Driftmower MCP, Agent MCP,
GitHub API, Inbound mail, Outbound mail and Artifact registry. Remote MCP/GitHub/
registry badges open their sites; local Agent/mail badges have no link. Align right
and wrap, moving badges into the burger drawer on mobile. No button borders or
separator above them. Logout is an icon with identity in its tooltip; Instructions
and Connect events remain in the sidebar. Hide checks on logout/token rejection.

Checks run once per controller process startup with bounded, sanitized failures.
MCP/GitHub remain cached. Normal IMAP STATUS/connect/error and SMTP send outcomes
update mail snapshots passively; task-list refreshes carry them without re-probing.
Repository discovery refresh is independent. Tooltips show actual checkedAt, scope
and outcome; green means a check passed, not continuously monitored liveness.

Probe remote MCP initialization with scoped credentials, GitHub `/user`, isolated
read-only IMAP login/inbox and SMTP verify without sending mail. Agent checks its
shared initializer/storage, not unissued worker tokens. These checks do not mutate
mail cursors/outbox/flags or resident IDLE. Successful handshake is not invalidated
by session cleanup failure.

Registry check pushes a uniquely tagged tiny zero-layer OCI image under the
configured prefix and `agent-connectivity-check/<namespace>`, verifies manifest
read-back, then deletes only that manifest digest. Credentials stay with the
provisioned HTTPS registry; reject redirects to other destinations. Report cleanup
failure separately from push success, with HTTP status/reference. Unreferenced
config blobs await registry GC. This instance's push succeeds; deletion was denied
HTTP 403. Do not add permissions or silently publish a fallback to hide that result.

### Read-only sharing and deferred diagnostics

Sharing modes are disabled, signed and public; development uses public. Signed
links require HMAC but no OIDC and have no expiry. Scope to installation and Task
share epoch; revocation increments the epoch and removes public listings. Public
mode lists only explicitly shared Tasks to anonymous visitors. Disabled mode denies
all shared reads. All creation/revocation/mutation requires normal admin OIDC.
Use an explicit signing key or a domain-separated key derived from the provisioned
webhook credential; never expose it to workers/browser. Shared views are allowlisted
and read-only, without signing keys, execution tokens or provider credentials.

Instructions view reads only the mounted worker instructions with source/revision/
read time. Workers reread the ConfigMap directory each turn, with no subPath;
propagation is eventual and active turns retain their starting text. This view is
not full input-context inspection or proof of an active worker's exact config.
Diagnostic endpoints, input-context capture, log viewers and PVC usage display
remain deferred. Revisit their access design before implementation; do not add
Pod exec or RBAC for diagnostics.

## Future design: codemowers/agent-operator

This section retains proposed declarative APIs and connectors only. These are
not installed CRDs, current endpoint guarantees or upstream AX manifests.
The future `codemowers/agent-operator` owns CRD reconciliation and launches the
same coding runners; migrate state with one explicit lifecycle authority.
Do not implement this design as part of routine work on the current harness.

### Definitions and routing

| Definition | Owns |
| --- | --- |
| Agent | Reusable instructions, CLI/model, Git identity, MCP configuration and readiness |
| Task | Specific objective/acceptance, desired state, canonical ID, external references and scratch PVC |
| Trigger | Exactly one authenticated event source, installation/namespace binding, filters and routing policy |
| Workspace | No separate kind; storage belongs to Task |

Resources are namespaced `agents.codemowers.io/v1alpha1`. Users declare spec;
controller owns status. Store bounded status summaries/session/execution identity
there, with transcripts/payloads/results outside Kubernetes objects. A configured
Agent is ready without an idle worker. Record Agent generation and resolved image
for attempts. Reuse sandbox ServiceAccounts, quotas and admission; no fixed Agent
execution cap or payload-selected namespace.

Exact external mappings remain deterministic and rebuildable from Task references.
An optional routing LLM runs in a separate triage worker with namespace-scoped
search tools. It proposes resume/create/ignore/clarification; controller validates
repository access and persists the accepted mapping before queuing developer work.
The triage conversation stays separate and bounded. CI/review completion policies
may target PR creation, verified checks, approval or merge; tie evidence to the
current PR head and required checks, not a successful CLI turn or stale commit.

Slack, expanded Gitea connectors, direct CI connectors and unassociated-event triage are planned.
For Slack, key by installation/team, channel and root thread, authenticate/deduplicate
events, ignore self/bot chatter and acknowledge durable receipt. Multiple threads
may link one Task. Preserve reply destinations and pending questions; channel
membership alone does not authorize another user's controls or repository.

Unassociated webhook errors would start a triage Pod using three separate keys:
source (`cluster + namespace + pod/workload`, or `host + container`), incident
(source plus stable error fingerprint) and delivery (integration plus deliveryId).
Optional Pod/container IDs describe an occurrence, not namespace identity. Persist
observations/mappings atomically, allow one triage execution per incident occurrence
and validate a proposed repository/issue/PR before linking or creating it. A resolved
incident followed by a new failure creates a new occurrence, not an implicit resume
of completed developer work. Payload tenant fields never establish authorization.

Future gateway/connector, API/router and reconciler can share a Deployment; coding
and optional triage workers remain separate. IMAP belongs to long-lived connectors,
not sleeping workers. Namespace deletion disconnects connectors/stops executions
and applies explicit state/PVC retention. Prevent inheritance of stale deleted
namespace records without introducing namespace UID fields into this harness.

### Declarative examples

These sketches illustrate the proposed contract; image versions, model IDs,
Secret names and check names must come from the actual installation/repository.

#### Agent

```yaml
apiVersion: agents.codemowers.io/v1alpha1
kind: Agent
metadata:
  name: backend-developer
  namespace: customer-sandbox
spec:
  backend: codex # codex or claude
  image: codemowers/agent:v1
  gitIdentity:
    provider: github # gitea support later
    serverURL: https://github.com
    username: backend-bot
    credentialSecretRef:
      name: github-agent
  instructions: |
    Implement assigned changes and open pull requests.
    Run relevant tests and report their results.
    Create test workloads in this sandbox when useful.
    Continue work after CI failures and review feedback.
  model:
    name: YOUR_MODEL_ID
    credentialSecretRef:
      name: model-api-key
  tools:
    mcpServers:
      - name: github
        url: http://github-mcp.customer-sandbox.svc:8080/mcp
```

#### Task

```yaml
apiVersion: agents.codemowers.io/v1alpha1
kind: Task
metadata:
  name: implement-pagination
  namespace: customer-sandbox
spec:
  agentRef:
    name: backend-developer
  objective: |
    Implement pagination for GET /projects.
    Preserve existing response fields and open a pull request.
  acceptanceCriteria:
    - Tests cover empty and multi-page results.
    - Relevant CI checks pass.
  completionPolicy:
    milestone: VerifiedChecks # PullRequestCreated, VerifiedChecks, Approved, Merged
    requiredChecks:
      - unit-tests
  externalReferences:
    - source: github
      repositoryId: "123456"
      kind: issue
      id: "42"
  desiredState: Active # Active, Paused, or Cancelled
```

#### Task status

```yaml
status:
  phase: Waiting
  reason: WaitingForCI
  observedGeneration: 1
  scratchVolume:
    claimName: agent-task-7c4e9d
  session:
    backend: codex
    id: thread-uuid
    epoch: 1
  lastExecution:
    id: execution-uuid
    podName: agent-worker-7c4e9d-3
    podUID: pod-uuid
    state: Succeeded
    exitCode: 0
    resultRef: execution-uuid
  progress:
    summary: Opened PR 87; waiting for CI.
    updatedAt: "2026-10-11T10:00:00Z"
  outputs:
    pullRequestURL: https://github.com/example/backend/pull/87
  conditions:
    - type: Ready
      status: "True"
      reason: Configured
      message: Ready to process follow-up events.
      lastTransitionTime: "2026-10-11T10:00:00Z"
```

#### Trigger

```yaml
apiVersion: agents.codemowers.io/v1alpha1
kind: Trigger
metadata:
  name: backend-github
  namespace: customer-sandbox
spec:
  agentRef:
    name: backend-developer
  source:
    github:
      repository: example/backend
      webhookSecretRef:
        name: github-webhook
      events:
        - issues
        - issue_comment
        - pull_request_review
        - workflow_run
  routing:
    mode: ReferencesThenTriage
    unmatchedAction: Triage
  instructions: |
    Start work for issues labeled agent-task.
    Review comments, CI failures, and reviews; act only on relevant changes.
    Ignore non-actionable updates; do not post acknowledgements.
```

Agent holds reusable identity/model/MCP behavior; Task holds the work and storage.
Trigger endpoints/readiness belong in controller-owned status. Sessions include
backend and epoch; record actual execution Pod identity and external outputs.
Keep phase separate from desired state and semantic waiting reasons. A Task may
wait for CI, review or human input after a successful native turn. Operator-side
worker owner references apply only when the Task CRD exists.

### Deferred directions

Executable image mounting for Claude/Codex, language-base selection and a static Go
worker adapter remain exploratory. Image volumes expose read-only files, not merged
root filesystems; image entrypoints/env do not run. Verify executable paths, absolute
symlinks, loaders/libraries and native session/MCP behavior before changing the
production npm installations. Mount compatible directories and combine PATH.
An Alpine/musl utility image cannot be assumed executable in Ubuntu by PATH alone.
A static adapter would still need the coding CLI, Git/toolchain, credentials, PVC
state and execution-envelope/progress/result/resume/interruption contract.

AX schemas and runner supervision/workspace hooks are possible design references.
Any reuse must preserve this harness's tenant authority, execution identity, CLI
configuration and progress/result contract; AX/Agent Substrate is not a runtime
dependency. Memory restore, filesystem branching, cross-namespace delegation,
billing and a full sandbox portal remain outside the initial scope.

Future reporting may add waiting/artifact/follow-up proposals; they do not transfer
lifecycle authority to the model. Persist timers and pending questions, reconcile
them after restarts and clean or retain Task-created workloads by explicit policy;
stopping a worker alone does not delete its deployments. CI/review/chat connectors
need duplicate/out-of-order delivery, loop suppression, pause and stale-report
verification. Shared control/queue behavior must remain identical across sources.
