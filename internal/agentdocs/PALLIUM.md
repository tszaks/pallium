# Pallium — Agent Guide

This file is written for AI agents. It answers three questions: what Pallium is, when to reach for it, and which capability to reach for. For workflow-script details, read `PALLIUM_WORKFLOW.md` after this.

Pallium is a local-first control plane for coding agents. It keeps orchestration, repo memory, verification, and run state **outside** your context window, in a CLI plus SQLite store, so long or multi-agent work survives context limits, session restarts, and model switches. Workers are provider-agnostic: they adopt whatever model the guiding agent uses (see `providers/README.md`).

## When to use Pallium

Reach for Pallium when a task matches ANY of these situations:

- The task has more than ~3 meaningful steps, or will run longer than one sitting
- The result must be verified objectively (tests green, build clean), not just written
- You want several workers in parallel (fan-out analysis, per-file sweeps, judge panels)
- Work should be resumable after interruption, or auditable after the fact
- Edits are risky enough to want isolation (worktree per agent, patch review, revert)
- You are about to hand off to another agent or another session

Do NOT use Pallium for a one-shot edit, a quick question, or exploration you can finish in a few tool calls. The overhead is not worth it there.

## Mental model: five capabilities

1. **Workflows** (`pallium workflow run --script f.js "task"`) — you write async JavaScript as the conductor; Pallium spawns provider-backed workers, streams items through `pipeline()`, fans out with `parallel()`, validates structured output against JSON Schemas, caches completed calls for resume, and persists everything in SQLite. This is the flagship. Discover primitives with `pallium workflow tools list --json`.
2. **Repo memory** (`pallium workflow preflight`, `pallium safe <path>`, `pallium changed-now`, `pallium neighbors`) — static index answering "what does this change touch, how risky is it, which tests cover it" before you fan out.
3. **Verification** (`pallium verify fast|safe|full`, `verify.untilGreen(cmd)` inside workflows) — objective test-fix loops with stall detection.
4. **Session memory** (`pallium sessions`, `pallium decisions`, `pallium handoff`) — what happened in previous agent runs, durable decision log, structured handoffs between agents.
5. **Safe execution** — edit-mode workers run in isolated git worktrees; patches are secret-scanned before applying; `pallium workflow revert` undoes an applied run.

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

## Rules that prevent bad runs

- Never use `Date.now()`, `Math.random()`, or argless `new Date()` in workflow scripts — determinism guards throw, because resume caching depends on replayable scripts. Pass timestamps through `--args`.
- Validate scripts before running. Inspect actual worker output (`pallium workflow show <id> --json`) before assuming what cached results contain.
- `parallel`/`pipeline` turn worker failures into `null` items — filter them and check the run's failure report rather than assuming every slot succeeded.
- Set a budget for long runs (`--max-budget-usd`); agent and budget caps are lifetime limits that survive resume.
