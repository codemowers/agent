# Shared knowledge and drafts: proposed API

These are design examples, not currently supported resources. The Knowledge and
Draft CRDs, Agent selectors and corresponding MCP tools are not implemented.
Do not include these files in installation or bootstrap apply commands.

- [Knowledge example](optional/containerd-apparmor-signals.yaml): an established
  finding or procedure, with references and optional expiry.
- [Draft example](../drafts/optional/shared-agent-knowledge.yaml): the evolving
  design for shared knowledge and drafts, including unresolved questions.

Use `Draft` for an evolving idea or design. `Proposal` is represented by the
Draft's proposal text and lifecycle phase rather than another resource kind.
A Draft can progress from idea to ready, accepted, rejected or implemented;
selecting it or accepting its design does not authorize an implementation Task.

Both resources are namespaced and can be shared by Agents in that namespace.
Keep them independent of Agent and Task owner references. Knowledge is for
established findings; a Draft records what might be built.

## Proposed Agent selectors

This is a fragment of the proposed Agent spec, not an applyable Agent manifest:

```yaml
spec:
  knowledgeSelector:
    matchLabels:
      topic: kubernetes
    matchExpressions:
      - key: region
        operator: In
        values: [ee-lte-1, shared]
  draftSelector:
    matchLabels:
      topic: agent-platform
```

An omitted selector selects nothing; `{}` selects all entries of that kind in
the Agent's namespace. Selectors do not grant permissions or cross namespace
boundaries. Full content is retrieved on demand; only bounded titles and
summaries belong in initial context.

Proposed Knowledge fields are required title, summary and content, plus optional
references and validUntil. Content is bounded to 32 KiB. Status provenance is
assigned by the operator from authenticated Agent and Task identity, not from
an agent-supplied claim. Updates use resourceVersion to avoid lost edits.

The existing bundled UI product drafts remain unchanged by these examples.
