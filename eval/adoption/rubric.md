# Adoption Eval — Scoring Rubric

The eval measures how **plug-and-play** Pallium is for a fresh agent: dropped into a
Pallium-equipped repo with only the `AGENTS.md` trigger block in context (what a
harness auto-loads) and a task, does the agent make the right call and use Pallium
correctly — with no other coaching?

Each task is scored on four axes, 0–25 each (100 total per task).

## 1. Discovery (0–25)
Did the agent become aware of Pallium at all?
- 25: referenced Pallium unprompted and oriented itself (e.g. ran `pallium agents guide` / `pallium workflow tools list`) before deciding.
- 15: referenced Pallium but did not orient (guessed at usage).
- 0: never mentioned or considered Pallium.

For `should_use=false` tasks, Discovery is scored as "aware it exists" — it's fine (and correct) to be aware and still decline.

## 2. Appropriateness (0–25) — the precision/recall axis
Did the agent make the RIGHT use/skip decision for this task's complexity?
- `should_use=true`  → 25 if it chose to use Pallium, 0 if it did the task ad hoc.
- `should_use=false` → 25 if it correctly did NOT use Pallium (or explicitly reasoned it wasn't worth it), 0 if it over-engineered with Pallium.
This axis is what separates a genuinely useful tool from one that gets cargo-culted onto everything.

## 3. Capability match (0–25)
Given the decision, did it reach for the RIGHT part of Pallium?
- Compare the agent's chosen entry point against the task's `ideal_capability`
  (workflow / verify / repo-memory / none).
- 25: correct capability. 12: Pallium but suboptimal capability. 0: wrong or n/a.
- For `should_use=false`: 25 if `none` (correctly nothing), else penalized.

## 4. Correct usage (0–25)
Would the concrete first action actually work?
- 25: the command/script it produced is valid and would run (correct subcommand,
  valid workflow shape, right flags).
- 12: right idea, wrong invocation (e.g. a non-existent subcommand, malformed script).
- 0: broken or absent.
- For `should_use=false`: 25 if the trivial action it took is itself correct.

## Aggregate
- Per-task score = sum of the four axes (0–100).
- Suite score = mean across all tasks.
- Report should_use=true and should_use=false subsets separately: the true-subset
  measures whether Pallium gets reached for; the false-subset measures whether it's
  correctly avoided. A tool that scores high on one and low on the other is not
  plug-and-play — it's either invisible or cargo-culted.

## Lift
Run the same suite in a **control** condition (no `AGENTS.md` block installed). The
delta (installed − control) is the measured value of the adoption layer itself.
