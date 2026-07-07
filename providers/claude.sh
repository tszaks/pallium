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
# emit bare conforming JSON. Pallium validates the result on its side. The
# schema root isn't necessarily an object (Pallium's validator accepts array
# and scalar roots too), so the instruction says "valid JSON", not "a JSON
# object" — forcing "object" wording would contradict an array/scalar schema.
if [ -n "${PALLIUM_WORKFLOW_SCHEMA_FILE:-}" ] && [ -s "${PALLIUM_WORKFLOW_SCHEMA_FILE:-/dev/null}" ]; then
  SCHEMA=$(cat "$PALLIUM_WORKFLOW_SCHEMA_FILE")
  PROMPT="$PROMPT

Respond with ONLY valid JSON conforming to this JSON Schema — no markdown fences, no prose before or after:
$SCHEMA"
fi

MODEL_ARGS=()
if [ -n "${PALLIUM_WORKFLOW_MODEL:-}" ]; then
  MODEL_ARGS=(--model "$PALLIUM_WORKFLOW_MODEL")
fi

# Mode mapping. Only "edit" agents run in an isolated worktree (per Pallium's
# runtime), so only "edit" gets file-write tools. "test"/"check"/"read-only"
# all execute against the live checkout and must never be able to mutate it —
# they get a read/search + test-runner toolchain allowlist, plus an explicit
# --disallowedTools denylist so a user's global Claude Code permission-mode
# default (acceptEdits/auto/bypassPermissions) can't grant edits anyway.
# NOTE: --allowedTools only skips the confirmation prompt for a tool, it does
# not sandbox it — a Bash(cmd:*) prefix match still runs whatever args follow.
# Keep entries to commands with no built-in mutate/execute capability: `find`
# was dropped entirely (its own `-delete`/`-exec` actions can remove or run
# arbitrary things, and no `Bash(find ...:*)` prefix can rule that out since
# the flags can be appended after any fixed prefix — use the built-in Glob
# tool for file-finding instead), so was `gofmt -l` (the same problem:
# `gofmt -l -w .` still matches a `Bash(gofmt -l:*)` prefix and writes files),
# and so was `rg` (ripgrep's own `--pre COMMAND` runs an arbitrary command
# per matched file — e.g. `rg --pre rm pattern .` — same unfixable-by-prefix
# problem; the built-in Grep tool already covers content search). The old
# blanket `npx:*` entry was dropped for the same reason: npx executes
# arbitrary, possibly unpinned/unaudited packages. Add a narrower, genuinely
# non-mutating entry only if you've confirmed it can't be abused.
#
# --allowedTools/--disallowedTools alone also can't protect against a
# checked-in project or user Claude config that has already pre-approved a
# broader tool (a permissive `.claude/settings.json` Bash pattern, or a
# side-effecting MCP tool) — that config would still apply underneath
# --allowedTools, since it only pre-approves, it doesn't define the
# available set. `--tools` is a hard restriction on which built-in tools
# exist for the session at all, so Edit/Write/NotebookEdit and anything else
# not listed simply aren't available. `--strict-mcp-config` (with no
# --mcp-config given) loads zero MCP servers, closing the same gap for a
# pre-approved MCP tool. But `--tools` only restricts at the tool-category
# level — it can't stop a project's own `.claude/settings.json` from
# pre-approving a broader Bash pattern (or defining a SessionStart hook that
# runs before any of this gating even applies), since that config is still
# loaded and merged by default. `--setting-sources user` closes that: it
# tells Claude to load ONLY the user's own global settings, never the
# project's `.claude/settings.json` or a `.claude/settings.local.json` in
# the checkout, so a malicious/compromised repo can't inject either a Bash
# allow rule or a hook into a "read-only" session. `--safe-mode` adds a
# second layer disabling hooks/plugins/custom commands generally.
#
# `--permission-mode plan` is also required here, and this reverses an
# earlier decision in this file to avoid it. Verified empirically: even
# with --tools/--strict-mcp-config/--setting-sources/--disallowedTools all
# present, if the *effective* permission mode ends up being something
# permissive (e.g. the invoking user's own ~/.claude/settings.json sets
# permissions.defaultMode to bypassPermissions or auto — a setting
# --setting-sources user deliberately still honors, since it's the user's
# own trusted config), an out-of-allowlist Bash command (e.g. a bare
# `touch file`) still executes; --disallowedTools alone does not stop this
# because it only denies the tools explicitly listed there (Edit/Write/
# NotebookEdit), not arbitrary Bash. --permission-mode plan is the only
# mode that reliably fails an unapproved action closed regardless of the
# effective default: verified it still runs already-allowlisted commands
# normally (a plain read-only query, and an allowlisted `go vet` test
# command, both completed and returned real output), while a request to
# write an unapproved file was refused with a plain-text explanation
# instead of executing. `manual`/`dontAsk`/`default` were all tested and do
# NOT reliably block this the same way.
# Adjust the allowlist for your stack.
READ_TOOLS="Read,Grep,Glob,LS,Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(wc:*),Bash(go doc:*)"
TEST_TOOLS="$READ_TOOLS,Bash(go test:*),Bash(go build:*),Bash(go vet:*),Bash(npm test:*),Bash(pytest:*)"
EDIT_TOOLS="$TEST_TOOLS,Edit,Write"
NON_EDIT_HARD_TOOLS="Read,Grep,Glob,LS,Bash"
NON_EDIT_ISOLATION_ARGS=(--safe-mode --setting-sources user --permission-mode plan --tools "$NON_EDIT_HARD_TOOLS" --strict-mcp-config)
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit)
    PERM_ARGS=(--permission-mode acceptEdits --allowedTools "$EDIT_TOOLS")
    ;;
  test|check)
    # NOTE: go test/npm test/pytest execute the repo's own test code, which
    # is inherently capable of doing anything the language runtime can do
    # (writing files, deleting things, network calls) — that's true of any
    # test suite, isolated or not, and no allowlist or permission-mode can
    # change it without breaking the ability to actually run the tests.
    # Isolation (isolation: "worktree") limits the blast radius by giving
    # the run a disposable checkout; it doesn't and can't limit the test
    # code itself. Only use test/check against repos whose test code you
    # trust, and prefer isolation: "worktree" for anything you don't.
    PERM_ARGS=("${NON_EDIT_ISOLATION_ARGS[@]}" --allowedTools "$TEST_TOOLS" --disallowedTools "Edit,Write,NotebookEdit")
    ;;
  *)
    PERM_ARGS=("${NON_EDIT_ISOLATION_ARGS[@]}" --allowedTools "$READ_TOOLS" --disallowedTools "Edit,Write,NotebookEdit")
    ;;
