# Task specification

Architecture refactor

Run controller suite per cluster in its own namespace
* Branch out scalable webhook handler into Go binary, include best effort key routing (email thread ID, GitHub issues etc)
* Branch out web UI and make it optional
* Adopt RabbitMQ or similar for webhook submission, persistence and deduplication
* Controller runs Deployment which consumes events from RabbitMQ, evaluates agent state and figures out what to do next (add Task, remove Task, abort Task) and when worker pod is up and running submits prompts to it from message queue

Declarative Agent instances via Kube native CRD-s:
* Agent represents privilege domain, which MCP-s are enabled and which credentials are accessible
* Agent state is tracked in Agent CRD .status field
* Task CRD tracks Worker state, what model is used, should worker be running or off etc
* Each agent shall have a Kubernetes namespace for it's workers
* Agent CRD namespaced declaration lives in the workers namespace. You can have multiple Agents spawn its workers in same namespace if security domain aligns
* Worker pod has it's ownerReferences pointint to Task CR, thus deleting Task CR deletes all pods assocated with it
* Task CRD has ownerReferences pointing to the Agent CR, thus deleting Agent deletes all Tasks associated with it
* Workspace PVC it's ownerReference pointing to the Task CR. Deleting Task CR deletes the workspace PVC associated with it

Open ended questions:
* How are secrets handled? In web UI I should be able to set up GH, SMTP, IMAP tokens, ideally mutating existing secret? In the workers namespace? Those should be easily configurable with kubectl as well.

Come up with admin vs developer vs customer support Kube RBAC and Agent CR scheme which would satisfy the requirements described in the README. Factor in one more scenario where Agent is for personal mailbox, reads mail and populates calendar.

Make sure the setup covers multiple Giteas, multiple Githubs (GitHub enterprise), multiple Agents CR-s per cluster, multiple Agents CR-s per namespace. Orchestration logic keep in one namespace

Don't execute before confirmation, start by commenting how the Agent, Task CR-s would look like and how the RBAC would be organized without dumping RBAC YAML
