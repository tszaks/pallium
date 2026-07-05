#!/usr/bin/env bash
set -euo pipefail

PALLIUM_BIN="${PALLIUM_BIN:-pallium}"
ROOT="$(mktemp -d)"
cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
    wait "$SERVER_PID" >/dev/null 2>&1 || true
  fi
  rm -rf "$ROOT"
}
trap cleanup EXIT

section() {
  echo "== $1 =="
}

assert_grep() {
  local file="$1"
  shift
  if ! grep -q "$@" "$file"; then
    echo "assertion failed: grep $* in $file" >&2
    exit 1
  fi
}

repo="$ROOT/repo"
other_repo="$ROOT/other-repo"
mkdir -p "$repo" "$other_repo"
for r in "$repo" "$other_repo"; do
  git -C "$r" init >/dev/null
  git -C "$r" config user.email test@example.com
  git -C "$r" config user.name "Workflow Acceptance"
  printf 'original\n' > "$r/note.txt"
  git -C "$r" add note.txt
  git -C "$r" commit -m initial >/dev/null
done

db="$ROOT/sessions.sqlite"

section "v1 core runner"
cat > "$ROOT/parallel.js" <<'JS'
phase("parallel");
const results = await parallel(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item, mode: "read-only" })
);
return { count: results.length };
JS
"$PALLIUM_BIN" workflow validate "$ROOT/parallel.js" >/dev/null
start_ms=$(python3 -c 'import time; print(int(time.time()*1000))')
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS=200 \
  "$PALLIUM_BIN" workflow run --id wf-accept-parallel --db "$db" --cwd "$repo" --script "$ROOT/parallel.js" "parallel acceptance" --json >/dev/null
end_ms=$(python3 -c 'import time; print(int(time.time()*1000))')
elapsed=$((end_ms - start_ms))
if [[ "$elapsed" -ge 800 ]]; then
  echo "expected parallel workflow to finish faster than sequential (~800ms), took ${elapsed}ms" >&2
  exit 1
fi
"$PALLIUM_BIN" workflow tools list --json >"$ROOT/tools.json"
assert_grep "$ROOT/tools.json" '"name": "parallel"'
assert_grep "$ROOT/tools.json" 'pipeline(items, stage1, stage2, ...)'
"$PALLIUM_BIN" workflow template list --json >"$ROOT/templates.json"
assert_grep "$ROOT/templates.json" '"name": "test-fix"'

cat > "$ROOT/pipeline-provider.py" <<'PY'
#!/usr/bin/env python3
import json
import os
import time

prompt = open(os.environ["PALLIUM_WORKFLOW_PROMPT_FILE"]).read().strip()
log = os.environ["PIPELINE_LOG"]
with open(log, "a") as f:
    f.write(f"start|{prompt}|{time.time()}\n")
if prompt == "stage1 slow":
    time.sleep(0.35)
elif prompt == "stage1 fast":
    time.sleep(0.05)
else:
    time.sleep(0.10)
with open(log, "a") as f:
    f.write(f"end|{prompt}|{time.time()}\n")
parts = prompt.split()
with open(os.environ["PALLIUM_WORKFLOW_OUTPUT_FILE"], "w") as f:
    json.dump({"stage": parts[0], "item": parts[1], "prompt": prompt}, f)
PY
chmod +x "$ROOT/pipeline-provider.py"
cat > "$ROOT/pipeline-stream.js" <<'JS'
phase("pipeline");
return pipeline(["slow", "fast"],
  item => agent("stage1 " + item, { label: "stage1-" + item, provider: "pipeline" }),
  result => agent("stage2 " + result.item, { label: "stage2-" + result.item, provider: "pipeline" })
);
JS
PIPELINE_LOG="$ROOT/pipeline.log" \
PALLIUM_WORKFLOW_PROVIDER_PIPELINE_COMMAND="$ROOT/pipeline-provider.py" \
  "$PALLIUM_BIN" workflow run --id wf-accept-pipeline-stream --db "$db" --cwd "$repo" --script "$ROOT/pipeline-stream.js" "pipeline streaming acceptance" --json >/dev/null
python3 - "$ROOT/pipeline.log" <<'PY'
import sys

times = {}
for line in open(sys.argv[1]):
    kind, prompt, ts = line.strip().split("|")
    times[f"{kind}|{prompt}"] = float(ts)
if times["start|stage2 fast"] >= times["end|stage1 slow"]:
    raise SystemExit("pipeline stage barrier detected")
PY

