# Pallium Workflow - Ultracode Parity Guide

For any agent (Claude, Codex, Cursor, custom) writing Pallium dynamic workflows.

## Mental model

You are the conductor. The workflow script is the score. Subagents are the players.

Pallium runs async JavaScript locally, spawns provider-backed workers, persists run state in SQLite, and returns structured artifacts. The goal is **Claude Ultracode-shaped orchestration** with **repo-native Pallium primitives** on top.

## Run a workflow

```bash
pallium workflow run --script workflow.js "task description" --json
pallium workflow run --background --script workflow.js "long task"
pallium workflow resume <run-id>
pallium workflow inspect <run-id> --json
pallium workflow report <run-id> --json
pallium workflow gc --older-than 7 --dry-run
```

Discover primitives first:

```bash
pallium workflow tools list --json
pallium workflow template list --json
pallium workflow validate workflow.js
```

## Script shape

### `meta` block

Start every script with a pure-literal block (no spreads, no function calls):

```js
export const meta = {
  name: "security-audit",
  description: "Parallel security review with adversarial verify",
  phases: ["scope", "audit", "synthesize"],
};
```

Pallium strips `meta` before execution. Phase names should match `phase()` calls.

### Execution context

- Async JavaScript only (not TypeScript)
- Top-level `await` supported
- `args` - value passed via `--args` JSON
- **Determinism guards:** `Date.now()`, `Math.random()`, and argless `new Date()` throw. Pass timestamps/randomness through `args` so resume cache stays sound.

## Core primitives

### `await agent(prompt, opts?)`

Spawns one worker. Default provider is Codex; set `provider` for others.

```js
const finding = await agent("Review auth middleware", {
  label: "auth-review",
  mode: "read-only",
  schema: {
    type: "object",
    properties: {
      findings: { type: "array", items: { type: "string" } },
    },
    required: ["findings"],
  },
});
```

| Opt | Ultracode | Pallium |
|-----|-----------|---------|
| `label` | display name | same |
| `phase` | progress group | use `phase()` instead |
| `model` | model id | same (Codex `--model`) |
| `effort` | low to max | **not yet** - use prompt/provider |
| `isolation: "worktree"` | edit isolation | same |
| `agentType` | named agent | use `provider` |
| `schema` | StructuredOutput | Codex `--output-schema`; providers get schema file, Pallium validates returned JSON locally and, for read-only agents only, retries once with a corrective prompt before failing the agent. Edit/test/check agents fail schema validation immediately: the retry re-runs the full provider command in the same cwd, which could apply side effects twice |
| `timeout_seconds` | - | per-call wall-clock cap; overrides `--agent-timeout` (`0` disables) |

Edit and worktree-isolated agents run in a detached git worktree under
`~/.pallium/workflow-runs/<run-id>/worktrees/`. The worktree is removed as soon
as the agent's patch is captured — the patch file is the durable artifact — and
kept only when patch capture fails, for debugging. `pallium workflow gc
[--older-than days] [--include-failed] [--dry-run]` removes the artifact
directories of completed/stopped runs older than N days (default 7), reports
the count and bytes freed, and prunes stale git worktree metadata. Failed runs
are resumable, so their workflow.js and patches are kept unless
`--include-failed` is passed.

Non-Codex providers: `PALLIUM_WORKFLOW_PROVIDER_<NAME>_COMMAND`

Provider commands receive `PALLIUM_WORKFLOW_PROMPT_FILE`, `PALLIUM_WORKFLOW_OUTPUT_FILE`, `PALLIUM_WORKFLOW_SCHEMA_FILE`, and `PALLIUM_WORKFLOW_USAGE_FILE`. A provider may write `{"input_tokens":N,"output_tokens":N,"cost_usd":X}` to the usage file; the reported `cost_usd` replaces the flat per-agent estimate for that agent (including budget accounting) and the raw JSON is persisted on the agent record as `usage_json`. The usage file is read (and removed) after each provider invocation, so when the corrective schema retry runs, `cost_usd` and token counts are summed across both attempts. Unreadable or absent usage files are ignored.

### `await pipeline(items, stage1, stage2, ...)`

**Default for multi-stage work.** Pallium matches Ultracode streaming semantics:

- Each item flows through stages **independently**
- Fast items do not wait behind slow items at a stage barrier
- Stage callback: `(prevResult, originalItem, index)`
- Stage throw -> that item becomes `null`, others continue

