# Security model

How `codemowers/agent` confines workers, where credentials go and what the
cluster administrator must enforce. The [README](../README.md) covers
installation.

## Resource kinds

All kinds are in the `codemowers.cloud/v1alpha1` API group. Namespace
administrators are humans: no automation gets RBAC to create or change Agents,
and workers never read or write these resources through the Kubernetes API.

| Kind | Scope | Administered by | Read by | Changed by |
| --- | --- | --- | --- | --- |
| Agent | Namespaced | Namespace administrators | Operator; dashboard users allowed on the Agent | Administrators; operator writes status |
| Task | Namespaced | Namespace administrators; the operator creates event and dashboard Tasks | Operator; dashboard users allowed on its Agent | Administrators; operator on behalf of users, events and the worker's MCP calls |
| WebhookBinding | Namespaced | Namespace administrators | Webhook receiver | Administrators |
| WebhookReceiver | Namespaced | Namespace administrators | Webhook receiver | Administrators |
| ClusterWebhookReceiver | Cluster | Cluster administrators | Webhook receiver | Cluster administrators |
| WebhookProtocol | Cluster | Cluster administrators | Webhook receiver | Cluster administrators |
| TaskPolicy | Namespaced | Namespace administrators | Operator | Administrators |
| Mailbox | Namespaced | Namespace administrators | Operator | Administrators; operator writes the IMAP cursor |
| MailBinding | Namespaced | Namespace administrators | Operator | Administrators |
| Device | Namespaced | Namespace administrators | Device bridge | Administrators; bridge writes connection status |
| Feed | Namespaced | Namespace administrators | Feed crawler | Administrators; crawler writes crawl state and recent items |
| FeedBinding | Namespaced | Namespace administrators | Feed crawler | Administrators |
| ClusterMCPServer | Cluster | Cluster administrators; the chart for built-in servers | Operator | Cluster administrators |
| MCPServer | Namespaced | Namespace administrators | Operator | Administrators |

- **Agent:** a worker definition with CLI and model, Pod template, workspace
  volume claims, Git providers, idle timeout, coalescing window and extra MCP
  servers.
- **Task:** one objective with its own PVC, conversation, routing keys such as
  an issue URL, and external references.
- **WebhookProtocol:** a sender's signature format, CEL extraction, allowed key
  prefixes and HTTP response.
- **WebhookReceiver** (`/hooks/<namespace>/<name>`) and
  **ClusterWebhookReceiver** (`/hooks/<name>`): an endpoint holding the
  sender's secret, token or key, and optional address limits.
- **WebhookBinding:** subscribes one Agent to a receiver for key prefixes such
  as `https://github.com/codemowers`, with a fallback Task. A cluster receiver
  fans each event out to every matching binding.
- **TaskPolicy:** when an event whose key no Task claims may create a Task.
- **Mailbox:** a Pod holding one account's IMAP and SMTP credentials.
  **MailBinding** wakes one Agent's Tasks for matching mail headers.
- **Device:** a remote machine and its connection key.
- **Feed:** a public feed the crawler polls; **FeedBinding** wakes a Task of
  its namespace with the feed's new items.
- **ClusterMCPServer** and **MCPServer:** an MCP server advertised to workers.
  A worker token's audience is the server's `audience`, else its Service,
  `<service>.<namespace>`, derived from an in-cluster URL.

Dashboard users reach an Agent's Tasks if they are listed in its `spec.access`
or, without that list, if they have `get` on the Agent.

Workers get a Mailbox, Device or MCP server, and callers wake a Task, through
custom RBAC verbs on that object. Mailbox and Device verbs are checked by
their MCP server on every call; `use` is checked when the operator builds a
worker Pod. Standard verbs such as `get` grant none of them, but wildcard
grants such as cluster-admin include them all.