cat > "$ROOT/pause.js" <<'JS'
phase("scan");
const first = await agent("stable prompt", { label: "stable", mode: "read-only" });
return first;
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"source":"first"}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-pause --db "$db" --cwd "$repo" --script "$ROOT/pause.js" "pause acceptance" --json >/dev/null
"$PALLIUM_BIN" workflow pause wf-accept-pause --db "$db" --json >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"source":"second"}' \
  "$PALLIUM_BIN" workflow resume wf-accept-pause --db "$db" --json >/dev/null
"$PALLIUM_BIN" workflow show wf-accept-pause --db "$db" --json >"$ROOT/pause-show.json"
assert_grep "$ROOT/pause-show.json" 'source.*first'
"$PALLIUM_BIN" workflow status wf-accept-pause --db "$db" --json >"$ROOT/pause-status.json"
assert_grep "$ROOT/pause-status.json" '"status": "completed"'

section "v2 generation reports budget composition"
"$PALLIUM_BIN" workflow generate "review auth" --style review --output "$ROOT/generated.js" >/dev/null
"$PALLIUM_BIN" workflow validate "$ROOT/generated.js" --json >"$ROOT/generated-validate.json"
assert_grep "$ROOT/generated-validate.json" '"valid": true'
PALLIUM_WORKFLOW_GENERATE_STUB=$'phase("llm");\nreturn { ok: true };' \
  "$PALLIUM_BIN" workflow generate "custom llm workflow" --llm --output "$ROOT/llm.js" >/dev/null
"$PALLIUM_BIN" workflow validate "$ROOT/llm.js" --json >"$ROOT/llm-validate.json"
assert_grep "$ROOT/llm-validate.json" '"valid": true'
mkdir -p "$repo/.pallium/workflows"
cat > "$repo/.pallium/workflows/inner.js" <<'JS'
return { composed: true, topic: args.topic };
JS
cat > "$ROOT/compose.js" <<'JS'
phase("compose");
const inner = await workflow("inner", { topic: "acceptance" });
return inner;
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-compose --db "$db" --cwd "$repo" --script "$ROOT/compose.js" "compose acceptance" --json >"$ROOT/compose-run.json"
assert_grep "$ROOT/compose-run.json" 'composed.*true'
set +e
PALLIUM_WORKFLOW_AGENT_COST_USD=0.02 \
  "$PALLIUM_BIN" workflow run --id wf-accept-budget --db "$db" --cwd "$repo" --max-budget-usd 0.03 --script "$ROOT/parallel.js" "budget acceptance" >/tmp/pallium-accept-budget.out 2>&1
budget_status=$?
set -e
if [[ "$budget_status" -eq 0 ]]; then
  echo "expected budget-limited workflow to fail with four parallel agents" >&2
  exit 1
fi
grep -q 'workflow budget exhausted' /tmp/pallium-accept-budget.out
PALLIUM_WORKFLOW_AGENT_STUB='{"summary":"scoped","observations":["ok"],"risks":[],"next_steps":["ship"]}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-report --db "$db" --cwd "$repo" --script "$ROOT/pause.js" "report acceptance" --json >/dev/null
"$PALLIUM_BIN" workflow report wf-accept-report --db "$db" --json >"$ROOT/report.json"
assert_grep "$ROOT/report.json" '"status": "completed"'

section "v3 repo-native loops decisions multi-repo revert"
"$PALLIUM_BIN" workflow preflight "review workflow changes" --cwd "$repo" --scope note.txt --json >"$ROOT/preflight.json"
assert_grep "$ROOT/preflight.json" 'files_to_inspect'
cat > "$ROOT/until-green.js" <<'JS'
phase("verify");
const result = await verify.untilGreen("go test ./...", { label: "green", maxRounds: 2 });
return result;
JS
PALLIUM_WORKFLOW_AGENT_STUB_SEQUENCE='["{\"ok\":false,\"summary\":\"failing\",\"failures\":[{\"name\":\"Test\",\"message\":\"boom\"}]}","{\"summary\":\"fixed\"}","{\"ok\":true,\"summary\":\"passing\",\"failures\":[]}"]' \
  PALLIUM_WORKFLOW_AGENT_STUB='{"ok":false}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-until-green --db "$db" --cwd "$repo" --script "$ROOT/until-green.js" "until green acceptance" --json >"$ROOT/until-green-run.json"
