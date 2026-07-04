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

repo="$ROOT/repo"
mkdir -p "$repo"
git -C "$repo" init >/dev/null
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name "Workflow Acceptance"
printf 'original\n' > "$repo/note.txt"
git -C "$repo" add note.txt
git -C "$repo" commit -m initial >/dev/null

db="$ROOT/sessions.sqlite"

cat > "$ROOT/parallel.js" <<'JS'
phase("parallel");
const results = await parallel(["a", "b", "c", "d"], item =>
  agent("inspect " + item, { label: item, mode: "read-only" })
);
return { count: results.length };
JS
"$PALLIUM_BIN" workflow validate "$ROOT/parallel.js" >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
PALLIUM_WORKFLOW_AGENT_STUB_DELAY_MS=200 \
  "$PALLIUM_BIN" workflow run --id wf-accept-parallel --db "$db" --cwd "$repo" --script "$ROOT/parallel.js" "parallel acceptance" --json >/dev/null

"$PALLIUM_BIN" workflow library list --json | grep -q '"name": "security-audit"'
"$PALLIUM_BIN" workflow library install security-audit --cwd "$repo" --json | grep -q '"installed": true'
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
  "$PALLIUM_BIN" workflow run --id wf-accept-provider --db "$db" --cwd "$repo" --script "$ROOT/provider.js" "provider acceptance" --json | grep -q '"provider": "fake"'

cat > "$ROOT/coordinator.js" <<'JS'
phase("inspect");
await agent("initial coordinator context", { label: "initial" });
const plan = await coordinator.replan("adapt the remaining work", { label: "coordinator" });
return plan;
JS
PALLIUM_WORKFLOW_AGENT_STUB='{"decision":"continue","next_steps":["inspect deeper"]}' \
  "$PALLIUM_BIN" workflow run --id wf-accept-coordinator --db "$db" --cwd "$repo" --script "$ROOT/coordinator.js" "coordinator acceptance" --json | grep -q '"label": "coordinator"'

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
"$PALLIUM_BIN" workflow trigger run changed-review --db "$db" --json | grep -q '"skipped": true'
"$PALLIUM_BIN" workflow trigger watch --once --db "$db" --json | grep -q '"skipped": 1'

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
"$PALLIUM_BIN" workflow gate list wf-accept-gate --db "$db" --json | grep -q '"status": "open"'
"$PALLIUM_BIN" workflow gate approve wf-accept-gate approve --db "$db" --json >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"ok":true}' \
  "$PALLIUM_BIN" workflow resume wf-accept-gate --db "$db" --json >/dev/null

"$PALLIUM_BIN" workflow report wf-accept-parallel --db "$db" --json | grep -q '"status": "completed"'
"$PALLIUM_BIN" workflow fleet status --db "$db" --json | grep -q '"runs_total"'
"$PALLIUM_BIN" workflow analytics --db "$db" --json | grep -q '"estimated_cost_usd"'

port="${PALLIUM_WORKFLOW_ACCEPTANCE_PORT:-18766}"
"$PALLIUM_BIN" workflow serve --db "$db" --addr "127.0.0.1:$port" >/tmp/pallium-workflow-acceptance-api.log 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  if curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$port/workflows/fleet" | grep -q '"runs_total"'
curl -fsS "http://127.0.0.1:$port/workflows/analytics" | grep -q '"estimated_cost_usd"'
curl -fsS "http://127.0.0.1:$port/workflows/library" | grep -q '"security-audit"'

printf '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}\n' |
  "$PALLIUM_BIN" workflow mcp --db "$db" | grep -q 'pallium_workflow_run'

echo "workflow acceptance passed"