Dropped items are never silent: every item that `parallel`/`pipeline` converts
to `null` (failed agent, stage throw) is logged to stderr
(`[workflow:<run-id>] dropped <label>: <error>`) and recorded in the run-level
`failures` list surfaced by `workflow read`, `workflow inspect`, and
`workflow report`.

```js
const verified = await pipeline(findings,
  f => agent(`Find issues: ${f.path}`, { label: "find-" + f.path, schema: FINDING_SCHEMA }),
  review => agent(`Refute: ${JSON.stringify(review)}`, { label: "verify-" + review.id, schema: VERDICT_SCHEMA }),
);
```

### `await parallel(itemsOrThunks, fn?)`

Barrier concurrency - waits for **all** items before returning.

Use only when the next step needs the full set (merge, dedupe, judge, early-exit on total count).

```js
const angles = ["security", "perf", "correctness"];
const reports = await parallel(angles, angle =>
  agent(`Review from ${angle}`, { label: angle }),
);
const merged = await agent(`Synthesize: ${JSON.stringify(reports)}`, { label: "synth" });
```

**Discipline:** if code between two `parallel()` calls is pure data plumbing (flat/map/filter), fold it into a `pipeline` stage instead.

### `phase(title, callback?)` / `log(msg)`

Progress grouping and stderr narration (`[workflow:<run-id>] ...`).

### `await workflow(name, args?)`

Compose a saved workflow from `.pallium/workflows/`. **One nesting level only.**

### `await gate(name, prompt, options?)`

Runs an autonomous verifier agent before the workflow continues:

```js
const verdict = await gate("patch-safety", "Verify generated patches before apply", {
  criteria: "tests pass, no secrets are introduced, and scope matches the task",
});
```

The verifier returns structured JSON with `approved` and `reason`. Rejected
gates fail the workflow by default. Set `fail_on_deny: false` when the script
should handle rejection itself.

### `budget`

Ultracode uses token budgets. Pallium exposes the same **shape** over local USD estimates:

```js
if (budget.total !== null && budget.remaining() < 0.01) {
  log("skipping deep verify: budget nearly exhausted");
}
```

Set ceiling with `--max-budget-usd` and per-agent estimate with `PALLIUM_WORKFLOW_AGENT_COST_USD`. Configured providers that report real usage through `PALLIUM_WORKFLOW_USAGE_FILE` replace the flat estimate with the reported `cost_usd`. Further `agent()` calls throw when exhausted. Agent and budget caps are lifetime run limits, so resume does not reset them.

## Pallium Extras

```js
const preflight = await pallium.preflight(task, "cmd/workflow.go");
const changed = await pallium.changedNow();
const safe = await pallium.safe(path);
const green = await verify.untilGreen("go test ./...", { maxRounds: 3, label: "tests" });
await pallium.decisions.record("Chose worktrees", "Edit agents stay isolated.", "workflow");
const plan = await coordinator.replan("adapt after verifier findings", { label: "coordinator" });
```

### `await verify.untilGreen(command, options?)`

Owns the check -> fix -> re-check loop. The first check runs in the original
cwd; if it is already green the loop finishes with no worktree and no patch.
Once a fix round is needed, the invocation gets **one persistent worktree**
for the rest of the loop: the fix agents and every later check run inside it,
so each check round sees the previous fixes immediately. The combined diff is
written to a durable patch file after every fix round, so an interrupted loop
restores that progress into a fresh worktree on resume. When the loop ends —
green, stalled, max rounds, or a mid-loop agent error — the final combined
diff is captured; on a clean end it is registered like a normal edit-agent
patch (auto-applies when the run completes and participates in `workflow
apply`/`revert`). The loop worktree is always removed (kept only if patch
capture fails, for debugging).

Options: `maxRounds` (default 3), `label`, `provider` (used for both check and
fix agents; a provider command can branch on `PALLIUM_WORKFLOW_MODE` — `test`
for check, `edit` for fix), `model`, and `fix_model`.

Returns `{ ok, command, rounds, stalled }`. The loop stops early when two
consecutive check rounds fail identically (stall detection) or when
`maxRounds` is exhausted; the patch is still captured so partial fixes are not
lost.

## Limits

