# Pallium

[![npm version](https://img.shields.io/npm/v/pallium.svg)](https://www.npmjs.com/package/pallium)
[![GitHub release](https://img.shields.io/github/v/release/tszaks/pallium?sort=semver)](https://github.com/tszaks/pallium/releases/latest)
[![license](https://img.shields.io/npm/l/pallium.svg)](./LICENSE)

**A local-first control plane for coding agents.**

Most people should not need to become expert agent managers to get expert-level
work from capable agents. Pallium translates a user's intent into the right
context, memory, execution shape, and verification while keeping authority with
the user. Durable orchestration and state live outside the context window, so
the work can survive a crash, restart, or end of chat.

One kernel, six services. Any model powers the work. Any agent drives it.

```bash
npm i -g pallium
pallium route "review my workflow changes and fix what's broken" --authority edit --execute --json
```

## The problem it solves

A coding agent in a chat session is powerful but underused and fragile. Users
must often know which tools to name, how to structure the work, and when to
verify it. The agent forgets prior sessions, cannot safely run ten workers in
parallel against one repo, and loses unfinished work when the session dies.

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

### `pallium route "<task>"`, agent-driven service choice

Give Pallium the task and the authority ceiling the user or environment already
granted. It inspects repository state, selects a named capability, explains why
it fits better than alternatives, and returns structured command arguments.
`--execute` runs that recommendation without a shell and returns its result. It
never widens authority or runs a blocked recommendation.

```bash
pallium route "find all running agent sessions" --authority observe --execute --json
pallium route "fix the checkout race and verify it" --authority edit --execute --json
pallium route capabilities --json
```

### `pallium start "<task>"`, the workflow golden path

Describe a task in plain language. Pallium scopes the repo, picks or generates a
plan, runs it, and reports back. Use it directly when you already know a
repo-scoped workflow is the right service, or follow a route that selects it.

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

Recall where prior Codex and Claude work stopped, what was completed, what is
still open, and the evidence behind the answer. Session memory stays local in
SQLite. Search results are compact and cite the source session and transcript
line instead of returning entire conversations.

```bash
pallium sessions sync
pallium sessions embedding status
pallium sessions embedding configure --provider openai --model text-embedding-3-small --credential-store keychain
pallium sessions embedding check
pallium sessions recall "where did the checkout migration stop?" --repo .
pallium sessions search "production smoke test" --hybrid --source codex
pallium sessions show <session-id>
pallium sessions read <session-id> --from-line 120 --limit 50
pallium sessions live --running-only --details
pallium sessions find "Which sessions finished a few minutes ago?" --details
pallium sessions find "Which sessions were updated most recently?" --limit 10
pallium decisions "why did we choose worktrees" --json
```

`sessions sync` indexes changed transcripts, creates bounded continuity
capsules, and catches up embeddings when a provider is configured. It
automatically performs a full upgrade pass when it detects legacy noisy titles,
metadata-only large sessions, or missing capsules. Raw provider events are not
stored unless `--raw-events` is explicitly supplied.

`sessions embedding configure` saves the provider, OpenAI-compatible base URL,
model, and optional credential-store selection in `~/.pallium/embedding.json`
with mode `0600`. On macOS, `--credential-store keychain` reads the provider key
from the login Keychain service `app.pallium.embedding`; the secret never enters
Pallium's JSON configuration, database, logs, or command line.
`PALLIUM_EMBED_API_KEY` remains an explicit per-process override. Use `sessions
embedding check` to verify the active vector space before a large embed.

`sessions recall` uses BM25 plus current embeddings when available. If semantic
retrieval is unavailable or times out, it returns lexical evidence and says so.
Filter recall and search with `--repo`, `--cwd`, `--source`, `--file`, `--since`,
or `--before`.

Semantic retrieval stores one bounded continuity vector per session, built from
the capsule, repository metadata, touched files, commands, and first/last
conversation evidence. Exact search still covers the full indexed transcript.

`sessions live` discovers local Codex CLI/Desktop and Claude Code sessions. JSON
output includes its best-effort coverage and explicit exclusions, so callers do
not mistake it for a generic inventory of shells, SSH/tmux, browsers, or unknown
agent providers. Transcript lifecycle and pending calls distinguish `active`,
`waiting`, `blocked`, `finished`, and `idle`. `stuck` requires both prolonged
silence and stopped or uninterruptible process evidence; silence alone is idle.

`sessions find` reasons over session metadata instead of transcript keywords.
It understands completion, unfinished work, last-activity age, and recency
phrases, then returns the exact interpretation and filters it applied. Use
explicit flags such as `--completion not_finished`, `--inactive-for 3h`,
`--finished-within 10m`, and `--sort updated` when deterministic automation is
more important than natural phrasing. Unknown completion evidence remains
unknown; Pallium does not silently treat it as unfinished.

Maintenance is explicit and safe by default:

```bash
pallium sessions doctor
pallium sessions doctor --repair --prune-raw-events --vacuum
pallium sessions forget <session-id>          # preview
pallium sessions forget <session-id> --confirm
pallium sessions prune --older-than 180d      # preview
pallium sessions prune --older-than 180d --confirm
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
