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
# Fidelity note: each task runs a fresh `claude -p` (no shared context) with its
# cwd AND HOME set to an isolated scratch checkout (so file/context access and
# the Pallium SQLite store are clean per run). Pallium usage is read from the
# assistant tool_use events in the --output-format stream-json event stream, so
# it reflects ACTUAL invocations, not self-report or text the agent merely read.

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
  # Run claude INSIDE the isolated checkout so its relative file/Bash access and
  # project context resolve to $scratch, not wherever the runner was launched.
  # stream-json (with --verbose, which the CLI requires alongside -p) emits one
  # JSON event per line INCLUDING assistant tool_use events — the plain `json`
  # format only emits the final result, so the tool-call walk below would never
  # see a real pallium invocation. The final result event is still in the stream,
  # so the transcript keeps the agent's answer.
  ( cd "$scratch" && HOME="$home" claude -p "$prompt" \
      --output-format stream-json --verbose \
      --allowedTools "Read,Grep,Glob,LS,Bash(pallium:*),Bash(go doc:*),Bash(cat:*),Bash(ls:*)" ) \
    > "$transcript" 2>/dev/null || true

  # Objective detection: did the agent ACTUALLY invoke `pallium` in a tool call?
  # Look only at assistant `tool_use` Bash commands — never the prompt, the
  # AGENTS block, file reads, or command output — so echoed 'pallium ' text
  # (e.g. reading AGENTS.md or a usage dump) can't create a false positive.
  used=$(python3 - "$transcript" <<'PY'
import json, re, sys

cmds = []
def walk(node):
    if isinstance(node, dict):
        if node.get("type") == "tool_use" and node.get("name") == "Bash":
            cmd = (node.get("input") or {}).get("command")
            if isinstance(cmd, str):
                cmds.append(cmd)
        for v in node.values():
            walk(v)
    elif isinstance(node, list):
        for v in node:
            walk(v)

# stream-json is JSONL: one JSON object per line (system init, assistant/user
# messages whose content holds the tool_use blocks, then a final result). Parse
# each line independently and walk it; skip blank / unparseable lines.
with open(sys.argv[1]) as fh:
    for raw in fh:
        raw = raw.strip()
        if not raw:
            continue
        try:
            walk(json.loads(raw))
        except Exception:
            continue

def invokes_pallium(cmd):
    # Split on shell separators so `pallium` must be the command actually run,
    # not an argument to cat/ls (e.g. `ls pallium/`).
    for seg in re.split(r"&&|\|\||[;|]", cmd):
        seg = seg.strip()
        if seg == "pallium" or seg.startswith("pallium "):
            return True
    return False

print("true" if any(invokes_pallium(c) for c in cmds) else "false")
PY
)
  python3 -c "import json,sys;print(json.dumps({'id':sys.argv[1],'condition':sys.argv[2],'used_pallium_toolcall':sys.argv[3]=='true','transcript':sys.argv[4]}))" "$id" "$cond" "$used" "$transcript" >> "$out"
  echo "[$cond] $id → used_pallium=$used"
  rm -rf "$scratch" "$home"
done < "$here/tasks.jsonl"

echo "wrote $out"
