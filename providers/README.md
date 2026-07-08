# Workflow Providers

Pallium workflows are provider-agnostic: the engine orchestrates, and any
agent CLI can do the work. Whichever model your guiding agent runs, Pallium
workers can adopt it. Codex is the built-in default; everything else plugs in
through a small wrapper script.

This directory ships reference wrappers:

| Provider | Wrapper | Notes |
|----------|---------|-------|
| Claude Code | `claude.sh` | Structured output, mode-scoped permissions, token/cost reporting |
| Gemini CLI | `gemini.sh` | Structured output via prompt contract; see security note below |

**Security note on `gemini.sh`:** Gemini CLI runs `SessionStart` hooks from
the target repo's `.gemini/settings.json` at startup, before this wrapper's
`--approval-mode` gating ever applies, and there's currently no documented
CLI flag or env var to disable hook loading. A `worktree` doesn't help
either, since it checks out the same tracked files. Only point `gemini.sh`
at repos whose committed Gemini config you've reviewed.

## Quick start

```bash
export PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND="$(pwd)/providers/claude.sh"
```

```js
const review = await agent("Review the auth middleware for missing checks", {
  provider: "claude",
  mode: "read-only",
  schema: { type: "object", properties: { findings: { type: "array", items: { type: "string" } } }, required: ["findings"] },
});
```

Provider names map to env vars as `PALLIUM_WORKFLOW_PROVIDER_<UPPER_SNAKE>_COMMAND`.
The command runs via `sh -c` with the agent workspace as the working directory
(an isolated git worktree for `edit`-mode agents).

## The environment contract

Pallium sets these variables for every provider invocation:

| Variable | Meaning |
|----------|---------|
| `PALLIUM_WORKFLOW_PROMPT_FILE` | Path to a file containing the full prompt. Prefer this over `PALLIUM_WORKFLOW_PROMPT` (also set) to avoid shell-length limits. |
| `PALLIUM_WORKFLOW_OUTPUT_FILE` | Where the wrapper must write the agent's result. If empty or unwritten, Pallium falls back to the command's stdout; empty output is an error. |
| `PALLIUM_WORKFLOW_SCHEMA_FILE` | Path to a JSON Schema when the call requested structured output; empty otherwise. Pallium validates the returned JSON against it. |
| `PALLIUM_WORKFLOW_MODE` | `read-only`, `edit`, `test`, or `check`. Wrappers are responsible for honoring it (e.g. restricting tools for `read-only`). |
| `PALLIUM_WORKFLOW_MODEL` | Model override from the `agent()` call, empty if none. |
| `PALLIUM_WORKFLOW_RUN_ID`, `_AGENT_ID`, `_PROVIDER`, `_LABEL`, `_REPO`, `_CWD` | Run metadata for logging or routing. |
| `PALLIUM_WORKFLOW_USAGE_FILE` | Optional (newer versions): if the wrapper writes `{"input_tokens":N,"output_tokens":N,"cost_usd":X}` here, Pallium records real usage and counts `cost_usd` toward the run budget instead of the flat per-agent estimate. |

## Writing your own wrapper

1. Read the prompt from `PALLIUM_WORKFLOW_PROMPT_FILE`.
2. If `PALLIUM_WORKFLOW_SCHEMA_FILE` is non-empty, instruct your model to emit
   bare JSON conforming to the schema, and strip fences/prose before writing
   the output (see the reference wrappers for a robust extraction snippet).
3. Map `PALLIUM_WORKFLOW_MODE` onto your CLI's permission model. `edit` agents
   run inside an isolated worktree, so granting file writes there is safe;
   `read-only` agents should not be able to modify anything.
4. Write the result to `PALLIUM_WORKFLOW_OUTPUT_FILE`.
5. Exit non-zero on failure. Inside `parallel()`/`pipeline()` a failed agent
   becomes `null` and siblings continue; a direct `agent()` call surfaces the
   error to the script.

Keep wrappers dependency-light (shell + python3 is plenty) and never echo
secrets: the wrapper's environment is the workflow's trust boundary.
