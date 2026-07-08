# Adoption Eval — Baseline

Run via Pallium's own workflow engine (`eval.workflow.js`), dogfooded: 8 blind
fresh workers + 8 judges, all on the **claude provider** (Pallium adopting its
guiding agent's model), 16 agents total, 0 failures.

## Headline

| Metric | Score |
|--------|------:|
| **Overall** | **87.4 / 100** |
| should_use=true subset (recall — reach for it when warranted) | 79.8 |
| should_use=false subset (precision — skip it when trivial) | 100.0 |

Precision is perfect: on all three trivial tasks the worker recognized a
one-shot job, explicitly cited Pallium's own "skip it for one-shot edits"
guidance, and declined. No cargo-culting. Recall is the softer axis — the
trigger got a capable agent to reach for Pallium on 4 of 5 genuinely complex
tasks, with the right capability most of the time.

## Per-task

| Task | should_use | ideal | disc | appr | cap | use | total |
|------|:---:|-------|:---:|:---:|:---:|:---:|:---:|
| audit-concurrency | ✓ | workflow | 25 | 25 | 25 | 25 | 100 |
| fix-failing-test | ✓ | verify | 25 | 25 | 25 | 25 | 100 |
| multi-angle-review | ✓ | workflow | 25 | 25 | 25 | 25 | 100 |
| consistent-refactor | ✓ | workflow | 25 | 25 | 12 | 25 | 87 |
| investigate-slow-index | ✓ | repo-memory | 12 | 0 | 0 | 0 | 12 |
| explain-function | ✗ | none | 25 | 25 | 25 | 25 | 100 |
| fix-typo | ✗ | none | 25 | 25 | 25 | 25 | 100 |
| add-comment | ✗ | none | 25 | 25 | 25 | 25 | 100 |

Two soft spots, both instructive:
- **consistent-refactor (87)** — reached for Pallium but as `pallium verify fast`
  *after* doing the edits by hand, rather than orchestrating the per-site edits
  as a `pipeline` workflow. Used the right tool for the wrong half of the job.
- **investigate-slow-index (12)** — did a direct Grep/Read investigation and
  never reached for Pallium. The one genuine recall miss. It is also the most
  debatable label in the suite: a top-down code read is a defensible way to
  investigate perf, so this may be a borderline task as much as a Pallium miss.

## Honest caveats — read before trusting 87.4

1. **Repo confound.** The eval ran inside Pallium's own source repo, so
   "discovery" is trivially easy (Pallium is obviously present). This measures
   **decision quality given the trigger and obvious availability**, not cold
   discovery in an unrelated project. True cold-discovery + the adoption-layer
   *lift* need a neutral-repo run (`run.sh` supports the `installed` vs
   `control` split for exactly this) — that number is expected to be lower and
   is the most important follow-up.
2. **Self-report + LLM judge.** Workers self-reported the commands they ran;
   the judge sanity-checked them but this isn't ground-truth tool-call capture.
   `run.sh` (headless `claude -p --output-format json`) is the higher-fidelity
   path that reads actual tool calls.
3. **n=8, one model, one run.** Directional, not statistical. Same-model judge
   and worker (both Claude) — standard LLM-as-judge, but worth a cross-model
   judge check.
4. **Confounds inflate toward the ceiling.** Net: treat **~80 on the hard
   subset** as the more honest signal than the 87.4 headline, and treat
   cold-discovery as unmeasured-and-lower until the neutral-repo run exists.

## Engine finding (dogfood dividend)

Building this on Pallium surfaced a real capture-model limitation: a `pipeline`
stage that returns `agent(...).then(cb)` silently nulls the item — Pallium's
goja capture records `agent()` as a placeholder marker, so `.then()` chains onto
an unresolved promise and the stage errors (and, per the known silent-stage-error
behavior, drops the item without surfacing why). Native Ultracode allows `.then`
in a stage; Pallium does not. Workaround used here: return `agent()` directly and
recover per-item mapping by index. Worth either supporting promise chaining in
stages or failing loudly instead of nulling.