assert_grep "$ROOT/until-green-run.json" '"status": "completed"'
assert_grep "$ROOT/until-green-run.json" '\\"ok\\": true'
cat > "$ROOT/decide-record.js" <<'JS'
phase("decide");
return pallium.decisions.record("Acceptance decision", "Persisted by v3 acceptance.", "workflow", "acceptance");
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-decision-record --db "$db" --cwd "$repo" --script "$ROOT/decide-record.js" "decision record" --json >/dev/null
cat > "$ROOT/decide-search.js" <<'JS'
phase("recall");
const decisions = await pallium.decisions.search("Acceptance decision", 5);
return { count: decisions.length, title: decisions[0].title };
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-decision-search --db "$db" --cwd "$repo" --script "$ROOT/decide-search.js" "decision search" --json >"$ROOT/decision-search-run.json"
assert_grep "$ROOT/decision-search-run.json" 'Acceptance decision'
cat > "$ROOT/multi-repo.js" <<JS
phase("multi");
return agent("inspect other repo", { label: "other", repo: "$other_repo", mode: "read-only" });
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"repo":"other"}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-multi-repo --db "$db" --cwd "$repo" --script "$ROOT/multi-repo.js" "multi repo acceptance" --json >"$ROOT/multi-repo-run.json"
assert_grep "$ROOT/multi-repo-run.json" '\\"repo\\": \\"other\\"'

section "v4 autonomy gates triggers library"
"$PALLIUM_BIN" workflow library list --json >"$ROOT/library.json"
assert_grep "$ROOT/library.json" '"name": "security-audit"'
"$PALLIUM_BIN" workflow library install security-audit --cwd "$repo" --json >"$ROOT/library-install.json"
assert_grep "$ROOT/library-install.json" '"installed": true'
test -f "$repo/.pallium/workflows/security-audit.js"
cat > "$ROOT/fake-provider.sh" <<'SH'
#!/usr/bin/env sh
if [ ! -s "$PALLIUM_WORKFLOW_PROMPT_FILE" ]; then
  echo "missing prompt file" >&2
  exit 1
fi
printf '{"ok":true,"provider":"%s","label":"%s"}' "$PALLIUM_WORKFLOW_PROVIDER" "$PALLIUM_WORKFLOW_LABEL" > "$PALLIUM_WORKFLOW_OUTPUT_FILE"
SH
chmod +x "$ROOT/fake-provider.sh"
cat > "$ROOT/provider.js" <<'JS'
phase("provider");
return agent("fake provider worker", { label: "fake-provider", provider: "fake" });
JS
PALLIUM_WORKFLOW_PROVIDER_FAKE_COMMAND="$ROOT/fake-provider.sh" \
  "$PALLIUM_BIN" workflow run --id wf-accept-provider --db "$db" --cwd "$repo" --script "$ROOT/provider.js" "provider acceptance" --json >"$ROOT/provider-run.json"
assert_grep "$ROOT/provider-run.json" '\\"provider\\": \\"fake\\"'
cat > "$ROOT/coordinator.js" <<'JS'
phase("inspect");
await agent("initial coordinator context", { label: "initial" });
const plan = await coordinator.replan("adapt the remaining work", { label: "coordinator" });
return plan;
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"decision":"continue","next_steps":["inspect deeper"]}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-coordinator --db "$db" --cwd "$repo" --script "$ROOT/coordinator.js" "coordinator acceptance" --json >"$ROOT/coordinator-run.json"
assert_grep "$ROOT/coordinator-run.json" '"label": "coordinator"'
cat > "$ROOT/edit.js" <<'JS'
phase("edit");
return agent("edit note", { label: "editor", mode: "edit", isolation: "worktree" });
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"summary":"edited"}' \
PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE=note.txt \
PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT='changed by workflow
' \
  "$PALLIUM_BIN" workflow run --id wf-accept-edit --db "$db" --cwd "$repo" --script "$ROOT/edit.js" "edit acceptance" --json >/dev/null
grep -q 'changed by workflow' "$repo/note.txt"
"$PALLIUM_BIN" workflow revert wf-accept-edit --db "$db" --json >/dev/null
grep -q '^original$' "$repo/note.txt"
cat > "$ROOT/secret.js" <<'JS'
phase("secret");
return agent("write secret", { label: "secret", mode: "edit", isolation: "worktree" });
JS
set +e
PALLIUM_WORKFLOW_AGENT_STUB='{"summary":"secret"}' \
PALLIUM_WORKFLOW_AGENT_STUB_WRITE_FILE=secret.env \
PALLIUM_WORKFLOW_AGENT_STUB_WRITE_CONTENT='OPENAI_API_KEY=sk-1234567890abcdefghijklmnop
' \
  "$PALLIUM_BIN" workflow run --id wf-accept-secret-policy --db "$db" --cwd "$repo" --script "$ROOT/secret.js" "secret policy acceptance" >/tmp/pallium-accept-secret.out 2>&1
