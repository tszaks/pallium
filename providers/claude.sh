#!/bin/bash
# Pallium workflow provider: Claude Code headless workers.
#
# Setup:
#   export PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND="$(pwd)/providers/claude.sh"
# Usage in a workflow script:
#   await agent("Review the auth middleware", { provider: "claude", mode: "read-only" })
#
# Requires the Claude Code CLI (`claude`) on PATH and an authenticated session.
# See providers/README.md for the full provider environment contract.

set -euo pipefail

PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")

# Structured output: Pallium hands us the JSON schema; instruct the model to
# emit bare conforming JSON. Pallium validates the result on its side.
if [ -n "${PALLIUM_WORKFLOW_SCHEMA_FILE:-}" ] && [ -s "${PALLIUM_WORKFLOW_SCHEMA_FILE:-/dev/null}" ]; then
  SCHEMA=$(cat "$PALLIUM_WORKFLOW_SCHEMA_FILE")
  PROMPT="$PROMPT

Respond with ONLY a single JSON object conforming to this JSON Schema — no markdown fences, no prose before or after:
$SCHEMA"
fi

MODEL_ARGS=()
if [ -n "${PALLIUM_WORKFLOW_MODEL:-}" ]; then
  MODEL_ARGS=(--model "$PALLIUM_WORKFLOW_MODEL")
fi

# Mode mapping. read-only gets read/search tools; edit/test/check additionally
# get file edits (auto-accepted, contained to the workspace Pallium set as cwd)
# and a narrow Go/JS/Python toolchain allowlist. Adjust for your stack.
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit|test|check)
    PERM_ARGS=(--permission-mode acceptEdits --allowedTools "Read,Grep,Glob,LS,Edit,Write,Bash(go test:*),Bash(go build:*),Bash(go vet:*),Bash(gofmt:*),Bash(npm test:*),Bash(npx:*),Bash(pytest:*),Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(rg:*),Bash(find:*),Bash(wc:*)")
    ;;
  *)
    PERM_ARGS=(--allowedTools "Read,Grep,Glob,LS,Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(rg:*),Bash(find:*),Bash(wc:*),Bash(go doc:*)")
    ;;
esac

# JSON envelope gives us the result text plus token usage in one call.
RAW=$(claude -p "$PROMPT" --output-format json ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} ${PERM_ARGS[@]+"${PERM_ARGS[@]}"} 2>/dev/null)

printf '%s' "$RAW" | python3 - <<'PY'
import json
import os
import re
import sys

raw = sys.stdin.read().strip()
result = raw
usage = None
try:
    envelope = json.loads(raw)
    if isinstance(envelope, dict):
        result = envelope.get("result", raw)
        cost = envelope.get("total_cost_usd")
        u = envelope.get("usage") or {}
        if cost is not None or u:
            usage = {
                "input_tokens": u.get("input_tokens"),
                "output_tokens": u.get("output_tokens"),
                "cost_usd": cost,
            }
except (json.JSONDecodeError, ValueError):
    pass

text = (result or "").strip()

# Strip accidental markdown fences.
fence = re.match(r"^```(?:json)?\s*(.*?)\s*```\s*$", text, re.S)
if fence:
    text = fence.group(1)

# When a schema is expected, extract the outermost JSON object even if the
# model wrapped it in prose, so Pallium's validation sees bare JSON.
if os.environ.get("PALLIUM_WORKFLOW_SCHEMA_FILE") and not text.startswith("{"):
    start, end = text.find("{"), text.rfind("}")
    if start != -1 and end > start:
        candidate = text[start:end + 1]
        try:
            json.loads(candidate)
            text = candidate
        except (json.JSONDecodeError, ValueError):
            pass

with open(os.environ["PALLIUM_WORKFLOW_OUTPUT_FILE"], "w") as f:
    f.write(text)

# Optional usage reporting (supported by newer Pallium versions; harmless otherwise).
usage_file = os.environ.get("PALLIUM_WORKFLOW_USAGE_FILE")
if usage_file and usage:
    try:
        with open(usage_file, "w") as f:
            json.dump({k: v for k, v in usage.items() if v is not None}, f)
    except OSError:
        pass
PY
