# Model and reasoning routing

Pallium selects provider, model, and reasoning effort together. The steering agent supplies a task class; a versioned policy selects an eligible configuration without another model call. Explicit model or effort pins win. Provider restrictions, worker permissions, and availability constrain selection.

## Use

```sh
pallium route models init
pallium route models catalog
pallium route models explain --task-class bounded-edit
pallium route models history --run RUN_ID
```

The policy lives at `.pallium/routing.json`, overridden by `PALLIUM_ROUTING_CONFIG`. Initialization creates a shadow policy and never overwrites an existing file. Shadow records a recommendation while preserving execution. Set `mode` to `auto` to apply recommendations, or `off` to disable selection. Add rules such as `"rules": {"bounded-edit": "luna-xhigh"}` after validating that configuration for your workload. The conservative default is `astra-high`; the starter has no task-family winner claims.

```js
return await agent("Implement the bounded change", {
  task_class: "bounded-edit",
  mode: "edit"
});
// An explicit pair bypasses automatic selection:
// { model: "gpt-5.6-luna", reasoning_effort: "xhigh" }
```

Effort is supported by agents, checks, gates, and team members, validated for the provider, transported to Codex/Claude, and persisted. Team CLI spawning accepts `--reasoning-effort`. Effort participates in cache identity; policy changes invalidate agent-gate approval. Native team sessions keep their selected configuration and recheck provider policy on dispatch.

Optional `escalations` maps task classes to candidate-ID lists (at most three). `untilGreen` advances the fix configuration only after verification failures, subject to its existing round and budget limits. Explicit pins still win. Missing providers do not authorize crossing the allowed-provider boundary.

## Observability and evidence

Workflow inspection and model-route history expose per-invocation configuration, duration, status, and usage. Codex completed-turn events provide token counts; token counts alone do not establish dollar charges. Unknown cost stays null, and retry costs include failed attempts. Configuration records describe settings sent to the provider, not an independently verified server-side model identity.

The implementation has adapter, policy, persistence, and integration tests. A live Auto smoke selected Luna xhigh and returned the expected response. That proves dispatch wiring, not task quality or savings. A larger source-reading trial timed out; it is not evidence that a cheaper configuration is superior. Auto remains opt-in until workload evidence supports a default change.

- [Evidence catalog](catalog.md): dated primary-source research and provisional candidates.
- [Evaluation protocol](evaluation.md): scoring, accounting, isolation, and promotion criteria.
- [Evaluation harness](../../eval/routing/README.md): fixture preparation, bounded execution, grading, reports, and shadow-policy proposals.
- [Task specifications](tasks.jsonl): 24 historical-source specifications, split 12/12 by related task groups. Runtime preparation materializes and hashes fixtures; the checked-in specifications are not measured results.

## Provider boundaries

A host's native delegation choices may be limited to one lab. Pallium can invoke another installed and authenticated provider CLI when explicit policy permits. Installation alone does not prove authentication. Provider-native sessions are not portable across labs. The starter and evaluation runner use Codex for the initial Astra/Luna comparison; the runtime also supports Claude and configured wrappers. Unsupported reasoning settings fail explicitly.