secret_status=$?
set -e
if [[ "$secret_status" -eq 0 ]]; then
  echo "expected secret patch policy to block" >&2
  exit 1
fi
grep -q 'workflow patch policy blocked' /tmp/pallium-accept-secret.out
test ! -f "$repo/secret.env"
"$PALLIUM_BIN" workflow trigger add changed-review "review changes" --kind on-changed --db "$db" --cwd "$repo" --json >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"summary":"trigger"}' \
  "$PALLIUM_BIN" workflow trigger run changed-review --id wf-accept-trigger-1 --db "$db" --json >/dev/null
"$PALLIUM_BIN" workflow trigger run changed-review --db "$db" --json >"$ROOT/trigger-skip.json"
assert_grep "$ROOT/trigger-skip.json" '"skipped": true'
"$PALLIUM_BIN" workflow trigger watch --once --db "$db" --json >"$ROOT/trigger-watch.json"
assert_grep "$ROOT/trigger-watch.json" '"skipped": 1'
cat > "$ROOT/gate.js" <<'JS'
phase("gate");
gate("approve", "acceptance gate");
return agent("after gate", { label: "after" });
JS
set +e
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-gate --db "$db" --cwd "$repo" --script "$ROOT/gate.js" "gate acceptance" >/tmp/pallium-accept-gate.out 2>&1
gate_status=$?
set -e
if [[ "$gate_status" -eq 0 ]]; then
  echo "expected gate workflow to pause" >&2
  exit 1
fi
"$PALLIUM_BIN" workflow gate list wf-accept-gate --db "$db" --json >"$ROOT/gate-list.json"
assert_grep "$ROOT/gate-list.json" '"status": "open"'
"$PALLIUM_BIN" workflow gate approve wf-accept-gate approve --db "$db" --json >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow resume wf-accept-gate --db "$db" --json >/dev/null

section "v5 fleet coordination"
"$PALLIUM_BIN" workflow fleet status --db "$db" --json >"$ROOT/fleet-status.json"
assert_grep "$ROOT/fleet-status.json" '"runs_total"'
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS=1500 \
  "$PALLIUM_BIN" workflow run --id wf-accept-fleet-1 --db "$db" --cwd "$repo" --script "$ROOT/pause.js" "fleet one" --json >/dev/null &
fleet_pid=$!
sleep 0.3
set +e
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-fleet-2 --db "$db" --cwd "$repo" --max-active-runs 1 --script "$ROOT/pause.js" "fleet two" >/tmp/pallium-accept-fleet.out 2>&1
fleet_status=$?
set -e
wait "$fleet_pid" || true
if [[ "$fleet_status" -eq 0 ]]; then
  echo "expected fleet limit to block second active run" >&2
  exit 1
fi
grep -q 'workflow fleet limit reached' /tmp/pallium-accept-fleet.out

section "v6 infrastructure api mcp sdk analytics"
"$PALLIUM_BIN" workflow analytics --db "$db" --json >"$ROOT/analytics.json"
assert_grep "$ROOT/analytics.json" '"estimated_cost_usd"'
port="${PALLIUM_WORKFLOW_ACCEPTANCE_PORT:-18766}"
"$PALLIUM_BIN" workflow serve --db "$db" --addr "127.0.0.1:$port" >/tmp/pallium-workflow-acceptance-api.log 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$port/workflows/fleet" >"$ROOT/api-fleet.json"
assert_grep "$ROOT/api-fleet.json" '"runs_total"'
curl -fsS "http://127.0.0.1:$port/workflows/analytics" >"$ROOT/api-analytics.json"
assert_grep "$ROOT/api-analytics.json" '"estimated_cost_usd"'
curl -fsS "http://127.0.0.1:$port/workflows/library" >"$ROOT/api-library.json"
assert_grep "$ROOT/api-library.json" 'security-audit'
printf '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}\n' |
  "$PALLIUM_BIN" workflow mcp --db "$db" >"$ROOT/mcp-tools.json"
assert_grep "$ROOT/mcp-tools.json" 'pallium_workflow_run'

section "v7 proof gate"
"$PALLIUM_BIN" workflow audit --json >"$ROOT/audit.json"
assert_grep "$ROOT/audit.json" '"versions"'
assert_grep "$ROOT/audit.json" '"v7"'
req_count=$(grep -c '"version": "v7"' "$ROOT/audit.json" || true)
if [[ "$req_count" -lt 2 ]]; then
  echo "expected at least two v7 requirements in audit output" >&2
  exit 1
fi

echo "workflow acceptance passed (v1-v7)"
