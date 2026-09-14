# Model + effort evaluation protocol

Status: the executable harness is implemented; adapter smoke inference has run, but no accepted comparative benchmark establishes quality or savings. Task specifications can be materialized; independent reference validation and private grading remain prerequisites for promotion. This experiment tests Pallium work first, not all of Tyler's applications.

## Question and unit of comparison

Can an Auto policy meet a fixed quality bar at lower total cost or elapsed time than `gpt-6-astra / high`?

The unit is one completed task, including dispatch/context preparation, worker execution, verification, retries, escalation, and final synthesis. A candidate identity includes provider, exact model, effort, CLI version, harness revision, tool/permission configuration, service tier, and context policy. Record requested and effective values separately. Unknown effective settings make the run invalid for configuration comparisons.

## Candidate matrix

| ID | Provider | Model | Effort | Purpose |
| --- | --- | --- | --- | --- |
| astra-high | codex | gpt-6-astra | high | Fixed baseline |
| astra-low | codex | gpt-6-astra | low | Strong model with less reasoning |
| sol-medium | codex | gpt-5.6-sol | medium | Intermediate tier |
| terra-medium | codex | gpt-5.6-terra | medium | Balanced tier |
| luna-medium | codex | gpt-5.6-luna | medium | Lower-cost tier |
| luna-xhigh | codex | gpt-5.6-luna | xhigh | Requested smaller-model/higher-effort hypothesis |

These are hypotheses, not assignments of task winners. Each pair is separate; never infer performance from the model name alone. Start same-provider to control more of the harness. Later, evaluate Claude/Gemini as separate agent stacks with explicit access and policy verification.

## Task construction

`tasks.jsonl` contains six families of four tasks: bounded edits, debugging, review, source-grounded research, repository understanding, and durable execution design. The initial split contains 12 calibration and 12 holdout specifications, grouped by related root causes rather than forcing equal counts per family. Historical bugs come from local commits `49c0afa` and `b986618`; the adoption corpus contributes workload shapes. New specifications are not represented as verbatim historical user requests.

Freeze an archive of the source revision, prompt, input artifacts, fixture modifications, reference answer/patch, private tests or rubric, and hashes before trials. Do not give workers this research directory, git history containing the fix, reference solutions, or private grading material. Historical repairs use the pre-fix parent and only the relevant defect; remove unrelated failures or document them in advance. A task is eligible only after its baseline fixture and reference solution have both been checked.

For synthetic mutations, verify that the uncorrected fixture fails the intended acceptance test and the reference solution passes. For read-only tasks, use a frozen source packet and itemized answer key. Research tasks use frozen excerpts and source metadata so web drift and Exa access do not confound model comparisons; evaluate live research separately later. Never execute benchmark payloads against production accounts.

Related provider, effort, accounting, and session-recovery tasks are grouped into a single split using `split_group`. Validate that no group crosses splits when freezing. The grouped design remains 12/12. Holdout answers remain unavailable to the router developer after freezing. Repeated runs on one task are not independent new tasks.

## Trial stages and budget

1. Validate explicit effort transport and usage capture without paid inference using adapter tests. Confirm account/model access with the eventual trial preflight.
2. Materialize fixtures and validate reference solutions. Use `eval/routing/run.py prepare`; preparation alone does not validate a reference solution.
3. Calibration pilot: 12 tasks × 6 candidates × 1 run = 72 attempts, randomized candidate order with identical initial context. No adaptive retry in the first comparison; internal tool iterations count as part of the attempt. Predeclare equal task timeouts and token/tool ceilings.
4. Freeze a simple routing policy using calibration outcomes. Compare always-Astra-high, always-Astra-low, the best calibration-selected fixed candidate, and the routing policy. No hindsight per-task selection on holdout.
5. Holdout: 12 tasks × 6 candidates × 3 repetitions = 216 attempts for paired outcome estimates. Reuse a fixed prompt/environment; vary run IDs and randomize order. A later escalation-policy trial must actually execute and account for the handoff; it cannot be synthesized from isolated successful runs alone.

Total initial design: 288 task attempts, excluding fixture validation and escalation trials. This is a design ceiling, not an authorization to launch them. Dollar cost is unknown until per-task usage is measured. Set an explicit overall trial budget and per-task stop conditions before inference; use the calibration pilot to price and, if necessary, reduce the holdout matrix before exposing holdout answers. Never silently truncate a started experiment and claim equivalent evidence.

