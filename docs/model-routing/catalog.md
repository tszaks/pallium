# Evidence catalog

Checked September 4, 2026 ET. Prices below are published USD per million tokens, standard API text input/output, before caching and other modifiers. They are **not** measured Pallium task costs or Codex/Claude subscription charges. All task-fit descriptions are hypotheses from provider documentation, not local benchmark findings.

## Initial OpenAI candidates

| Model | Input / output | API effort documented | Locally advertised Codex effort | Initial hypothesis | Evidence |
| --- | --- | --- | --- | --- | --- |
| gpt-6-astra | $10 / $50 | low, medium, high, xhigh, max | low, medium, high, xhigh, max, ultra | Baseline at high; compare low to avoid assuming smaller is cheaper per task | O1, L1 |
| gpt-5.6-sol | $4 / $20 | none, low, medium, high, xhigh, max | low, medium, high, xhigh, max, ultra | Intermediate capability option at medium | O2, O5, L1 |
| gpt-5.6-terra | $2 / $12 | none, low, medium, high, xhigh, max | low, medium, high, xhigh, max, ultra | Balanced candidate at medium | O3, O5, L1 |
| gpt-5.6-luna | $0.20 / $1.20 | none, low, medium, high, xhigh, max | low, medium, high, xhigh, max | Compare medium and xhigh; do not assume high effort closes the capability gap | O4, O5, L1 |

O1–O4 advertise 1,050,000-token context windows. API prices increase above 272K input tokens for the whole request: 2x input and 1.5x output. O6 distinguishes standard, batch/flex, and fast rates. Pin the price schedule and service tier before calculating costs. O2 describes Sol pricing as promotional through at least November 21, 2026. Verify again before trials.

Confidence: high that these are the values returned from the cited official sources during this research; medium for present billing applicability because pages/aliases change and the trial transport is a CLI; unmeasured for local performance. O2–O4 values were available in targeted Exa search extracts, while full-page fetches were dominated by navigation and their advertised `.md` endpoints returned not-found pages. Re-fetch authoritative content before using these rows as a billing contract.

Do not map `ultra` to an API effort: the local cache describes it as automatic task delegation and public API pages here do not list it. Do not enable `none` through Codex just because the API supports it. `pro` reasoning mode is a separate API dimension, excluded from this initial CLI experiment. Effort labels are not equal compute budgets across models or providers.

## Other providers and existing host choices

| Candidate | Published base input / output | Effort evidence | Availability and disposition |
| --- | --- | --- | --- |
| Claude Fable 5 | $10 / $50 | low, medium, high, xhigh, max; adaptive thinking always on | C1/C2; Claude CLI installed, specific account/model access untested; phase two |
| Claude Opus 5 | $5 / $25 | low, medium, high, xhigh, max | C1/C2; phase two, include a low-effort strong-model comparator |
| Claude Sonnet 5 | Conflicting $2 / $10 and $3 / $15 source variants | low, medium, high, xhigh, max | C1/C2; price unresolved, cannot use for savings claims |
| Claude Haiku 4.5 | $1 / $5 | Extended thinking, no effort parameter in C1 | C1; do not synthesize an xhigh setting; phase two |
| Gemini 3.5 Flash | $1.50 / $9 | minimal, low, medium, high | G1/G2/G3; wrapper exists, CLI absent; unavailable locally |
| Gemini 3.1 Flash-Lite | $0.25 / $1.50 text | minimal, low, medium, high | G1/G2/G3; unavailable locally |
| Gemini 3.1 Pro Preview | $2 / $12 at <=200K input; $4 / $18 above | low, medium, high | G2/G3; exact preview ID `gemini-3.1-pro-preview`; unavailable locally |
| Gemini 3.6 Flash / 3.5 Flash-Lite | Not established in this pass | G3 lists supported thinking levels | G1 catalog entries; exact invocation/pricing unresolved, excluded |
| GPT-5.5 / GPT-5.4-mini / GPT-5.3-codex-spark | Not established in this catalog | Local cache: low, medium, high, xhigh | L1; reserve comparators if current four-model pilot warrants expansion |
| gpt-daybreak-blue-latest | Alias-dependent | Local cache advertises low through ultra | O6 says alias currently points to Sol; avoid duplicate baseline and pin resolution before use |

Claude context limits in C1 are 1M for Fable/Opus/Sonnet and 200K for Haiku. Gemini thinking tokens are included in billed output (G3). Model capabilities do not guarantee the CLI adapter exposes those capabilities: Pallium's built-in Claude tool/network restrictions differ from Codex, and the Gemini wrapper has its own permission behavior. Cross-provider comparisons measure the whole agent stack unless the harness is standardized.

## Research findings that affect design

