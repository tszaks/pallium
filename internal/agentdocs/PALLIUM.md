# Pallium — Agent Guide

This file is written for AI agents. It answers three questions: what Pallium is, when to reach for it, and which capability to reach for. For workflow-script details, read `PALLIUM_WORKFLOW.md` after this. For the kernel/services architecture ruling that governs how new capabilities get added (and why), read `ARCHITECTURE.md`.

Pallium is a local-first control plane for coding agents. It keeps orchestration, repo memory, verification, and run state **outside** your context window, in a CLI plus SQLite store, so long or multi-agent work survives context limits, session restarts, and model switches. Workers **adopt whatever agent is steering Pallium** — run it from inside Claude Code and workers use Claude automatically, with no configuration; any other model (Gemini, a local CLI, whatever ships next) plugs in via a one-line wrapper script (see `providers/README.md`); Codex is only the fallback when no steering agent is detected. No model is ever hardcoded, and any agent — of any make — can drive Pallium through the CLI (stable `--json`) or the MCP server (`pallium workflow mcp`).

**Start here:** `pallium start "<task>"` is the golden path — it generates a repo-scoped workflow using the current model and runs it, in one command. Reach for the lower-level `pallium workflow ...` commands when you need finer control.

## What does Pallium do?

Pallium is one kernel with six services on top. The kernel — the SQLite store, provider dispatch, worktree/patch machinery, budgets, leases, the run journal — is shared substrate every service sits on; it is never split, and no service reaches into another's tables or internals directly. Composition happens through each service's own front door (`workflow run`, the team API, `loop tick`'s own spawn of a child workflow run, etc.), never a shortcut across.

1. **Repo intelligence** (`pallium index|explain|neighbors|risk|review|handoff`) — a static index answering "what does this change touch, how risky is it, which tests cover it" before you commit to a plan.
2. **Session awareness** (`pallium sessions`, `pallium decisions`) — what happened in previous agent runs, a durable decision log, structured handoffs between agents.
3. **Workflows** (`pallium workflow run --script f.js "task"`) — the flagship: write async JavaScript as the conductor, Pallium spawns provider-backed workers, streams items through `pipeline()`, fans out with `parallel()`, validates structured output against JSON Schemas, caches completed calls for resume. Discover primitives with `pallium workflow tools list --json`.
4. **Loops** (`pallium loop start|tick|status|list|stop|reset`) — a bounded, named cycle. `loop tick <name>` advances it by exactly one round — no daemon, cron/a trigger/an agent decides when to tick again — spawning a FRESH child workflow run each time (a loop never reuses one run across ticks, so there's no cross-tick cache collision and no risk of hitting a run's lifetime agent cap purely from ticking). Terminal states (`success`/`no_op`/`blocked`/`exhausted`/`stagnated`/`already_running`) map to distinct process exit codes so a caller can branch without parsing JSON. Use a loop when the SAME script needs to run again and again until some condition holds (a PR review clean, a metric recovered) — a one-shot workflow run is still the right tool for anything that finishes in one pass.
5. **Agent teams** (`pallium team start|spawn|join|member|send|tasks|run|status|approve|reject|gate|attach|template`) — a lead plus independent named peer agents that coordinate over a shared task board and mailbox, each with a real persistent session (`--resume`/`codex exec resume`) that survives across turns and even across the steering process being killed. Plan-approval (`approve`/`reject`) and quality gates (`gate set`, hooked at task-created/task-completed/teammate-idle) keep autonomous coordination checked; `team member stop|restart|steer` supervises ONE teammate at a time — soft, not a kill: a turn already in flight runs to its own natural completion, the supervision takes effect starting that member's next turn. `team start --template <name>` (`team template list|show` to browse) spawns a known-good team shape in one step — `parallel-review` (distinct-lens reviewers over one artifact) and `adversarial-debate` (two members explicitly arguing opposite sides) are built from real teams that ran during Pallium's own development, not theory. `team join <team-id> --as <name>` lets an ALREADY-RUNNING agent session (another Claude Code tab, a Codex session, a human) attach as a self-driving "external" member — no provider dispatch, it drives itself via the ordinary `inbox`/`send`/`tasks claim|complete` CLI, with a `last_active_at` heartbeat and read-your-own-inbox-is-the-delivery-receipt so `team status` shows real liveness instead of guessing. Use a team when the work genuinely benefits from PEERS reasoning independently and messaging each other — a workflow's `parallel()`/`pipeline()` is still the right tool for fan-out work that doesn't need peer-to-peer coordination.
6. **Adoption layer** (`PALLIUM.md`, `pallium agents guide|block|install`, `pallium start`) — how an agent discovers Pallium exists and adopts it with zero configuration, adapting to whichever model is actually steering.

Most tasks reach for exactly one service. A workflow can convene a team; a loop always runs a workflow every tick. None of the six ever reimplements another's job — if two services seem to need the same capability, that capability belongs in the kernel, not copy-pasted into both.

Kernel-level facilities every service can rely on, not services of their own: verification (`pallium verify fast|safe|full`, `verify.untilGreen(cmd)` inside workflows — objective test-fix loops with stall detection), and safe execution (edit-mode workers run in isolated git worktrees; patches are secret-scanned before applying; `pallium workflow revert` undoes an applied run).

## When to use Pallium

Reach for Pallium when a task matches ANY of these situations:

- The task has more than ~3 meaningful steps, or will run longer than one sitting
- The result must be verified objectively (tests green, build clean), not just written
- You want several workers in parallel (fan-out analysis, per-file sweeps, judge panels)
- Work should be resumable after interruption, or auditable after the fact
- Edits are risky enough to want isolation (worktree per agent, patch review, revert)
- You are about to hand off to another agent or another session

Do NOT use Pallium for a one-shot edit, a quick question, or exploration you can finish in a few tool calls. The overhead is not worth it there.

## The recommended pattern

1. Scope first: `pallium workflow preflight "<task>"` to get files-to-inspect, risk, and test commands.
2. Write a workflow script (see `examples/workflows/` for commented recipes, or `pallium workflow template list`).
3. Validate, then run: `pallium workflow validate f.js && pallium workflow run --script f.js "<task>" --json`.
4. Inside the script: `pipeline()` for multi-stage per-item work (default), `parallel()` only when the next step needs the full set, `verify.untilGreen()` before declaring edits done, `gate()` for autonomous approval checkpoints.
5. Afterward: `pallium workflow report <run-id> --json` for findings, `await pallium.decisions.record(title, body, ...tags)` inside a workflow script for choices worth remembering, `pallium handoff` if another agent continues.

## Decision table

| Situation | Use | Why |
|-----------|-----|-----|
| Complex or multi-step task | Workflow script | Structure, state, resume |
| Must end with tests passing | `verify.untilGreen` | Objective, stall-detecting loop |
| Many files or angles to cover | `pipeline()` / `parallel()` | Streaming concurrency, one context per worker |
| Findings need to be trustworthy | Adversarial verify recipe | Skeptic workers refute weak claims |
| Continuing yesterday's run | `pallium workflow resume <run-id>` | Unchanged prefix replays from cache |
| Switching agents or models | `pallium handoff` + providers | State lives outside any one context |
| Small single edit | Plain editing, no Pallium | Overhead not justified |
| Same task, run again and again until a condition holds | `pallium loop start` + `loop tick` | Bounded per-tick, resumable, no daemon |

## Rules that prevent bad runs

- Never use `Date.now()`, `Math.random()`, or argless `new Date()` in workflow scripts — determinism guards throw, because resume caching depends on replayable scripts. Pass timestamps through `--args`.
- Validate scripts before running. Inspect actual worker output (`pallium workflow show <id> --json`) before assuming what cached results contain.
- `parallel`/`pipeline` turn worker failures into `null` items — filter them and check the run's failure report rather than assuming every slot succeeded.
- Set a budget for long runs (`--max-budget-usd`); agent and budget caps are lifetime limits that survive resume.
- `pallium team ...`/`pallium loop ...` with no `--db` write to the real, shared, global `~/.pallium/codex-sessions.sqlite` — the same store real long-lived teams and loops use. For throwaway/test ones, either pass `--db <tmpfile>` every time, or set `PALLIUM_TEST_DB=<tmpfile>` once for the session (every `team ...`/`loop ...` call that forgets `--db` then redirects there instead, with a loud stderr warning so the redirect is never silent).
- Team and loop budgets share the same honesty caveat: not every provider reports real cost. claude, and a wrapper that reports `PALLIUM_WORKFLOW_USAGE_FILE`, are cost-tracked; codex is not (it has no usage envelope at all, true of the regular non-team/non-loop worker path too). A codex-only team's or loop's `--budget-usd`/`cycleBudgetUsd`/`lifetimeBudgetUsd` ceiling therefore cannot self-enforce — `team status`/`loop status` say so explicitly rather than showing an indistinguishable $0.0000.
