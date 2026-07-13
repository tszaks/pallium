# Adoption Eval — Baseline

## v2 (2026-07-13) — M3: +team/+loop scenarios, fresh 10-task run

Run via Pallium's own workflow engine (`eval.workflow.js`), dogfooded: 10 blind
fresh workers + 10 judges, all on the **claude provider** (Pallium adopting its
guiding agent's model), 20 agents total, 0 failures. Supersedes v1 below — same
methodology, plus the two capabilities v1 never covered (team, loop), run against
a corrected trigger block (see "Real bug found and fixed" below).

### Headline

| Metric | Score |
|--------|------:|
| **Overall** | **75.2 / 100** |
| should_use=true subset (recall — reach for it when warranted) | 64.6 |
| should_use=false subset (precision — skip it when trivial) | 100.0 |

Precision is still perfect — 3/3 trivial tasks correctly declined, no cargo-culting,
identical to v1. Recall is markedly lower than v1's 79.8. Read past the headline
number before treating it as "adoption regressed" — see the per-task breakdown and
analysis below; the honest read is more nuanced than a single number.

### Per-task

| Task | should_use | ideal | disc | appr | cap | use | total |
|------|:---:|-------|:---:|:---:|:---:|:---:|:---:|
| audit-concurrency | ✓ | workflow | 25 | 25 | 25 | 25 | 100 |
| **team-adversarial-review** | ✓ | **team** | 25 | 25 | 25 | 25 | **100** |
| **recurring-test-fix-loop** | ✓ | **loop** | 25 | 25 | 25 | 25 | **100** |
| explain-function | ✗ | none | 25 | 25 | 25 | 25 | 100 |
| fix-typo | ✗ | none | 25 | 25 | 25 | 25 | 100 |
| add-comment | ✗ | none | 25 | 25 | 25 | 25 | 100 |
| fix-failing-test | ✓ | verify | 22 | 0 | 0 | 25 | 47 |
| multi-angle-review | ✓ | workflow | 25 | 0 | 0 | 25 | 50 |
| consistent-refactor | ✓ | workflow | 15 | 0 | 0 | 25 | 40 |
| investigate-slow-index | ✓ | repo-memory | 15 | 0 | 0 | 0 | 15 |

**The headline finding: both brand-new scenarios scored a clean 100.** The
team-adversarial-review worker oriented for real, correctly chose `team`, and
specifically picked the `adversarial-debate` template — the exact match for
"have someone argue it's fine and someone else try to break it." The
recurring-test-fix-loop worker chose `pallium loop` and every concrete detail
(`loop start <name>`, `loop tick`, `loop status`) verified against the real CLI.
Both capabilities were previously undiscoverable from the trigger block at all
(see below) — first exposure, first real test, clean pass.

**Three of the five original should_use=true tasks dropped hard** (47, 50, 40,
down from 100, 100, 87 in v1) for the *same tasks*. The judge notes are specific
and consistent, not vague: the worker was Pallium-*aware* in every case (discovery
15–25) but explicitly declined orchestration on tasks the labels say warrant it —
"cherry-picked [the skip clause] while ignoring the decision table's directly-
matching row" (fix-failing-test), "declined without orienting" (consistent-refactor,
multi-angle-review). This reads as a real recall miss pattern, not judge noise:
a Pallium-aware worker choosing to work ad hoc anyway on multi-step tasks.
investigate-slow-index (15) repeats v1's exact same miss.

### Honest analysis — don't over-read a single run

This is genuinely a **different measurement** from v1, not a rerun of the same
one, for three compounding reasons — any or all could explain the recall drop,
and this run alone can't isolate which:

1. **Fresh LLM samples.** v1 and v2 are different real model calls on the same
   tasks; LLM sampling variance at n=1 is real and already flagged in v1's own
   caveats below. A worker "declining without orienting" on one draw and
   orienting-and-using on another is consistent with ordinary variance, not
   necessarily a regression.
2. **The trigger block changed mid-measurement.** Fixing the real staleness bug
   (below) made the block noticeably longer — more capabilities to mention. A
   longer trigger competing for attention against a specific task's own
   complexity is a plausible, testable hypothesis for why recall on the
   *original* tasks moved, independent of whether the new content itself is
   correct.
3. **Same-model judge and worker**, same limitation v1 already named — not new
   to v2, but still unresolved.

