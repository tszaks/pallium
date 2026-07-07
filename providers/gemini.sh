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

# Approval-mode mapping. Only "edit" agents run in Pallium's isolated
# worktree, so only "edit" gets full auto-approval (--approval-mode=yolo —
# the modern, non-deprecated replacement for the old -y flag).
#
# "test"/"check"/"read-only" all execute against the live (non-isolated)
# checkout, so they get an explicit --approval-mode=default. This is
# deliberate even though it means test/check can't run shell commands
# unattended on this reference wrapper: Gemini's per-tool allowlist
# (--allowed-tools) is deprecated in favor of an external Policy Engine
# config file, and its own "auto_edit" mode still leaves shell commands
# gated behind confirmation, so there is no current stable, non-deprecated
# way to auto-approve just a test-runner command here without also opening
# up broader tool access. If you need Gemini to run tests unattended, either
# wire up a Policy Engine policy file scoped to your test command (see
# https://geminicli.com/docs/core/policy-engine) and pass it via --policy,
# or run test/check agents with isolation: "worktree" so full yolo is safe.
#
# We also do NOT use --approval-mode=plan for read-only, despite it being a
# read-only tool restriction: in non-interactive runs Gemini auto-approves
# its own enter_plan_mode/exit_plan_mode transitions, and exiting plan mode
# to "execute the plan" auto-switches to YOLO with no further confirmation
# (see https://geminicli.com/docs/cli/plan-mode/) — a worse failure mode
# than the unscoped-approval problem it would be solving here. Explicit
# --approval-mode=default fails closed instead: any tool needing
# confirmation errors out (CONFIRMATION_REQUIRED) rather than executing or
# hanging, and it overrides a user's local settings.json default (e.g.
# auto_edit/yolo) that would otherwise silently grant a "read-only" worker
# write access.
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit)
    ARGS+=(--approval-mode yolo)
    ;;
  *)
    ARGS+=(--approval-mode default)
    ;;
esac

if [ -n "${PALLIUM_WORKFLOW_MODEL:-}" ]; then
  ARGS+=(-m "$PALLIUM_WORKFLOW_MODEL")
fi

# stderr is left connected (not redirected to /dev/null) so auth/quota/
# permission failures reach Pallium's run logs instead of being discarded.
RAW=$(gemini "${ARGS[@]}")

# Write RAW to a temp file rather than an env var: a large response can
# exceed the OS argv/environment size limit (~128KiB on Linux), which would
# make the env-var approach fail with "Argument list too long" under
# `set -e` before ever writing the output file.
RAW_FILE=$(mktemp)
trap 'rm -f "$RAW_FILE"' EXIT
printf '%s' "$RAW" > "$RAW_FILE"

GEMINI_PROVIDER_RAW_FILE="$RAW_FILE" python3 - <<'PY'
import json
import os
import re

# NOTE: read RAW from a file path passed via env var, not stdin — this
# script body is itself fed to `python3 -` via a heredoc, so stdin is
# already consumed by the heredoc redirection and would read back empty.
with open(os.environ["GEMINI_PROVIDER_RAW_FILE"], "r") as f:
    text = f.read().strip()

fence = re.match(r"^```(?:json)?\s*(.*?)\s*```\s*$", text, re.S)
if fence:
    text = fence.group(1)

def extract_first_json_object(s):
    """Return the first brace-balanced {...} substring in s, honoring
    strings/escapes so braces inside string values don't confuse the scan.
    Using text.find('{')/text.rfind('}') instead would span into any braces
    that show up in trailing prose (e.g. '{"ok":true}\\nNote: {done}'),
    producing an invalid, unrecoverable candidate."""
    start = s.find("{")
    while start != -1:
        depth = 0
        in_string = False
        escape = False
        for i in range(start, len(s)):
            ch = s[i]
            if in_string:
                if escape:
                    escape = False
                elif ch == "\\":
                    escape = True
                elif ch == '"':
                    in_string = False
            else:
                if ch == '"':
                    in_string = True
                elif ch == "{":
                    depth += 1
                elif ch == "}":
                    depth -= 1
                    if depth == 0:
                        return s[start:i + 1]
        start = s.find("{", start + 1)
    return None

# When a schema is expected, try the whole response as JSON first; if that
# fails (e.g. trailing prose after a valid object, like `{...}\nDone.`),
# extract the first balanced {...} object and validate that instead, so
# Pallium's validation sees bare JSON either way.
if os.environ.get("PALLIUM_WORKFLOW_SCHEMA_FILE"):
    try:
        json.loads(text)
    except (json.JSONDecodeError, ValueError):
        candidate = extract_first_json_object(text)
        if candidate:
            try:
                json.loads(candidate)
                text = candidate
            except (json.JSONDecodeError, ValueError):
                pass

with open(os.environ["PALLIUM_WORKFLOW_OUTPUT_FILE"], "w") as f:
    f.write(text)
PY