| Kind | Verb | Allows |
| --- | --- | --- |
| Mailbox | `send` | Sending mail |
| Mailbox | `read` | Listing folders and messages, reading messages |
| Device | `exec` | `device_shell` |
| Device | `read` | `device_file_read` |
| Device | `write` | `device_file_write`, which usually allows code execution too |
| ClusterMCPServer, MCPServer | `use` | The server's endpoint and a token for its audience in the worker |
| Task | `wake` | Waking the Task through the operator's wake endpoint |

## Kubernetes permissions

| Identity | Permissions |
| --- | --- |
| `agent-operator` | Reads Agents, Mailboxes, MCP servers and policies; manages Tasks, worker Pods, PVCs and Mailbox Services; writes Agent, Task and Mailbox status; reads `agent-operator-provider-tokens` by name; creates TokenReviews and SubjectAccessReviews. |
| `agent-operator-webhook` | Reads protocols, receivers, bindings and the Secret each receiver names; creates TokenReviews for ServiceAccount-token senders. |
| `agent-devices` | Reads Devices and the Secrets named in `deviceBridge.secretNames`; writes Device status; creates TokenReviews and SubjectAccessReviews. |
| `agent-mailbox` | Creates TokenReviews and SubjectAccessReviews. Only the Mailbox Pod's MCP container gets its token. |
| `agent-feeds` | Reads Feeds and FeedBindings, writes Feed status, holds `wake` on Tasks cluster-wide and creates TokenReviews. FeedBindings decide which Tasks it wakes. |
| `agent-worker-<agent>` | Whatever the namespace administrator grants; [examples/](../examples/README.md) has three profiles. |

### Credential storage

Credentials live in namespaced Secrets, and resources select individual keys,
so no component mounts a whole Secret. Keep the values out of manifests,
images, chat and shell history.

| Secret | Holds | Selected by |
| --- | --- | --- |
| `agent-operator-provider-tokens` | Model API keys and Git provider tokens | `credentialKeyRef` in an Agent's model and Git providers |
| Any, conventionally `agent-operator-webhook-keys` | Webhook signing keys | `senders.secretKeyRef` in a receiver |
| A name allowed by `deviceBridge.secretNames` | Device connection keys | `connectionSecretRef` in a Device |
| Any | Mailbox IMAP and SMTP credentials | `credentialsSecretRef` in a Mailbox |

A worker without its model key fails instead of switching provider. A forge
webhook signing key only authenticates deliveries; it is not an API token.

### Credential delivery

| Credential | Operator | Worker |
| --- | --- | --- |
| Model API keys | Reads the selected key to discover models | The Agent's selected key |
| Git provider tokens | Reads them to check repositories and assignment | Each provider key the Agent names |
| Kaniko push credential | Not used | Mounted into Kaniko builder Pods by Skaffold |
| Webhook signing keys | Not held; the webhook receiver reads them | Never |
| RabbitMQ | Operator connection | A projected token that reads its ServiceAccount's queues and cannot publish |
| Mailbox IMAP and SMTP | Not held | Never; mail goes through the Mailbox MCP server |
| Device keys | Not held; the Device bridge reads them | Never; commands go through the Devices MCP server |
| Kubernetes API token | Operator identity | Worker identity, when the Pod template mounts it |
| MCP tokens | Verifies Agent MCP tokens | A projected, audience-bound token per server |
| OIDC | Verifies user access tokens | Never |

### Namespace layout

Agents can each have their own namespace, share one in groups, or all live in
the harness namespace.

- **A namespace per Agent** separates Secrets, reads and network policy with
  plain RBAC, at the cost of more namespaces and per-namespace setup.
- **Several Agents per namespace**, grouped by trust level, shares that setup
  between Agents that may see each other's workloads.
- **One namespace for all Agents** is simplest. It relies on the
  `resourcePrefix` admission policy below, and every worker can still read
  the namespace's Pods, logs and ConfigMaps.

## Admission policies

Use ValidatingAdmissionPolicy and MutatingAdmissionPolicy, both stable in
Kubernetes 1.36, to enforce for worker, Kaniko and development Pods alike:

