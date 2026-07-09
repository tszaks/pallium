# Workflow Providers

Pallium workflows are provider-agnostic: the engine orchestrates, and any
agent CLI can do the work. Whichever model your guiding agent runs, Pallium
workers can adopt it. Codex and Claude Code both have a built-in Go
invocation (no wrapper script needed) — Pallium even auto-detects Claude
Code as the steering agent and switches workers to it with zero
configuration (see `PALLIUM_WORKFLOW.md`). Everything else plugs in through a
small wrapper script.

This directory ships reference wrappers:

| Provider | Wrapper | Notes |
|----------|---------|-------|
| Claude Code | `claude.sh` | Optional: the built-in claude provider covers the common case. Use this wrapper instead when you need its extra hardening (`--safe-mode`, `--setting-sources user`, `--strict-mcp-config`, `--permission-mode plan`) — set `PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND` to it to override the built-in path |
| Gemini CLI | `gemini.sh` | Structured output via prompt contract; see security note below |

**Security note on `gemini.sh`:** Gemini CLI runs `SessionStart` hooks from
the target repo's `.gemini/settings.json` at startup, before this wrapper's
`--approval-mode` gating ever applies, and there's currently no documented
CLI flag or env var to disable hook loading. A `worktree` doesn't help
either, since it checks out the same tracked files. Only point `gemini.sh`
at repos whose committed Gemini config you've reviewed.

## Quick start

`claude` needs no setup beyond having the CLI on PATH — Pallium's built-in
provider handles it:

```js
const review = await agent("Review the auth middleware for missing checks", {
  provider: "claude",
  mode: "read-only",
  schema: { type: "object", properties: { findings: { type: "array", items: { type: "string" } } }, required: ["findings"] },
});
```

To use `claude.sh` (or any other wrapper) instead, point the matching env var
at it — an explicitly configured command always overrides a built-in
provider:

```bash
export PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND="$(pwd)/providers/claude.sh"
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
| `PALLIUM_WORKFLOW_NETWORK` | `1` when this agent is allowed network egress (the `agent()` call passed `network: true` **and** the run was started with `--allow-network`), `0` otherwise. Default `0`. Wrappers should only expose networked tools (web fetch, `gh`, `curl`, etc.) when this is `1`; the default keeps workers sandboxed. |
| `PALLIUM_WORKFLOW_RUN_ID`, `_AGENT_ID`, `_PROVIDER`, `_LABEL`, `_REPO`, `_CWD` | Run metadata for logging or routing. |
| `PALLIUM_WORKFLOW_USAGE_FILE` | Optional (newer versions): if the wrapper writes `{"input_tokens":N,"output_tokens":N,"cost_usd":X}` here, Pallium records real usage and counts `cost_usd` toward the run budget instead of the flat per-agent estimate. |

## Bring your own model

Any model CLI can power Pallium workers — grok, gemini, moonshot, a local
Ollama model, whatever you've got a CLI for. A wrapper is the whole
integration; there's no code change and no special-casing in Pallium itself.
Minimal copy-paste starting point:

```bash
#!/bin/sh
set -eu
PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")
ARGS=""
[ "$PALLIUM_WORKFLOW_MODE" = "edit" ] && ARGS="--yolo"           # trust the model in an isolated worktree
[ "$PALLIUM_WORKFLOW_NETWORK" = "1" ] && ARGS="$ARGS --allow-net" # only when the run opted in
printf '%s' "$PROMPT" | your-model-cli $ARGS > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
```

Save it, `chmod +x`, then point Pallium at it:

```bash
export PALLIUM_WORKFLOW_PROVIDER_GROK_COMMAND="$(pwd)/providers/grok.sh"
```

Now `agent("...", { provider: "grok" })` runs on Grok. Same pattern for any
other CLI — swap the binary and its flags for mode/network mapping.

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

## Extra contract for teammates (`pallium team ...`)

A `pallium team` teammate is a persistent identity whose life is a SERIES of
one-shot CLI calls, not a single request/response — each turn must resume the
same native conversation your CLI already tracks, so it remembers what it
said and did in earlier turns. Your wrapper needs exactly one more file:

- `PALLIUM_WORKFLOW_SESSION_FILE` — read it at the start of your turn. Empty
  means this is the teammate's first turn ever; anything else is the resume
  handle YOU wrote on a previous turn. Before you exit, OVERWRITE this file
  with whatever session/thread id your CLI needs to resume this exact
  conversation next time (your CLI's own concept — a session id, a thread id,
  whatever it calls it). Pallium never inspects the value; it just shuttles it
  between turns.

```bash
#!/bin/sh
set -eu
PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")
SESSION=$(cat "$PALLIUM_WORKFLOW_SESSION_FILE" 2>/dev/null || true)
if [ -n "$SESSION" ]; then
  printf '%s' "$PROMPT" | your-model-cli --resume "$SESSION" --output-session-id > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
else
  printf '%s' "$PROMPT" | your-model-cli --new-session --output-session-id > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
fi
# Whatever your CLI printed as its session id, write it back for the next turn:
your-model-cli last-session-id > "$PALLIUM_WORKFLOW_SESSION_FILE"
```

The rest of the contract (prompt/output/schema files, mode/network mapping,
exit codes) is identical to a regular worker — a team-capable wrapper is a
regular wrapper plus this one file.
