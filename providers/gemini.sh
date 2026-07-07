#!/bin/bash
# Pallium workflow provider: Gemini CLI workers.
#
# Setup:
#   export PALLIUM_WORKFLOW_PROVIDER_GEMINI_COMMAND="$(pwd)/providers/gemini.sh"
# Usage in a workflow script:
#   await agent("Summarize the auth flow", { provider: "gemini", mode: "read-only" })
#
# Requires the Gemini CLI (`gemini`) on PATH and an authenticated session.
# See providers/README.md for the full provider environment contract.

set -euo pipefail

PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")

if [ -n "${PALLIUM_WORKFLOW_SCHEMA_FILE:-}" ] && [ -s "${PALLIUM_WORKFLOW_SCHEMA_FILE:-/dev/null}" ]; then
  SCHEMA=$(cat "$PALLIUM_WORKFLOW_SCHEMA_FILE")
  PROMPT="$PROMPT

Respond with ONLY a single JSON object conforming to this JSON Schema — no markdown fences, no prose before or after:
$SCHEMA"
fi

# -y auto-approves tool use; the Gemini CLI operates in the cwd Pallium set,
# which is an isolated worktree for edit-mode agents.
RAW=$(gemini -y "$PROMPT" 2>/dev/null)

printf '%s' "$RAW" | python3 - <<'PY'
import json
import os
import re
import sys

text = sys.stdin.read().strip()

fence = re.match(r"^```(?:json)?\s*(.*?)\s*```\s*$", text, re.S)
if fence:
    text = fence.group(1)

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
PY
