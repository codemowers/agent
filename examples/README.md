# Examples

These manifests run the Agents of the [segmentation
example](../docs/security-model.md#agent-segmentation-example). The Agents,
their Tasks, bindings, Mailboxes and the `hifiberry` Device live in namespace
`agents`, the harness in `codemowers-agent-system`, and `matmlazi` deploys
into `demo`.

| Agent | Profile | Worker access in `rbac.yaml` |
| --- | --- | --- |
| `krtek` | Kube admin | Cluster-admin |
| `matmlazi` | Developer | Namespace workloads without Secrets; `get` on the Kaniko push Secret |
| `patpalouk` | Customer service | None; API token mount disabled |
| `laurivosandi` | Personal Gitea agent | None |

No Agent sets `resourcePrefix`, so plain RBAC decides what each worker may
change. Set it to confine a worker's writes through the `agent-resource-prefix`
admission policy.

## Apply

Files under `*/optional/` are templates with placeholder IDs and URLs.

1. CRDs with `kubectl apply --server-side -f ../crd/`, then `rbac.yaml` and
   `agents/`.
2. `tasks/fallbacks.yaml`, and optionally `tasks/krtek-capacity-review.yaml`.
   Creating a Task starts a worker turn.
3. `webhooks/protocol-*.yaml`, the receivers you use, then their bindings and
   policies. Set each protocol's `allowedKeyPrefixes` to your hosts and point
   each forge hook at its receiver's path.
4. `mail/rbac.yaml`, then the Mailboxes and MailBinding.
5. `devices/hifiberry.yaml`.
6. Optionally `browser/browser-mcp.yaml` in `demo`; see
   [browser/README.md](browser/README.md).

Outside these files:

- `agents` needs the `agent-operator-provider-tokens` and
  `agent-operator-webhook-keys` Secrets, and `demo` the
  `zot-registry-cluster-push-kaniko` Secret for Kaniko builds.
- Slack needs the app's signing secret as key `slack-codemowers` in
  `agent-operator-webhook-keys`. A Slack API token cannot verify events.
- Gitea assignment policies need a Gitea API token in
  `agent-operator-provider-tokens` and a matching `gitProviders` entry.
- Configure senders and `device-agent` with the values the dashboard shows.

`task-quota.yaml` caps the namespace at 1000 Tasks and alerts before that.
`task-maintain-agent.yaml` is a long-running maintenance Task for this
repository.
