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

"$PALLIUM_BIN" workflow trigger add changed-review "review changes" --kind on-changed --db "$db" --cwd "$repo" --json >/dev/null
PALLIUM_WORKFLOW_AGENT_STUB='{"summary":"trigger"}' \
  "$PALLIUM_BIN" workflow trigger run changed-review --id wf-accept-trigger-1 --db "$db" --json >/dev/null
"$PALLIUM_BIN" workflow trigger run changed-review --db "$db" --json | grep -q '"skipped": true'

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

echo "workflow acceptance passed"
