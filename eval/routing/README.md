# Routing evaluation harness

Requires Python 3.12+, Git, Go, and a built Pallium binary. Preparation does not call a model. Trial execution uses your authenticated Codex CLI and may consume quota.

```sh
python3 eval/routing/run.py prepare --output /tmp/pallium-experiment
pallium route models init --config /tmp/pallium-policy.json
python3 eval/routing/run.py run --experiment /tmp/pallium-experiment --task dispatch-map --candidate luna-xhigh --config /tmp/pallium-policy.json --pallium /absolute/path/to/pallium --timeout 180 --isolated-codex-config
python3 eval/routing/run.py grade --result /absolute/path/to/result.json --outcome accepted --reviewer reviewer-name --evidence /absolute/path/to/review.md
python3 eval/routing/run.py report --experiment /tmp/pallium-experiment
python3 eval/routing/run.py suggest --experiment /tmp/pallium-experiment --config /tmp/pallium-policy.json --output /tmp/proposed-policy.json
```

Each run has its own checkout, process timeout, artifacts, and fixture/harness/binary hashes. Successful execution remains pending review. Edit trials run workflow tests and reject test-file modifications; a reviewer must also assess the task acceptance contract. Simulation results are excluded from reports and suggestions.

Reports retain failed attempts in cost per accepted success. Missing dollar usage makes dollar comparisons unknown. Suggestions use only paired calibration observations, require independent task groups and complete cost evidence, and emit shadow policies without changing the active configuration. The small starter corpus is insufficient for broad promotion; expand and independently validate it before enabling rules by default.

See the [protocol](../../docs/model-routing/evaluation.md) for reference validation, holdout isolation, and promotion requirements. In particular, prepared research packets contain synthesized source material and holdout isolation is instructional, not an OS security boundary.