| Finding | Implication for Pallium | Source / limit |
| --- | --- | --- |
| RouteLLM learns cost/quality routing; task-matched data improves generalization | Use research to seed policies, then calibrate against local work | R1; old model pair, primarily short benchmark prompts, not proof for current coding agents |
| RouterBench provides precomputed multi-model outcomes | Compare routing policies offline after collecting outcomes; include static baselines | R2; published datasets are not Pallium workloads |
| SWE-bench evaluates patches in controlled environments | Freeze repo/environment, distinguish model failure from infrastructure failure | B1/B2; public tasks can be contaminated and tests are an incomplete quality measure |
| Terminal-Bench records agent and model separately and supports effort configuration | Record full model + effort + harness identity; pin benchmark versions | B3/B4; leaderboard ordering is not a task router |
| Anthropic recommends evaluating strong models at lower effort too | Include Astra low rather than only comparing Astra high to smaller models | C3; provider guidance with illustrative, not measured, curves |
| OpenAI guidance calls for task success, latency, reasoning/cache tokens and cost per success | Total-cost accounting and a quality floor are required | O5/O7; vendor guidance, not independent evidence of superiority |

No source in this pass supplied a controlled, task-level Astra-high versus Luna-xhigh comparison using Pallium's harness. Do not invent a task leaderboard. There is no measured latency table here. Research can suggest candidates; it cannot establish a winner without comparable outcomes.

## Source register

All URLs retrieved or discovered through Exa on the research date. Official model docs are primary capability/pricing evidence with a vendor incentive; benchmark maintainers are primary harness evidence; papers report their own experiments. Source agreement is not independence when multiple URLs reproduce one paper.

| ID | Source | Use and quality |
| --- | --- | --- |
| O1 | [Astra model](https://developers.openai.com/api/docs/models/gpt-6-astra) | Full body fetched; official specs, not independent performance evidence |
| O2 | [Sol model](https://developers.openai.com/api/docs/models/gpt-5.6-sol) | Targeted search extracts; official pricing/effort; full fetch incomplete |
| O3 | [Terra model](https://developers.openai.com/api/docs/models/gpt-5.6-terra) | Targeted search extracts; same extraction limitation |
| O4 | [Luna model](https://developers.openai.com/api/docs/models/gpt-5.6-luna) | Targeted search extracts; same extraction limitation |
| O5 | [Model guidance](https://developers.openai.com/api/docs/guides/latest-model) | Search extracts; official guidance, mutable page and model generation |
| O6 | [OpenAI pricing](https://developers.openai.com/api/docs/pricing) | Search extracts; distinguish service-tier tables |
| O7 | [Reasoning guide](https://developers.openai.com/api/docs/guides/reasoning) | Search extracts; model-dependent efforts and separate pro mode |
| C1 | [Claude overview](https://platform.claude.com/docs/en/models/overview) | Full comparison fetched; search variants conflicted on Sonnet price and Fable generation |
| C2 | [Claude effort](https://platform.claude.com/docs/en/build-with-claude/effort) | Full relevant sections fetched; supported efforts vary by model |
| C3 | [Choosing Claude models](https://claude.com/blog/claude-models-explained-choosing-the-best-model-for-your-use-case) | Vendor practical guidance, explicitly illustrative performance curves |
| G1 | [Gemini models](https://ai.google.dev/gemini-api/docs/models) | Full relevant catalog fetched; does not establish local CLI access |
| G2 | [Gemini pricing](https://ai.google.dev/gemini-api/docs/pricing) | Full relevant pricing sections fetched; exact IDs and standard tier retained |
| G3 | [Gemini thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking) | Search extracts; effort/budget distinctions and thinking-token billing |
| R1 | [RouteLLM](https://arxiv.org/html/2406.18665v4) | Paper inspected via Exa in this task's earlier feasibility pass and current discovery; limited transfer to coding agents |
| R2 | [RouterBench](https://ar5iv.labs.arxiv.org/html/2403.12031) | Author paper rendered by ar5iv; search extracts; routing evaluation design |
| B1 | [SWE-bench](https://www.swebench.com/) | Maintainer leaderboard; same-agent view supports comparability |
| B2 | [SWE-bench evaluation](https://github.com/swe-bench/swe-bench/blob/main/docs/guides/evaluation.md) | Maintainer evaluation instructions; outcome and cache semantics |
| B3 | [Terminal-Bench](https://www.tbench.ai/) | Maintainer benchmark descriptions; multiple versions |
| B4 | [Terminal-Bench repository](https://github.com/harbor-framework/terminal-bench) | Maintainer commands include agent/model/effort and repeated oracle checks |
| L1 | Local `~/.codex/models_cache.json`, timestamp `2026-09-05T02:24:30.902532Z` | Allowlisted model/effort metadata only; no authentication proof |

## Search audit and refresh policy

This implementation turn used 10 searches requesting 40 result slots across model capabilities, routing research, and evaluation methodology, plus page fetches. Result slots include duplicates and are not 40 independently reviewed sources. The register retains 19 distinct web sources and one local source. Earlier feasibility research is separate. Navigation-heavy fetches and `.md` not-found results were excluded as substantive evidence.

Observed conflicts: OpenAI's general model index lagged targeted model pages; Claude search variants named both Fable 5 and 5.1 and disagreed on Sonnet pricing. Preserve conflicts instead of merging variants into a fictional current specification. The first pilot excludes those unresolved candidates.

Proposed freshness rule: recheck availability, effective model ID, effort support, and pricing immediately before trials; invalidate performance evidence on model/harness/policy changes. Revisit task-fit evidence after representative new outcomes, not after arbitrary web consensus. No recurring refresh automation was created.