## Quality and accounting

For edits: required behavior, private regression checks, existing relevant checks, no unauthorized edits, and a correct completion report must all pass. Test success alone is insufficient if the implementation defeats the test or violates the prompt.

For reviews: score confirmed defect recall and false positives against a private defect ledger, with clean controls. A finding needs a reachable trigger, effect, and source evidence. For research/explanation: itemized factual coverage, correct citations, explicit unresolved facts, and no unsupported certainty. Use deterministic checks where possible; blind subjective graders to model identity and adjudicate disagreements. Astra must not be the sole quality oracle for its own comparison.

Store at least:

```text
run_id, task_id, family, split, repetition
source_sha, fixture_hash, prompt_hash, grader_hash, harness_sha
requested_provider/model/effort, effective_provider/model/effort
cli_version, service_tier, tool_policy_hash, context_tokens
start/end, queue_ms, execution_ms, verification_ms, total_ms
input_tokens, cached_input_tokens, cache_write_tokens
output_tokens, reasoning_tokens, provider_reported_cost_usd
estimated_api_equivalent_cost_usd, price_source/date, billing_basis
dispatch_cost, worker_cost, verification_cost, retry_cost, synthesis_cost
attempts, escalations, outcome, failure_class, quality_items, artifact_paths
```

Token buckets must follow each provider's billing semantics: do not charge reasoning twice if it is already included in output. Preserve raw provider usage alongside normalized data. Separate measured charges, API-equivalent estimates, subscription quota consumption, and unknowns. Zero means observed zero; missing means null. A subscription run does not prove cash savings from published API rates.

Report success rate and cost per success: **all attempts' total cost / accepted successes**. Zero successes yields undefined/infinite cost per success, never zero. Also report per-family success, false-positive rates, p50/p95 latency, timeout/escalation frequency, and human intervention. Infrastructure failures remain in operational totals but are labeled separately in capability analyses; report exclusions and reruns explicitly.

## Shadow Auto and promotion

Shadow mode records a recommendation without changing the executing configuration. It must not claim the unexecuted alternative succeeded. Recommendations use only pre-execution task features, eligible configurations, calibration evidence, and an explicit policy version. Store a short decision rationale and evidence IDs, not private chain-of-thought.

Proposed policy order: explicit user pin → allowed-provider/capability filter → calibrated task-family rule → conservative baseline when uncertain. Routing never expands permissions, invokes unavailable providers, or downgrades verification. The steering model may suggest task characteristics, but cannot invent eligible models or override policy. Configuration failures are not task-reasoning failures and should not trigger blind model escalation.

Suggested pilot signal: no new critical failures, no reduction in paired accepted tasks, and at least 20% lower aggregate cost with no more than 10% p95 latency regression, or at least 20% lower latency with no cost increase. These thresholds are proposed product choices, not research-derived guarantees.

The 24-task pilot is too small to establish broad non-inferiority or make Auto the global default. Promote first to opt-in on passing task families. To enable default Auto, collect a larger independent representative sample, predeclare a quality margin (suggested 2 percentage points), and require the lower one-sided 95% confidence bound of the paired success-rate difference to exceed minus that margin. Bootstrap by independent task/family, not by repeated attempts alone; predeclare treatment of clustered tasks and critical failures. An inconclusive interval means keep collecting data. Add rollback to the fixed baseline if quality or availability changes.

## Concrete prerequisites before model trials

- Effort is validated, transported, persisted, and included in replay/cache identity across the chosen execution paths.
- Effective configuration and complete usage accounting are observable; missing usage cannot produce savings claims.
- Fixture/reference checks pass and the holdout split is frozen without related-defect leakage.
- The trial budget, caps, billing basis, and permitted provider set are recorded.
- Results are written as observations, with unknowns and failures retained.

## Current harness limits

The harness records binary and harness hashes, rejects mixed configurations in comparisons, excludes simulations, and requires explicit grading evidence. Research fixtures currently use the catalog as a source packet, so they measure packet comprehension, not unaided research. Worker instructions prohibit sibling/history access, but this is not OS-enforced holdout isolation. Independent reference validation, private grading environments, CLI/service-tier attestation, and the larger promotion study are not supplied by fixture preparation. Do not interpret a shadow-policy proposal as authorization or statistical proof for global Auto.