- `hostUsers: false` or a supported [Kata Containers](https://katacontainers.io/)
  runtime. Kata adds a VM and guest kernel, but does not change API
  authorization or hide credentials delivered into the guest.
- No privileged containers, host namespaces or host paths.
- For Agents sharing a namespace, `resourcePrefix`: the `agent-resource-prefix`
  policy takes the namespace's Agents as parameters and confines each worker's
  writes, Pod ServiceAccounts, Secret references and PVC mounts to resources
  named with its prefix.

## Current limits

Confine external credentials first, for example with a dedicated GitHub account
whose permissions and branch protection forbid pushing to `main`. A stolen
token can still be used within those grants. Known gaps:

- **Workers can manage broker topology.** Developer RBAC allows `rabbitmq.com`
  resources, so a worker in the broker's namespace can declare a topology
  `User` or `Permission` against it. Deny that in admission or keep workers out
  of that namespace.
- **Labels and selectors are not confined.** A worker can label its Pods into
  another Service, or point a NetworkPolicy, Ingress or autoscaler at harness
  workloads.
- **Reads are namespace-wide.** Admission does not see reads, so a developer
  worker can read every Pod spec, log and ConfigMap in its namespace.
- **Tasks of one Agent share broker access.** A worker's token reaches every
  queue of its ServiceAccount.
- **Agents may share Git credentials.** Separate review rights need a separate
  forge identity per Agent.
- **Delivered credentials leave with the code.** Malicious code can copy model
  keys, forge tokens or the registry push credential and use them elsewhere
  within their grants.

## Agent segmentation example

Three Agents separated by trust level, in any of the namespace layouts above.
[examples/](../examples/README.md) implements it.

| Boundary | Kube admin | Developer | Customer service |
| --- | --- | --- | --- |
| Kubernetes access | Cluster-admin | Cluster-wide operational reads; build and deploy in its namespace | No ServiceAccount token |
| GitHub access | Read reviewed `main` | Pull, push topic branches, manage issues, open PRs | Issues in selected support repositories |
| Model | Local model (planned) | Claude or Codex | A cheaper Claude or Codex model |
| People reach it through | Dashboard, limited to DevOps by OIDC group | GitHub issues and PR reviews | Email, GitHub issues, Slack |
| Events | Critical alerts | GitHub events, critical and warning alerts | GitHub issue events, external webhooks |
| Main risk | Sensitive-data disclosure | Supply-chain attack and privilege escalation | Prompt injection |

The developer pushes topic branches; branch protection requires human review
and reserves merging for humans, with no bypass for agents. The admin reads the
reviewed branch with a read-only token. GitHub read tokens cannot be limited to
one branch, so enforcing `main`-only reads takes a branch-only mirror.

**Kube admin** can read every namespace's Secrets, configuration and logs and
change permissions, so trust it accordingly. A local model keeps prompts away
from a hosted provider but does not stop leaks through other channels, and PR
review does not gate its direct API writes.

**Developer** runs repository code, dependencies and build scripts, starts
Kaniko Pods, pushes development images and deploys them. Its cluster-wide reads
exclude Secrets, and its writes stay in its namespace, under its prefix when
the namespace is shared. The admission policies above limit host compromise;
restrict its ServiceAccounts, Secret references and registry ACLs to contain
credential theft, image poisoning and API escalation.

**Customer service** reads email, tickets, webhook payloads and Slack messages
written by outsiders. An authenticated source does not make that text trusted,
so this worker has no Kubernetes access, source-code writes or administrative
credentials, and issues it files for the developer remain untrusted input.
Isolation limits what a fooled model can do, but not misuse of its permitted
mail and issue actions or disclosure of customer data it can read.

Mailbox Pods and the webhook receiver hold the mail and webhook credentials and
authenticate events; payloads cannot choose namespaces or grant permissions.
The customer-service worker gets issue and model credentials and its Agent MCP
token, with no Kubernetes or Driftmower token. The developer uses separate
GitHub credentials and gets no mail credentials. Separate namespaces keep these
workers from mounting each other's Secrets, but cannot constrain a cluster-admin
Agent.
