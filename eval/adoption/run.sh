#!/usr/bin/env bash
# Adoption eval runner — measures whether a fresh agent reaches for Pallium
# correctly given only the AGENTS.md trigger block in context.
#
# Usage:
#   eval/adoption/run.sh installed   # AGENTS.md block present (realistic)
#   eval/adoption/run.sh control     # no block (baseline lift comparison)
#
# Requires: the `claude` CLI (headless) and a built `pallium` on PATH.
# Produces: eval/adoption/results.<condition>.jsonl — one record per task with
# the full transcript path and detected Pallium usage. Score with score.py.
#
# Fidelity note: each task runs a fresh `claude -p` (no shared context) against
# an isolated scratch checkout with its own HOME (so the Pallium SQLite store is
# clean per run). Tool calls are read from --output-format json, so Pallium usage
# is detected from ACTUAL invocations, not self-report.

set -euo pipefail
cond="${1:?usage: run.sh <installed|control>}"
here="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$here/../.." && pwd)"
block="$(cd "$repo_root" && go run . agents block)"
out="$here/results.$cond.jsonl"
: > "$out"

while IFS= read -r line; do
  [ -z "$line" ] && continue
  id=$(printf '%s' "$line" | python3 -c "import json,sys;print(json.load(sys.stdin)['id'])")
  task=$(printf '%s' "$line" | python3 -c "import json,sys;print(json.load(sys.stdin)['task'])")

  scratch="$(mktemp -d)"
  home="$(mktemp -d)"
  git -C "$repo_root" archive HEAD | tar -x -C "$scratch"
  ( cd "$scratch" && git init -q && git add -A && git -c user.email=e@x.com -c user.name=e commit -qm init )

  if [ "$cond" = "installed" ]; then
    printf '%s\n' "$block" > "$scratch/AGENTS.md"
    prompt="$block

---
You are working in the repository at $scratch. Task: $task"
  else
    prompt="You are working in the repository at $scratch. Task: $task"
  fi

  transcript="$here/transcript.$cond.$id.json"
  HOME="$home" claude -p "$prompt" \
    --output-format json \
    --allowedTools "Read,Grep,Glob,LS,Bash(pallium:*),Bash(go doc:*),Bash(cat:*),Bash(ls:*)" \
    > "$transcript" 2>/dev/null || true

  # Objective detection: did any tool call actually invoke `pallium`?
  used=$(python3 - "$transcript" <<'PY'
import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    print("false"); sys.exit()
events = data if isinstance(data, list) else data.get("messages", [data])
text = json.dumps(events)
print("true" if "pallium " in text or '"pallium"' in text else "false")
PY
)
  python3 -c "import json,sys;print(json.dumps({'id':sys.argv[1],'condition':sys.argv[2],'used_pallium_toolcall':sys.argv[3]=='true','transcript':sys.argv[4]}))" "$id" "$cond" "$used" "$transcript" >> "$out"
  echo "[$cond] $id → used_pallium=$used"
  rm -rf "$scratch" "$home"
done < "$here/tasks.jsonl"

echo "wrote $out"
