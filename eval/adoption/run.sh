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

  # KNOWN LIMITATION, found live running this eval for real (not fixed here,
  # see eval/adoption/README.md): isolating HOME gives each task a clean
  # Pallium store (~/.pallium/), which is the whole point — but on at least
  # one real setup, `claude -p` under that isolated HOME failed with
  # "Not logged in" every time, even after copying ~/.claude/.credentials.json
  # and ~/.claude.json into it. That account's actual session auth isn't in
  # either file (~/.claude/.credentials.json here holds only per-MCP-plugin
  # OAuth entries with empty tokens), so it likely resolves through the OS
  # keychain or another mechanism this script can't safely reproduce by
  # copying files. Net: run.sh's real-transcript path needs a login flow
  # that survives HOME isolation before it can be trusted as a source of
  # numbers — until then, treat its output as environment-dependent and
  # prefer the dogfooded eval.workflow.js path (which never isolates HOME)
  # for anything you intend to report.
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
  #
  # Mid-session decay (M3): a single `-p` invocation isn't one atomic action —
  # the agent chains many tool calls autonomously within it to finish a
  # multi-part task. First-call adoption (one preflight, then silently
  # reverting to manual work for the rest) is the exact gap logged in the
  # ledger's adoption-decay incident. Walking ALL tool calls in order (not
  # just pallium ones) and splitting at the midpoint gives an objective,
  # model-agnostic signal: did pallium usage survive into the back half of
  # the agent's own tool-call sequence, or only show up early.
  decay_json=$(python3 - "$transcript" <<'PY'
import json, re, sys

def invokes_pallium(cmd):
    # Split on shell separators so `pallium` must be the command actually run,
    # not an argument to cat/ls (e.g. `ls pallium/`).
    for seg in re.split(r"&&|\|\||[;|]", cmd):
        seg = seg.strip()
        if seg == "pallium" or seg.startswith("pallium "):
            return True
    return False

# Ordered list of every tool_use call, True where it's a pallium invocation —
# order matters here (unlike the old cmds-only list), so this walk collects
# EVERY tool_use node, not just Bash ones, to fairly represent how much total
# activity happened before/after the midpoint.
calls = []
def walk(node):
    if isinstance(node, dict):
        if node.get("type") == "tool_use":
            is_pallium = False
            if node.get("name") == "Bash":
                cmd = (node.get("input") or {}).get("command")
                if isinstance(cmd, str):
                    is_pallium = invokes_pallium(cmd)
            calls.append(is_pallium)
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

total = len(calls)
mid = total // 2
first_half, second_half = calls[:mid], calls[mid:]
first_pallium = sum(first_half)
second_pallium = sum(second_half)
used = any(calls)
# Decayed: reached for it early, then went the ENTIRE back half without it
# again — the "one preflight call, then manual for the rest" pattern,
# distinct from simply never using it at all (that's a plain recall miss,
# already scored by used_pallium_toolcall).
decayed = first_pallium > 0 and second_pallium == 0

print(json.dumps({
    "used_pallium_toolcall": used,
    "total_toolcalls": total,
    "pallium_toolcalls_first_half": first_pallium,
    "pallium_toolcalls_second_half": second_pallium,
    "decayed_mid_session": decayed,
}))
PY
)
  python3 -c "
import json, sys
decay = json.loads(sys.argv[3])
record = {'id': sys.argv[1], 'condition': sys.argv[2], 'transcript': sys.argv[4]}
record.update(decay)
print(json.dumps(record))
" "$id" "$cond" "$decay_json" "$transcript" >> "$out"
  used=$(python3 -c "import json,sys;print('true' if json.loads(sys.argv[1])['used_pallium_toolcall'] else 'false')" "$decay_json")
  decayed=$(python3 -c "import json,sys;print('true' if json.loads(sys.argv[1])['decayed_mid_session'] else 'false')" "$decay_json")
  echo "[$cond] $id → used_pallium=$used decayed_mid_session=$decayed"
  # Best-effort cleanup, not fatal: found live running this eval for real —
  # a lingering subprocess (a package-manager cache directory materialized
  # mid-run) can still be writing into $scratch/$home for a moment after
  # claude's own process exits, so a single `rm -rf` can race and return
  # non-zero. Under this script's `set -e`, that used to silently ABORT THE
  # WHOLE EVAL after whichever task happened to hit the race — every task
  # after it never ran, with no error surfaced beyond the rm noise. Retries
  # briefly, then falls back to `|| true` and a visible warning rather than
  # losing the rest of the suite over one directory that'll get reaped by
  # the OS's own tmp cleanup anyway.
  for attempt in 1 2 3; do
    rm -rf "$scratch" "$home" 2>/dev/null && break
    sleep 1
  done
  rm -rf "$scratch" "$home" 2>/dev/null || echo "[$cond] $id → warning: cleanup left some files behind (non-fatal, OS tmp cleanup will reap them): $scratch $home" >&2
done < "$here/tasks.jsonl"

echo "wrote $out"
