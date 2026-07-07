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

# -p/--prompt is the documented non-interactive form (a bare positional query
# does not force non-interactive mode the way -p does).
ARGS=(-p "$PROMPT")

# -y auto-approves tool use. Only "edit"/"test"/"check" agents should get it:
# "edit" runs in an isolated worktree Pallium creates, and "test"/"check" need
# to execute the test-runner toolchain unattended. "read-only" (default/unset)
# runs against the live checkout with no isolation, so it must NOT auto-approve
# writes or shell execution there.
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit|test|check)
    ARGS+=(-y)
    ;;
esac

if [ -n "${PALLIUM_WORKFLOW_MODEL:-}" ]; then
  ARGS+=(-m "$PALLIUM_WORKFLOW_MODEL")
fi

RAW=$(gemini "${ARGS[@]}" 2>/dev/null)

GEMINI_PROVIDER_RAW="$RAW" python3 - <<'PY'
import json
import os
import re

# NOTE: read RAW from an env var, not stdin — this script body is itself
# fed to `python3 -` via a heredoc, so stdin is already consumed by the
# heredoc redirection and would read back empty.
text = os.environ.get("GEMINI_PROVIDER_RAW", "").strip()

fence = re.match(r"^```(?:json)?\s*(.*?)\s*```\s*$", text, re.S)
if fence:
    text = fence.group(1)

# When a schema is expected, try the whole response as JSON first; if that
# fails (e.g. trailing prose after a valid object, like `{...}\nDone.`),
# extract the outermost {...} substring and validate that instead, so
# Pallium's validation sees bare JSON either way.
if os.environ.get("PALLIUM_WORKFLOW_SCHEMA_FILE"):
    try:
        json.loads(text)
    except (json.JSONDecodeError, ValueError):
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
