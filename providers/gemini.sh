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
#
# SECURITY NOTE (unresolved upstream limitation): Gemini CLI runs
# SessionStart hooks from the target repo's `.gemini/settings.json` at
# startup, before this wrapper's approval-mode ever gets a chance to gate
# anything (https://geminicli.com/docs/hooks/). There is currently no
# documented CLI flag or env var to disable hook loading, and a worktree
# doesn't help either — a worktree checks out the same tracked files,
# including `.gemini/settings.json`, so a malicious/compromised hook in the
# repo runs regardless of mode or isolation. Only run this provider against
# repos whose committed Gemini config you've reviewed; approval-mode alone
# does not make it safe on an untrusted checkout.

set -euo pipefail

PROMPT=$(cat "$PALLIUM_WORKFLOW_PROMPT_FILE")

# The schema root isn't necessarily an object (Pallium's validator accepts
# array and scalar roots too), so the instruction says "valid JSON", not "a
# JSON object" — forcing "object" wording would contradict an array/scalar
# schema.
if [ -n "${PALLIUM_WORKFLOW_SCHEMA_FILE:-}" ] && [ -s "${PALLIUM_WORKFLOW_SCHEMA_FILE:-/dev/null}" ]; then
  SCHEMA=$(cat "$PALLIUM_WORKFLOW_SCHEMA_FILE")
  PROMPT="$PROMPT

Respond with ONLY valid JSON conforming to this JSON Schema — no markdown fences, no prose before or after:
$SCHEMA"
fi

# -p/--prompt is the documented non-interactive form (a bare positional query
# does not force non-interactive mode the way -p does). Its value is kept
# short and the actual (possibly large) prompt is piped via stdin instead:
# Gemini's own docs describe -p's value as "appended to input on stdin (if
# any)", and the documented pattern is `cat file | gemini -p "instruction"`.
# Passing the whole prompt as -p's argv value, like a large PROMPT_FILE with
# many files/findings, risks the OS argument-list size limit (~128KiB on
# Linux) and exec failing with E2BIG before Gemini even starts.
ARGS=(-p "Carry out the instructions in the piped input above exactly.")

# Approval-mode mapping. "edit" agents always run in Pallium's isolated
# worktree, so "edit" always gets full auto-approval (--approval-mode=yolo —
# the modern, non-deprecated replacement for the old -y flag).
#
# "test"/"check" also run isolated when the caller passes
# isolation: "worktree" explicitly (Pallium's runtime creates a worktree for
# agent.Mode=="edit" OR agent.Isolation=="worktree" — see
# internal/workflow/runtime.go). Detect that case from the env vars Pallium
# already sets: if PALLIUM_WORKFLOW_CWD differs from PALLIUM_WORKFLOW_REPO,
# the agent's cwd was redirected into a worktree, so it's safe to grant
# yolo there too. Otherwise test/check (and read-only, always) run against
# the live checkout and get an explicit --approval-mode=default. This is
# deliberate even though it means non-isolated test/check can't run shell
# commands unattended on this reference wrapper: Gemini's per-tool allowlist
# (--allowed-tools) is deprecated in favor of an external Policy Engine
# config file, and its own "auto_edit" mode still leaves shell commands
# gated behind confirmation, so there is no current stable, non-deprecated
# way to auto-approve just a test-runner command here without also opening
# up broader tool access. If you need Gemini to run tests unattended on the
# live checkout, wire up a Policy Engine policy file scoped to your test
# command instead (see https://geminicli.com/docs/core/policy-engine) and
# pass it via --policy.
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
IS_ISOLATED_WORKTREE=0
if [ -n "${PALLIUM_WORKFLOW_REPO:-}" ] && [ "${PALLIUM_WORKFLOW_CWD:-}" != "${PALLIUM_WORKFLOW_REPO:-}" ]; then
  IS_ISOLATED_WORKTREE=1
fi
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit)
    ARGS+=(--approval-mode yolo)
    ;;
  test|check)
    if [ "$IS_ISOLATED_WORKTREE" = "1" ]; then
      ARGS+=(--approval-mode yolo)
    else
      ARGS+=(--approval-mode default)
    fi
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
# The actual prompt is piped via stdin (see the ARGS comment above) rather
# than living in argv.
RAW=$(printf '%s' "$PROMPT" | gemini "${ARGS[@]}")

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

def extract_first_json_value(s):
    """Return the first balanced {...} or [...] substring in s, honoring
    strings/escapes so brackets inside string values don't confuse the
    scan, and supporting array-rooted schemas as well as object-rooted
    ones (Pallium's validator accepts either). Using text.find('{')/
    text.rfind('}') instead would span into any brackets that show up in
    trailing prose (e.g. '{"ok":true}\\nNote: {done}'), producing an
    invalid, unrecoverable candidate."""
    opens, closes = "{[", "}]"
    pairs = {"{": "}", "[": "]"}
    pos = 0
    while True:
        start = next((i for i in range(pos, len(s)) if s[i] in opens), None)
        if start is None:
            return None
        stack = []
        in_string = False
        escape = False
        matched = None
        for i in range(start, len(s)):
            ch = s[i]
            if in_string:
                if escape:
                    escape = False
                elif ch == "\\":
                    escape = True
                elif ch == '"':
                    in_string = False
                continue
            if ch == '"':
                in_string = True
            elif ch in opens:
                stack.append(pairs[ch])
            elif ch in closes:
                if not stack or stack[-1] != ch:
                    break
                stack.pop()
                if not stack:
                    matched = s[start:i + 1]
                    break
        if matched:
            return matched
        pos = start + 1

# Structured output only: strip accidental markdown fences and, if the
# response isn't already bare JSON, recover the first balanced JSON value
# from surrounding prose. Skipped entirely when no schema was requested, so
# an unstructured response that happens to BE a fenced code block (e.g. a
# Markdown snippet the caller actually wants) is returned unmodified.
if os.environ.get("PALLIUM_WORKFLOW_SCHEMA_FILE"):
    fence = re.match(r"^```(?:json)?\s*(.*?)\s*```\s*$", text, re.S)
    if fence:
        text = fence.group(1)
    try:
        json.loads(text)
    except (json.JSONDecodeError, ValueError):
        candidate = extract_first_json_value(text)
        if candidate:
            try:
                json.loads(candidate)
                text = candidate
            except (json.JSONDecodeError, ValueError):
                pass

with open(os.environ["PALLIUM_WORKFLOW_OUTPUT_FILE"], "w") as f:
    f.write(text)
PY
