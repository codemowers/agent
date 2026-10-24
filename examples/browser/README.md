# Shared browser

`browser-mcp.yaml` runs one headless Chromium for the `matmlazi` Agent in its
development namespace `demo`, outside the harness and worker namespaces. Each
MCP session gets its own in-memory browser context in that one browser
process; a worker CLI session that reconnects starts with an empty context.

Apply it with the developer's or worker's own access to `demo`. The
`browser` MCPServer and its `use` grant to `matmlazi` are in
[../mcp/servers.yaml](../mcp/servers.yaml).

Access control is the ingress NetworkPolicy, which admits only Pods labelled
`codemowers.ai/agent: matmlazi` in `agents`. The browser has no bearer
authentication, so anyone who can create such a Pod in `agents` can use it.
Its egress reaches `demo` Pods, cluster DNS and non-private addresses, not
the harness, the Kubernetes API or other namespaces. Files the browser saves
stay in its Pod; screenshots return inline through MCP.

Chromium runs with `--no-sandbox` under the Pod's user namespace and default
seccomp profile. Do not share one Deployment between Agents or namespaces.