**What this run does support with real confidence:** the two new capabilities
are discoverable and get chosen correctly when they're the right fit (100/100,
real commands, real template match) — that's the actual M3 ask, answered. What
it does NOT support: a confident claim that "recall dropped from 79.8 to 64.6"
in any strong causal sense. Both numbers stand as honestly-measured, real,
single-run snapshots; neither should be treated as *the* number without a
multi-run average, which is the real follow-up this baseline earns (same
"most important follow-up" caveat v1 already flagged and still hasn't been
done — track record: this is the second baseline in a row citing it).

### Real bug found and fixed, load-bearing for this whole measurement

`cmd/agents.go`'s `agentsBlock` — the ACTUAL string shipped in the `pallium`
binary and installed into real AGENTS.md/CLAUDE.md files via `pallium agents
install` (this machine's own files included) — only ever mentioned workflows.
It never mentioned `team` or `loop` at all, despite both having shipped
milestones ago. A fresh agent relying on this exact trigger had no textual path
to discovering 2 of Pallium's 6 services. Fixed directly in the source (mirrored
in `eval.workflow.js`'s own `BLOCK` constant, which was drifting from the real
one before this fix). This is the actual root cause the team/loop scenarios
exist to catch, and it's why their perfect scores here are meaningful rather
than a easy layup — the trigger genuinely didn't mention them before this fix.

### Mid-session decay — mechanism built, real baseline number not yet obtained

`tasks.jsonl` gained two `decay_probe` tasks and `run.sh` gained real
transcript-walking decay detection (`decayed_mid_session`: pallium used in the
first half of the tool-call sequence, never again in the second — the exact
"one preflight call then silently reverted to manual for the rest" pattern from
the ledger's own adoption-decay incident). The detection logic itself is
verified correct against 5 synthetic transcripts (sustained use, early-only
use, late-only use, never-used, and a `ls pallium/`-shaped false-positive
guard) — all five classified exactly as expected.

**What's NOT done:** a real number from this measures-actual-behavior path.
`run.sh`'s isolated-HOME design (deliberately clean per-task `~/.pallium/`
store) breaks `claude -p` login on this machine — that account's session auth
isn't in `~/.claude/.credentials.json` (which here holds only per-MCP-plugin
OAuth entries with empty tokens) or `~/.claude.json`, so it likely resolves
through the OS keychain, which a copied-files approach can't safely reproduce.
This is now documented plainly in `run.sh` and `README.md` as a known
limitation rather than silently producing a fake "0% decay" number from every
task failing to authenticate (which is exactly what happened before this was
caught — the first two runs both showed `total_toolcalls: 0` across the board
before the auth failure was traced). The dogfooded `eval.workflow.js` path
(used for the numbers above) can't substitute: it only asks a worker to
self-report its *intended* approach for one task description, and decay is a
BEHAVIORAL fact about an actually-executed multi-phase task — a worker could
self-report "I'd use it throughout" without that claim ever being tested.
Real decay measurement needs `run.sh`'s working real-transcript path on an
environment where headless `claude -p` auth survives HOME isolation.

Also found and fixed while getting even this far: `run.sh`'s cleanup
(`rm -rf "$scratch" "$home"`) could race a lingering subprocess still writing
into the scratch directory, return non-zero, and — under the script's
`set -euo pipefail` — silently abort the ENTIRE eval run after whichever task
happened to lose that race, with every task after it simply never running.
Now retries briefly and falls back to a visible warning instead of losing the
rest of the suite over one directory the OS's own tmp cleanup reaps anyway.

---

## v1 (2026-07-08, historical)

Run via Pallium's own workflow engine (`eval.workflow.js`), dogfooded: 8 blind
fresh workers + 8 judges, all on the **claude provider** (Pallium adopting its
guiding agent's model), 16 agents total, 0 failures.

### Headline

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

### Per-task

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

### Honest caveats — read before trusting 87.4

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

### Engine finding (dogfood dividend)

Building this on Pallium surfaced a real capture-model limitation: a `pipeline`
stage that returns `agent(...).then(cb)` silently nulls the item — Pallium's
goja capture records `agent()` as a placeholder marker, so `.then()` chains onto
an unresolved promise and the stage errors (and, per the known silent-stage-error
behavior, drops the item without surfacing why). Native Ultracode allows `.then`
in a stage; Pallium does not. Workaround used here: return `agent()` directly and
recover per-item mapping by index. Worth either supporting promise chaining in
stages or failing loudly instead of nulling.
