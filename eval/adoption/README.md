# Adoption Eval

Measures how **plug-and-play** Pallium is for a fresh agent: dropped into a
Pallium-equipped repo with only the `AGENTS.md` trigger block in its context
(what a harness like Claude Code / Codex auto-loads) and a task — with no other
coaching — does it make the right call and use Pallium correctly?

This exists because "how plug-and-play is it?" was being answered with vibe
scores. This turns it into a measured number from observed agent behavior.

## What it measures

Three things at once, which is the whole point:

- **Recall** — on tasks that genuinely warrant orchestration (multi-step,
  must-end-green, parallel, resumable, peer-coordinated, or cross-invocation), does
  the agent reach for Pallium? Covers all five services now: workflows, verify,
  repo-memory, teams, and loops.
- **Precision** — on trivial tasks (one-line edits, single-file reads), does it
  correctly *skip* Pallium instead of cargo-culting it?
- **Mid-session decay** — on tasks with several distinct pallium-warranting phases
  chained into one task, does adoption survive into the back half of the agent's own
  work, or does it fire once early and quietly revert to ad hoc for the rest? A tool
  that only gets credit for its first tool call would never catch this — see the
  ledger's adoption-decay incident, the evidence this closes.

A tool that scores high on recall/precision and still decays mid-session is not
plug-and-play: it's a one-shot habit, not a sustained one. See `rubric.md` for the
four scoring axes plus the decay dimension.

## Files

- `tasks.jsonl` — the task suite, each labeled `should_use` + `ideal_capability`
  (`workflow` / `verify` / `repo-memory` / `team` / `loop` / `none`). Tasks with
  `"decay_probe": true` describe several distinct pallium-warranting phases chained
  into one task, specifically to probe whether adoption survives past the first move.
- `rubric.md` — the four-axis scoring rubric (discovery, appropriateness,
  capability match, correct usage) plus the mid-session decay dimension.
- `run.sh` — reproducible standalone runner. Runs each task through a fresh
  headless `claude -p` against an isolated scratch checkout (own HOME → clean
  Pallium store), in `installed` (AGENTS.md present) or `control` (absent)
  condition, and detects Pallium usage from actual tool calls in the JSON
  transcript — not self-report. Also computes `decayed_mid_session`: walks every
  tool call in order, splits at the midpoint, and flags pallium used in the first
  half but never again in the second.
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
