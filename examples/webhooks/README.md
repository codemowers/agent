# Webhooks

- A receiver is the endpoint and holds the credential: a ClusterWebhookReceiver
  (`/hooks/<name>`) for one org or instance shared by many Agents, or a
  WebhookReceiver (`/hooks/<namespace>/<name>`) for one namespace. Bindings
  hold no key; they name a receiver and the event keys they want.
- Event keys are canonical resource URIs, such as
  `https://github.com/owner/repo/issues/1`; pull requests use the issue URL and
  other repository events the repository URL. Binding `keyPrefixes` select a
  hierarchy at path boundaries: `https://github.com/codemowers` for an org,
  `https://github.com/codemowers/agent` for one repository. A protocol's
  `allowedKeyPrefixes` bounds the keys its events and binding `keyCEL` may
  produce. Webhook and mail events for the same URI reach the same Task.
- CEL sees `rawBody`, `body`, lowercase `headers` and `receiver.match`, plus
  `urlHost(string)`, `sha256Hex(bytes)` and the cel-go string extensions such
  as `lowerAscii()`. It cannot choose a namespace, Agent, Task or credential.
- Anyone with a receiver's secret can forge its deliveries; GitHub and Gitea
  sign neither headers nor a timestamp. `githubMeta` or `cidrs` add a
  source-address check, which needs `webhook.trustedProxies` set to the
  ingress proxies.
- Alertmanager authenticates with a projected token for the receiver's
  `audience`, checked by TokenReview against its `serviceAccounts`. It needs
  egress to the webhook Service's port 8080: Cilium `ipBlock` rules such as
  `::/0` do not match in-cluster Pods.
- A StringSecret can add a key to `agent-operator-webhook-keys` but cannot
  adopt the existing Secret. Keep its other keys.
- Slack bot posts, including Alertmanager notifications, are dropped. A human
  reply routes by its thread URL and reaches the Slack fallback Task until
  another Task claims that key.
