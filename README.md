# Pallium

[![npm version](https://img.shields.io/npm/v/pallium.svg)](https://www.npmjs.com/package/pallium)
[![GitHub release](https://img.shields.io/github/v/release/tszaks/pallium?sort=semver)](https://github.com/tszaks/pallium/releases/latest)
[![license](https://img.shields.io/npm/l/pallium.svg)](./LICENSE)

**A local-first control plane for coding agents.**

Pallium keeps orchestration, state, and verification outside an agent's context
window, so an agent can run large, multi-step, parallel work that survives a
crash, a restart, or the end of the chat.

One kernel, six services. Any model powers the work. Any agent drives it.

```bash
npm i -g pallium
pallium start "review my workflow changes and fix what's broken"
```

## The problem it solves

A coding agent in a chat session is powerful but fragile. It forgets prior
sessions. It cannot safely run ten workers in parallel against one repo. If the
session dies mid-task, the work dies with it. And its orchestration is welded to
whichever model you happened to open.

Pallium moves the durable parts out of the model:

- **State lives in SQLite**, not the context window, so a task survives the
  session that started it.
- **Edits happen in isolated worktrees** with delayed, reviewable patch
  application, so parallel workers never corrupt each other.
- **Verification is first-class.** Objective checks, fix-until-green loops, and
  gates that must pass before work is accepted.
- **Providers are pluggable.** Claude, Codex, or any CLI behind a small wrapper.
  The agent steering Pallium and the agents doing the work can be different
  models.

## The six services

Pallium is one binary and one local database. The services share that kernel and
compose through public interfaces.

### `pallium start "<task>"`, the golden path

Describe a task in plain language. Pallium scopes the repo, picks or generates a
plan, runs it, and reports back. Start here when you are not sure which service
you need.

### Workflows: deterministic multi-agent orchestration

Author a plan as async JavaScript: fan out parallel workers, pipeline stages,
force objective checks, gate on verification. Runs persist with stable IDs, full
history, and resumable state.

```bash
pallium workflow preflight "audit the auth module" --scope internal/auth --json
pallium workflow run "audit the auth module"
pallium workflow report <run-id> --json
```

### Loops: bounded recurring cycles

A loop runs one observe, act, verify, record cycle per tick and stops on a named
terminal state (`success`, `no_op`, `blocked`, `stagnated`, `exhausted`). It
detects when it stops making progress instead of spinning forever. No daemon; an
external scheduler or agent drives the ticks.

```bash
pallium loop start review-until-clean --script review.loop.js "<task>"
pallium loop tick review-until-clean
pallium loop status review-until-clean --json
```

### Agent teams: persistent collaborating peers (experimental)

A lead plus independent teammates that share a task board and a mailbox and
coordinate on their own. Teammates persist across many turns by resuming their
native provider sessions, so a team survives the process that started it. This
service is early and evolving.

```bash
pallium team start "find the root cause of the checkout hang"
pallium team spawn <team-id> reviewer --role "trace the failure"
pallium team run <team-id>
pallium team status <team-id>
```

### Repo intelligence: context before an edit

Fast, scriptable answers about a codebase: what a file does, what usually changes
with it, what is risky to touch, what changed in the working tree, and how to
hand work off.

```bash
pallium index   # one-time per repo, before the queries below
pallium explain cmd/workflow.go --json
pallium risk internal/workflow/runtime.go --json
pallium neighbors cmd/app.go --json
pallium changed-now --json
pallium handoff origin/main --json
```

### Session awareness and decisions

Recall prior agent sessions across tools, and record the decisions behind the
work so the reasoning outlives any one context.

```bash
pallium sessions live --json
pallium decisions "why did we choose worktrees" --json
```

## A worked example

A review workflow that fans out across dimensions, then verifies each finding
before trusting it. You author the plan as a script and run it:

```bash
pallium workflow generate "review the changed files for correctness and \
  security bugs, then verify each finding adversarially" \
  --style review --output review.workflow.js
pallium workflow validate review.workflow.js
pallium workflow run --script review.workflow.js "review the diff" --json
```

The script runs a reviewer per dimension in parallel, spawns a skeptic to try to
refute each finding, keeps the ones that survive, and returns a ranked list. The
run is saved: inspect it, resume it, or read its report later.

(A bare `pallium workflow run "<task>"` with no script uses a built-in
plan-and-verify default. Supply a script, or `pallium start`, when you want a
specific shape like the review above.)

## Mental model

Think operating system, not script runner. The **kernel** (SQLite store,
provider dispatch, worktrees, budgets, leases) is shared and never bypassed. The
**services** (workflows, loops, teams, repo intelligence, sessions, adoption) are
separate products on top that compose only through each other's front doors. A
loop can run a workflow; a workflow can convene a team. None of them reach into
another's internals.

Most commands accept `--json` for agent parsing. Runs, loops, and teams are all
resumable and inspectable.

## For agents

Pallium is built to be driven by an agent, not only a human. Point your agent at
the guide:

```bash
pallium agents guide      # the full agent-facing manual
pallium agents install    # add a short adoption block to AGENTS.md / CLAUDE.md
```

Deep reference for authoring workflows lives in
[`PALLIUM_WORKFLOW.md`](./PALLIUM_WORKFLOW.md); the agent guide is
[`PALLIUM.md`](./PALLIUM.md).

## Install and build from source

```bash
npm i -g pallium          # released binary via the npm wrapper
```

```bash
git clone https://github.com/tszaks/pallium.git
cd pallium
go test ./...
go run . --help
```

## Data locations

- Repo-local index and verification data: `.pallium/`
- Session memory database: `~/.pallium/codex-sessions.sqlite`
- Workflow run artifacts: `~/.pallium/workflow-runs/`
- User workflow library: `~/.pallium/workflows/`

## Status

Pallium is young and moving fast. Workflows, loops, and the repo-intelligence and
session services are stable in daily use. Agent teams are experimental and change
release to release. Expect sharp edges, file issues, and read the release notes
before upgrading.

## License

MIT. See [LICENSE](./LICENSE).
