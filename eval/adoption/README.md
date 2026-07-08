# Adoption Eval

Measures how **plug-and-play** Pallium is for a fresh agent: dropped into a
Pallium-equipped repo with only the `AGENTS.md` trigger block in its context
(what a harness like Claude Code / Codex auto-loads) and a task — with no other
coaching — does it make the right call and use Pallium correctly?

This exists because "how plug-and-play is it?" was being answered with vibe
scores. This turns it into a measured number from observed agent behavior.

## What it measures

Two things at once, which is the whole point:

- **Recall** — on tasks that genuinely warrant orchestration (multi-step,
  must-end-green, parallel, resumable), does the agent reach for Pallium?
- **Precision** — on trivial tasks (one-line edits, single-file reads), does it
  correctly *skip* Pallium instead of cargo-culting it?

A tool that scores high on one and low on the other is not plug-and-play: it's
either invisible or over-applied. See `rubric.md` for the four scoring axes.

## Files

- `tasks.jsonl` — the task suite, each labeled `should_use` + `ideal_capability`.
- `rubric.md` — the four-axis scoring rubric (discovery, appropriateness,
  capability match, correct usage).
- `run.sh` — reproducible standalone runner. Runs each task through a fresh
  headless `claude -p` against an isolated scratch checkout (own HOME → clean
  Pallium store), in `installed` (AGENTS.md present) or `control` (absent)
  condition, and detects Pallium usage from actual tool calls in the JSON
  transcript — not self-report.
- `BASELINE.md` — the most recent measured baseline and per-task breakdown.

## Running

```bash
# Full behavioral run (needs the `claude` CLI + a built `pallium` on PATH):
eval/adoption/run.sh installed
eval/adoption/run.sh control     # for the lift comparison
```

The **lift** = installed − control isolates the value of the adoption layer
(the `AGENTS.md` trigger) from the agent's baseline instincts.

## Interpreting

Report the `should_use=true` and `should_use=false` subsets separately, plus the
lift. Target for an external control plane is a measured **mid-80s** on the
combined score — the ceiling is lower than an in-model capability because the
decision to reach for Pallium lives outside the model doing the reasoning.
Chasing 90+ fights that structural limit and has sharply diminishing returns.
