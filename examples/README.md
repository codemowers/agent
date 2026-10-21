# Examples

The Agents implement the README's segmentation example. Agents, their workers,
Tasks, bindings and Mailbox share the namespace `agents`; the harness and the
`blueos` Device stay in `codemowers-agent-system`. `matmlazi` deploys into the
workload namespace `demo`:

| Agent | Profile | Worker access in `rbac.yaml` |
| --- | --- | --- |
| `krtek` | Kube admin | Cluster-admin |
| `matmlazi` | Developer | Namespace workloads without Secrets; `get` on the Kaniko push Secret |
| `patpalouk` | Customer service | None; API token mount disabled |
| `laurivosandi` | Personal Gitea agent | None |

None of the Agents sets `resourcePrefix`, so plain RBAC decides what each
worker may change; set it to confine a worker's writes through the cluster's
`agent-resource-prefix` admission policy. `agents` holds the
`agent-operator-provider-tokens` and `agent-operator-webhook-keys` Secrets.
Kaniko builds use the `zot-registry-cluster-push-kaniko` Secret in `demo`.

Apply with the context and namespace from `skaffold.env`. Files under
`*/optional/` are templates with placeholder IDs and URLs.

1. `crd/codemowers.cloud_*.yaml` with `kubectl apply --server-side`, then
   `rbac.yaml` and `agents/`.
2. `tasks/fallbacks.yaml`, and optionally the durable `tasks/krtek-capacity-review.yaml`.
   Creating a Task starts a worker turn.
3. `webhooks/protocol-*.yaml`, the `clusterreceiver-*.yaml` and
   `receiver-*.yaml` you use, then their `binding-*.yaml` and `policy-*.yaml`.
   Set each protocol's `allowedKeyPrefixes` to your hosts, and point each
   forge hook at its receiver's path.
4. `mail/rbac.yaml`, then the Mailboxes and MailBinding.
5. `devices/blueos.yaml`.

Steps outside these files:

- Slack needs the app's signing secret as key `slack-codemowers` in
  `agent-operator-webhook-keys`. `mat-slack-token` holds API tokens and cannot
  verify events.
- Gitea assignment policies need a Gitea API token in
  `agent-operator-provider-tokens` and a matching `gitProviders` entry. The
  generated webhook signing key is not an API token.
- Configure senders and `device-agent` with the values the UI shows.
