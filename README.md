# Pallium

[![npm version](https://img.shields.io/npm/v/pallium.svg)](https://www.npmjs.com/package/pallium)
[![npm downloads](https://img.shields.io/npm/dm/pallium.svg)](https://www.npmjs.com/package/pallium)
[![GitHub release](https://img.shields.io/github/v/release/tszaks/pallium?sort=semver)](https://github.com/tszaks/pallium/releases/latest)
[![GitHub Packages](https://img.shields.io/badge/GitHub%20Packages-%40tszaks%2Fpallium-24292f?logo=github)](https://github.com/tszaks/pallium/pkgs/npm/pallium)

Pallium is a local-first control plane for AI coding agents.

It gives agents repo memory, risk context, verification history, session recall,
and Claude-shaped dynamic workflows that can run independently of the main chat.

The core idea is simple: keep orchestration, state, and verification outside the
model context so agents can do larger tasks without losing the thread.

## What Pallium Does

Pallium has three main jobs:

1. Build repo context before an edit.
2. Remember prior agent sessions and decisions.
3. Run auditable, resumable, multi-agent workflows.

Use it when an agent needs to know:

- which files are risky
- which files usually move together
- what changed in the working tree
- which tests matter first
- what verification recently passed or failed
- whether a task drifted outside its planned scope
- what prior sessions or decisions explain the current work
- how to hand work off cleanly to another agent or human

Most user-facing commands support `--json` where agent parsing matters.

## Quick Start

```bash
pallium index
pallium doctor
pallium changed-now --json
pallium explain cmd/workflow.go --json
pallium safe internal/workflow/runtime.go --json
pallium plan internal/workflow/runtime.go --json
pallium verify fast --json
pallium handoff origin/main --json
```

For a dynamic workflow:

```bash
pallium workflow tools list --json
pallium workflow template list --json
pallium workflow preflight "review workflow changes" --scope internal/workflow --json
pallium workflow run "review workflow changes"
```

For a generated workflow script:

```bash
pallium workflow generate "fix tests until green" \
  --style test-fix \
  --test-command "go test ./..." \
  --output fix.workflow.js

pallium workflow validate fix.workflow.js
pallium workflow run --script fix.workflow.js "fix tests until green"
```

## Dynamic Workflows

`pallium workflow` is Pallium's local dynamic workflow runtime. It is inspired by
Claude Code dynamic workflows, but it is agent-agnostic and grounded in Pallium's
repo memory.

Agents should read `PALLIUM_WORKFLOW.md` before writing custom workflow scripts.

A workflow is an async JavaScript script executed by Pallium's Go runtime. The
script coordinates workers through primitives such as:

- `phase(name, fn?)`
- `await agent(prompt, options)`
- `await parallel(items, fn)`
- `await pipeline(items, stage1, stage2, ...)`
- `await check(command, options)`
- `await verify.untilGreen(command, options)`
- `await workflow(savedName, args)`
- `await gate(name, prompt, options)`
- `await coordinator.replan(goal, options)`
- `await pallium.preflight(task, ...scopes)`
- `await pallium.decisions.record(title, body, ...tags)`
- `await pallium.decisions.search(query, limit)`

The script owns orchestration. Workers own thinking, shell use, and edits.

Pallium persists each run in SQLite and stores artifacts under
`~/.pallium/workflow-runs/`. Runs have stable ids, phases, workers, outputs,
patches, reports, gates, and status.

### Example Workflow

```js
phase("scope");
const preflight = await pallium.preflight(
  "review workflow runtime changes",
  "internal/workflow",
  "cmd/workflow.go"
);

phase("inspect");
const findings = await parallel(preflight.files_to_inspect || [], file =>
  agent("Review " + file + " for correctness risks", {
    label: "review-" + file,
    mode: "read-only"
  })
);

phase("verify");
const verification = await verify.untilGreen("go test ./...", {
  label: "tests",
  maxRounds: 3
});

return { findings, verification };
```

## How It Differs From A Normal Agent Loop

Normal agent work keeps planning, state, verification output, and next steps in
one chat context. That works for small tasks, then gets noisy.

Pallium workflows move that control flow into executable JavaScript:

- loops are normal code
- intermediate state lives in variables and SQLite
- workers can run in parallel
- completed workers are cached on resume
- tests are treated as external checks
- reports and patches are inspectable after the run
- gates, triggers, API calls, and MCP clients can continue work outside chat

This is the part that makes it feel close to Claude-style dynamic workflows:
the model can author a script, Pallium runs the script, and subagents carry out
the work under a tracked runtime.

`pipeline(items, stage1, stage2, ...)` uses Ultracode-style streaming stages:
each item moves to its next stage as soon as its prior stage completes. A slow
item does not hold faster items behind a stage-wide barrier.

## Agent Workers

The default worker provider is Codex:

```js
await agent("Inspect auth routes", {
  label: "auth-review",
  mode: "read-only"
});
```

Worker modes:

- `read-only`: inspect code with a read-only sandbox
- `test`: run verification commands and summarize failures
- `edit`: make changes in an isolated worktree and produce a patch

Edit workers do not edit the target checkout directly. They run in git
worktrees under `~/.pallium/workflow-runs/`, then Pallium applies patches back
to the target repo only after the workflow completes successfully.

Before applying a patch, Pallium scans added lines for common secret patterns
such as API keys, tokens, passwords, OpenAI-style keys, and AWS access keys. A
matching patch is blocked unless `PALLIUM_WORKFLOW_ALLOW_SECRET_PATCH=1` is set.

Use `workflow revert <run-id>` to reverse workflow patches after they have been
applied.

## Any Agent, Any Model

Pallium is not limited to Codex. A workflow agent can target any configured
provider:

```js
await agent("Review this design decision", {
  label: "claude-review",
  provider: "claude-code",
  mode: "read-only"
});
```

Configure providers with environment variables:

```bash
export PALLIUM_WORKFLOW_PROVIDER_CLAUDE_CODE_COMMAND='claude -p "$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"'
```

Provider commands run through `sh -c` in the worker cwd and receive:

- `PALLIUM_WORKFLOW_RUN_ID`
- `PALLIUM_WORKFLOW_AGENT_ID`
- `PALLIUM_WORKFLOW_PROVIDER`
- `PALLIUM_WORKFLOW_LABEL`
- `PALLIUM_WORKFLOW_MODE`
- `PALLIUM_WORKFLOW_MODEL`
- `PALLIUM_WORKFLOW_REPO`
- `PALLIUM_WORKFLOW_CWD`
- `PALLIUM_WORKFLOW_PROMPT_FILE`
- `PALLIUM_WORKFLOW_OUTPUT_FILE`
- `PALLIUM_WORKFLOW_SCHEMA_FILE`

The prompt is delivered **only** as a file (`PALLIUM_WORKFLOW_PROMPT_FILE`);
read it with `PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")`. It is not passed
inline in the environment — a large prompt would exceed the OS `ARG_MAX` limit
on the environment block and fail the spawn with "argument list too long".

The provider should write its final worker message to
`PALLIUM_WORKFLOW_OUTPUT_FILE`. Stdout is used as a fallback.

Agents can also target another repo:

```js
await agent("Inspect backend API contract", {
  label: "backend",
  repo: "/path/to/backend",
  mode: "read-only"
});
```

For edit workers, patches apply back to that worker repo.

## Workflow CLI

Discovery and generation:

```bash
pallium workflow tools list --json
pallium workflow template list --json
pallium workflow template show test-fix --json
pallium workflow library list --json
pallium workflow library show security-audit --json
pallium workflow library install security-audit --cwd .
pallium workflow generate "review auth" --style review --output review.js
pallium workflow generate "custom migration workflow" --llm --output custom.js
pallium workflow validate custom.js
```

Run and inspect:

```bash
pallium workflow run "review this branch"
pallium workflow run --script review.js "review this branch"
pallium workflow run --workflow review-branch "review this branch"
pallium workflow run /review-branch "review this branch"
pallium workflow run --background "audit route handlers for missing auth"
pallium workflow list
pallium workflow status <run-id>
pallium workflow inspect <run-id>
pallium workflow show <run-id>
pallium workflow read <run-id>
pallium workflow report <run-id> --json
pallium workflow watch <run-id>
```

Control:

```bash
pallium workflow pause <run-id>
pallium workflow resume <run-id>
pallium workflow stop <run-id>
pallium workflow save <run-id> --name review-branch
pallium workflow apply <run-id>
pallium workflow revert <run-id>
```

Automation and fleet:

```bash
pallium workflow trigger add daily-review "review workflow changes" --cwd .
pallium workflow trigger add changed-review "review workflow changes" --kind on-changed --cwd .
pallium workflow trigger list
pallium workflow trigger run changed-review
pallium workflow trigger watch --once
pallium workflow fleet status --json
pallium workflow analytics --json
```

Agent gates:

```js
await gate("patch-safety", "Verify patches are safe to apply", {
  criteria: "tests pass, no secrets are introduced, and scope matches the task"
});
```

API and MCP:

```bash
pallium workflow serve --addr 127.0.0.1:8765
pallium workflow serve --addr 0.0.0.0:8765 --token "$PALLIUM_WORKFLOW_API_TOKEN"
pallium workflow mcp
```

Audit:

```bash
pallium workflow audit --json
pallium workflow audit --run-acceptance --json
scripts/workflow-acceptance.sh
```

## Verification Loops

Use `check()` when the script wants to branch on one command result:

```js
const result = await check("go test ./...", { label: "go-tests" });
if (!result.ok) {
  await parallel(result.failures || [], failure =>
    agent("Fix this failure: " + JSON.stringify(failure), {
      label: "fix-" + failure.name,
      mode: "edit",
      isolation: "worktree"
    })
  );
}
```

Use `verify.untilGreen()` when Pallium should own the check, fix, re-check loop:

```js
return verify.untilGreen("go test ./...", {
  label: "go-tests",
  maxRounds: 3
});
```

The loop stops when the command passes, the round limit is reached, or failures
stop changing.

## Costs And Budgets

Pallium does not add a billing layer. Workflow cost comes from whatever worker
provider you run, such as Codex, Claude, or another model CLI.

Pallium tracks an estimated per-agent cost so agents can make budget decisions:

```bash
export PALLIUM_WORKFLOW_AGENT_COST_USD=0.02
pallium workflow run "large review" --max-budget-usd 1.00
pallium workflow analytics --json
```

The estimate is local bookkeeping. It is not provider billing data unless your
provider command makes it so.

## Repo Memory Commands

```bash
pallium index
pallium doctor
pallium explain <path>
pallium risk <path>
pallium neighbors <path>
pallium decisions <query>
pallium safe <path>
pallium plan <path>
pallium changed-now
pallium review [base-ref]
pallium verify <fast|safe|full>
pallium handoff [base-ref]
pallium task start "Tighten auth flow" src/auth cmd
pallium task show
pallium task clear
```

`verify` records each run in the repo-local Pallium database so future `review`
and `handoff` output can include recent verification history.

## Session Memory

Pallium indexes Codex CLI transcripts from `~/.codex/sessions/**/*.jsonl` and
Claude Code transcripts from `~/.claude/projects/**/*.jsonl`.

```bash
pallium sessions live --details
pallium sessions index
pallium sessions index --safety-buffer 30m
pallium sessions index --force
pallium sessions index --provider claude
pallium sessions index --provider codex
pallium sessions list --limit 20
pallium sessions search "MCP auth" --limit 10
pallium sessions search "MCP auth" --hybrid --limit 10
pallium sessions related . --file cmd/app.go --limit 5
pallium sessions grep "Timed out waiting for PGLite lock" --limit 20
pallium sessions show <session-id> --transcript
pallium sessions embed --batch-size 8
pallium sessions semantic "find the session where we debugged MCP startup failures"
pallium sessions stats
```

Indexing is incremental. Active transcript files modified in the last two
minutes are skipped so Pallium does not chase logs while agents are still
writing them.

Session-memory data lives at `~/.pallium/codex-sessions.sqlite`. If an older
database exists, Pallium can fall back to `~/.codex-memory/codex-sessions.sqlite`.
Embeddings use `OPENAI_API_KEY` or `OPENAI_ADMIN_API_KEY`.

For another machine's sessions:

```bash
pallium sessions index --include /path/to/other/.codex/sessions --machine tylers-macbook
pallium sessions index --provider claude --include /path/to/other/.claude/projects --machine tylers-macbook
```

## Console Coordination

`pallium console` is a local coordination surface for agent sessions. It can
inspect live Codex and Claude Code sessions, store manifests, create handoffs,
track file claims, request authority, and manage review gates.

Pallium can also spawn sessions it owns:

```bash
pallium console ls --details
pallium console run --id owned-demo -- /bin/sh -c 'echo hello'
pallium console run --background --id owned-worker -- /bin/sh -c 'sleep 30'
pallium console owned list
pallium console owned show owned-worker
pallium console read owned-worker --tail 50
pallium console interrupt owned-worker
```

The current control boundary is conservative: Pallium controls sessions it
spawned itself. It does not inject commands into arbitrary existing Codex or
Claude Code sessions.

## HTTP API And SDK

Start the local workflow API:

```bash
pallium workflow serve --addr 127.0.0.1:8765
```

When binding outside localhost, set `--token` or `PALLIUM_WORKFLOW_API_TOKEN`.
Authenticated requests use `Authorization: Bearer <token>`. `GET /healthz` stays
open for local health checks.

Endpoints:

- `GET /healthz`
- `GET /workflows/fleet`
- `GET /workflows/analytics`
- `GET /workflows/library`
- `GET /workflows/library/{name}`
- `POST /workflows/library/install`
- `GET /workflows/runs/{id}`
- `POST /workflows/run`

The Go client in `pkg/workflowclient` wraps this API.

## Open Source Operations

Repository management files live in `.github/`:

- issue templates for bugs and feature requests
- pull request checklist
- `CODEOWNERS`
- Dependabot updates for Go, npm, and GitHub Actions
- CI, CodeQL, OpenSSF Scorecard, and release asset gates

Project policy files:

- `CONTRIBUTING.md`
- `SECURITY.md`
- `CODE_OF_CONDUCT.md`
- `SUPPORT.md`

Security issues should follow `SECURITY.md`, not public issue threads.

## Proof Gates

The workflow system has an installed-CLI acceptance gate:

```bash
PALLIUM_BIN="$(which pallium)" scripts/workflow-acceptance.sh
pallium workflow audit --run-acceptance --json
```

The acceptance script exercises:

- parallel worker timing
- resume cache reuse
- workflow generation and validation
- composition with saved workflows
- budget failure behavior
- structured reports
- repo preflight
- `verify.untilGreen`
- durable decisions
- multi-repo agents
- library install
- configured providers
- coordinator replanning
- edit patch apply and revert
- secret patch blocking
- on-changed triggers
- agent gates
- fleet limits
- analytics
- HTTP API
- MCP tool listing
- the v1-v7 audit checklist

For normal development:

```bash
go test ./...
go vet ./...
scripts/workflow-acceptance.sh
```

## Install

Install from npm:

```bash
npm install -g pallium
pallium version
```

The npm package installs the matching Pallium release binary from GitHub and
keeps it in `~/.pallium/npm/<version>/`.

Install a release tarball directly:

```bash
npm install -g https://github.com/tszaks/pallium/releases/download/vX.Y.Z/pallium-X.Y.Z.tgz
```

Install with Go:

```bash
go install github.com/tszaks/pallium@latest
```

Install from GitHub Packages:

```bash
npm config set @tszaks:registry https://npm.pkg.github.com
npm install -g @tszaks/pallium
```

Build from source:

```bash
git clone https://github.com/tszaks/pallium.git
cd pallium
go test ./...
go run . --help
```

## Data Locations

- Repo-local index and verification data: `.pallium/`
- Session memory database: `~/.pallium/codex-sessions.sqlite`
- Workflow run artifacts: `~/.pallium/workflow-runs/`
- User workflow library: `~/.pallium/workflows/`
- Legacy session database fallback: `~/.codex-memory/codex-sessions.sqlite`

## License

MIT