| Limit | Value |
|-------|-------|
| Concurrent agents | `--max-concurrent-agents` (default 16) |
| Lifetime agents | `--max-agents` (default 1000) |
| Per-agent wall clock | `--agent-timeout` seconds (default 600, `0` disables); stored on the run and reused by `resume` unless the flag is passed again |
| Items per `parallel`/`pipeline` | 4096 |
| Nested `workflow()` | 1 level |

A timed-out agent fails with `workflow agent timed out after Ns`: it becomes `null` inside `parallel`/`pipeline` (recorded in the run `failures` list) and throws for a direct `agent()` call, so a hung worker can never stall the run.

## Resume and caching

Completed `agent()` calls reuse cache by deterministic call position plus prompt, provider, repo, mode, model, schema, and args hash.

What that means:

- Two identical `agent()` calls in the same execution are separate calls and both run.
- Resuming a run replays the script from the top and reuses matching completed call positions.
- Editing the tail of a script keeps the unchanged prefix cached.
- Changing args, model, provider, repo, mode, label, prompt, or schema invalidates the affected call.
- Resuming with an edited script warns on stderr (`script changed since original run; unchanged prefix will replay from cache`) and sets `script_changed: true` on the run snapshot and `workflow inspect` output.

```bash
pallium workflow resume <run-id>
```

Inspect actual worker output before assuming cache behavior:

```bash
pallium workflow show <run-id> --json
pallium workflow inspect <run-id> --json
```

## Quality patterns (compose in script)

These are **patterns**, not built-ins. Match harness to task:

| Pattern | Shape |
|---------|-------|
| Adversarial verify | `pipeline(findings, f => agent(skeptic))` + majority vote in plain JS |
| Perspective-diverse verify | Different verifier prompts per lens |
| Judge panel | `parallel(angles, ...)` -> `agent(judge)` -> `agent(synthesize)` |
| Loop-until-dry | `while` + dedup against **all seen**, not just confirmed |
| Multi-modal sweep | `parallel(modalities, ...)` |
| Completeness critic | Final `agent("what's missing?")` -> next round |
| No silent caps | `log("dropped", n, "items after cap")` |

## Ultracode Vs Pallium

| Ultracode | Pallium today |
|-----------|---------------|
| Native Workflow tool in-session | CLI / MCP / HTTP (`pallium workflow run`) |
| Token `budget` | USD-shaped `budget` object |
| `effort`, `agentType` | `provider`, `model`, `mode` |
| Agent death -> `null` | Parallel/pipeline agent failure -> `null` plus a run-level `failures` entry; direct `agent()` failure throws |
| `journal.jsonl` in transcript dir | SQLite store + `workflow show/inspect` |
| MCP via ToolSearch in headless workers | Provider must bundle tools; Pallium exposes repo via `pallium.*` |
| Permission dialog from `meta` | `meta` for naming/phases; gates run verifier agents |

## Proof gate

```bash
bash scripts/workflow-verify.sh
pallium workflow audit --run-acceptance
```

## Example: adversarial review

```js
export const meta = {
  name: "adversarial-review",
  description: "Parallel find + per-finding skeptic verify",
  phases: ["scope", "find", "verify", "synthesize"],
};

const task = args?.task ?? "Review this change set";
phase("scope");
const preflight = await pallium.preflight(task);

phase("find");
const files = (preflight.files_to_inspect ?? []).slice(0, 20);
log("inspecting", files.length, "files");
const findings = (await pipeline(files,
  file => agent(`Find concrete issues in ${file}. Task: ${task}`, {
    label: "find-" + file,
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        file: { type: "string" },
        findings: { type: "array", items: { type: "string" } },
      },
      required: ["file", "findings"],
    },
  }),
)).flatMap(r => (r?.findings ?? []).map(text => ({ file: r.file, text })));

phase("verify");
const surviving = (await pipeline(findings,
  f => agent(`Try to refute this finding. Default to refuted if uncertain.\n${JSON.stringify(f)}`, {
    label: "skeptic-" + f.file,
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        verdict: { type: "string", enum: ["confirmed", "refuted"] },
        reason: { type: "string" },
      },
      required: ["verdict", "reason"],
    },
  }),
)).filter(r => r?.verdict === "confirmed");

phase("synthesize");
return await agent(`Synthesize confirmed findings into next steps.\n${JSON.stringify(surviving)}`, {
  label: "synthesize",
  mode: "read-only",
});
```
