# Mail

- Mailbox Pods run as the `agent-mailbox` ServiceAccount from `rbac.yaml`;
  apply it first.
- Workers get a Mailbox through RBAC verbs `send` and `read` on it. The
  operator attaches Mailboxes when it creates a worker Pod, so RBAC changes
  apply from the next worker Pod. `agentRef` pins a Mailbox to one Agent even
  when broader RBAC, such as Krtek's cluster-admin, grants others access;
  without it, RBAC alone lets several Agents share it.
- The `mail-intake` fallback Task must exist before routing starts.
- `mailbinding-matmlazi-github.yaml` trusts `Authentication-Results` only
  from authserv-id `mail.codemowers.io`. That holds because Haraka strips
  sender-supplied results (`headers.clean_auth_results`, its default). Other
  mail servers need the same boundary before a rule relies on that header.
- Run one IMAP reader per account.