esac

# JSON envelope gives us the result text plus token usage in one call. stderr
# is left connected (not redirected to /dev/null) so auth/quota/permission
# failures reach Pallium's run logs instead of being silently discarded. The
# prompt is piped via stdin rather than passed as a `-p` argument: a large
# prompt (many files/findings) as a single argv string can exceed the OS
# argument-list size limit (~128KiB on Linux) and fail the exec itself with
# E2BIG before the CLI even starts. `claude -p` (no value) reads the prompt
# from stdin when one isn't given positionally.
RAW=$(printf '%s' "$PROMPT" | claude -p --output-format json ${MODEL_ARGS[@]+"${MODEL_ARGS[@]}"} ${PERM_ARGS[@]+"${PERM_ARGS[@]}"})

# Write RAW to a temp file rather than an env var: a large review or
# structured-JSON response can exceed the OS argv/environment size limit
# (~128KiB on Linux), which would make the env-var approach fail with
# "Argument list too long" under `set -e` before ever writing the output file.
RAW_FILE=$(mktemp)
trap 'rm -f "$RAW_FILE"' EXIT
printf '%s' "$RAW" > "$RAW_FILE"

CLAUDE_PROVIDER_RAW_FILE="$RAW_FILE" python3 - <<'PY'
import json
import os
import re

# NOTE: read RAW from a file path passed via env var, not stdin — this
# script body is itself fed to `python3 -` via a heredoc, so stdin is
# already consumed by the heredoc redirection and would read back empty.
with open(os.environ["CLAUDE_PROVIDER_RAW_FILE"], "r") as f:
    raw = f.read().strip()
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

def extract_first_json_value(s):
    """Return the first balanced {...} or [...] substring in s that
    actually parses as JSON, honoring strings/escapes so brackets inside
    string values don't confuse the scan, and supporting array-rooted
    schemas as well as object-rooted ones (Pallium's validator accepts
    either). Using text.find('{')/text.rfind('}') instead would span into
    any brackets that show up in trailing prose (e.g.
    '{"ok":true}\\nNote: {done}'), producing an invalid, unrecoverable
    candidate. Keeps scanning past a balanced-but-non-JSON match too (e.g.
    bracketed prose like 'Here is [the JSON]: {"ok":true}' — the first
    balanced span, '[the JSON]', isn't valid JSON, so this moves on to the
    next '{'/'[' rather than giving up)."""
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
            try:
                json.loads(matched)
                return matched
            except (json.JSONDecodeError, ValueError):
                pass
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
            text = candidate

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
