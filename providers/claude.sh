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
# exist for the session at all, so anything not listed simply isn't
# available. `--strict-mcp-config` (with no --mcp-config given) loads zero
# MCP servers, closing the same gap for a pre-approved MCP tool. But
# `--tools` only restricts at the tool-category level — it can't stop a
# project's own `.claude/settings.json` from pre-approving a broader Bash
# pattern (or defining a SessionStart hook that runs before any of this
# gating even applies), since that config is still loaded and merged by
# default. `--setting-sources user` closes that: it tells Claude to load
# ONLY the user's own global settings, never the project's
# `.claude/settings.json` or a `.claude/settings.local.json` in the
# checkout, so a malicious/compromised repo can't inject either a Bash
# allow rule or a hook. `--safe-mode` adds a second layer disabling hooks/
# plugins/custom commands generally. Applied to ALL modes, including
# "edit": Pallium's worktree isolation only gives edit agents a disposable
# checkout, it doesn't sandbox a hook/MCP process the repo's own config
# could launch, which could still reach outside the worktree onto the real
# host — isolation limits blast radius for the model's own actions, not
# for a project config that runs before the model does anything. Verified
# this doesn't break edit mode's actual purpose: a live test with all of
# --safe-mode/--setting-sources user/--strict-mcp-config/--tools added
# still successfully created a file via Edit/Write and returned normal
# output. Trade-off worth knowing: --safe-mode also disables reading the
# project's CLAUDE.md, so an edit agent loses that convention/context too.
#
# `--permission-mode plan` (read-only/test/check only) is also required,
# and this reverses an earlier decision in this file to avoid it. Verified
# empirically: even with --tools/--strict-mcp-config/--setting-sources/
# --disallowedTools all present, if the *effective* permission mode ends
# up being something permissive (e.g. the invoking user's own
# ~/.claude/settings.json sets permissions.defaultMode to bypassPermissions
# or auto — a setting --setting-sources user deliberately still honors,
# since it's the user's own trusted config), an out-of-allowlist Bash
# command (e.g. a bare `touch file`) still executes; --disallowedTools
# alone does not stop this because it only denies the tools explicitly
# listed there (Edit/Write/NotebookEdit), not arbitrary Bash.
# --permission-mode plan is the only mode that reliably fails an unapproved
# action closed regardless of the effective default: verified it still
# runs already-allowlisted commands normally (a plain read-only query, and
# an allowlisted `go vet` test command, both completed and returned real
# output), while a request to write an unapproved file was refused with a
# plain-text explanation instead of executing. `manual`/`dontAsk`/`default`
# were all tested and do NOT reliably block this the same way. Not used
# for "edit", which needs acceptEdits to actually write without prompting.
#
# `go build`/`go test`/`go vet` were dropped from the non-isolated
# TEST_TOOLS allowlist for the same unfixable-by-prefix reason as
# find/gofmt/rg: verified empirically that neither a wildcard pattern
# (`Bash(go test:*)`) nor an exact-looking one with no trailing `:*`
# (`Bash(go test ./...)`) stops extra flags from being appended and
# executed — `go test ./... -exec /bin/echo` still ran under both
# --permission-mode plan and --permission-mode default with only
# `Bash(go test ./...)` allowed, proving Claude's Bash pattern matching is
# a prefix match regardless of a trailing wildcard, not an anchored exact
# match. `go build -toolexec`/`-o`, `go test -exec`/`-o`/profile-output
# flags, and `go vet -vettool` can all write or execute arbitrary things,
# so there is no pattern that allows the safe form without also allowing
# the dangerous one. They remain available in EDIT_TOOLS (isolated
# worktree, where the model already has full Edit/Write access and the
# blast radius is a disposable checkout) but not in the live-checkout
# TEST_TOOLS. npm test/pytest stay in TEST_TOOLS — Go-specific test/check
# workers on a live checkout currently have no safe built-in test-runner
# entry; add one only behind a real sandbox, not a Bash allowlist pattern.
# Adjust the allowlist for your stack.
READ_TOOLS="Read,Grep,Glob,LS,Bash(ls:*),Bash(cat:*),Bash(grep:*),Bash(wc:*),Bash(go doc:*)"
TEST_TOOLS="$READ_TOOLS,Bash(npm test:*),Bash(pytest:*)"
EDIT_TOOLS="$TEST_TOOLS,Bash(go test:*),Bash(go build:*),Bash(go vet:*),Edit,Write"
NON_EDIT_HARD_TOOLS="Read,Grep,Glob,LS,Bash"
EDIT_HARD_TOOLS="Read,Grep,Glob,LS,Bash,Edit,Write"

# Network access is OFF by default. Pallium sets PALLIUM_WORKFLOW_NETWORK=1
# only when this agent both requested `network: true` AND the run was started
# with `--allow-network` (the operator ceiling); it is "0" otherwise. When
# granted, expose ONLY WebFetch — Claude's native, side-effect-free fetch —
# for EVERY mode, including "edit". Raw `Bash(curl:*)`/`Bash(gh:*)` used to be
# granted to "edit" on the theory that its worktree contains the blast radius,
# but the worktree is a `cwd` change, not an OS-level network sandbox: it
# limits which FILES an agent can touch, not what a process launched from
# inside it can reach or exfiltrate over the network. A prompt-injected edit
# agent with raw curl/gh could read anything in its worktree (or the wider
# host, since nothing here sandboxes network egress at the OS level) and POST
# it out, or run `gh api -X POST` against the operator's real GitHub account.
# WebFetch has no such escape hatch, so it's the only networked tool granted
# anywhere.
#
# WebFetch is also added to the hard `--tools` set below because `--tools` is a
# hard cap on which built-in tools exist at all — listing WebFetch only in
# --allowedTools would leave it unavailable. The mode-based filesystem gating
# (Edit/Write still denied for non-edit modes) is left intact; network only
# widens egress, not write scope.
NET_TOOLS=""
if [ "${PALLIUM_WORKFLOW_NETWORK:-0}" = "1" ]; then
  NET_TOOLS=",WebFetch"
  NON_EDIT_HARD_TOOLS="$NON_EDIT_HARD_TOOLS,WebFetch"
  EDIT_HARD_TOOLS="$EDIT_HARD_TOOLS,WebFetch"
fi

HARDENING_ARGS=(--safe-mode --setting-sources user --strict-mcp-config)
NON_EDIT_ISOLATION_ARGS=("${HARDENING_ARGS[@]}" --permission-mode plan --tools "$NON_EDIT_HARD_TOOLS")
case "${PALLIUM_WORKFLOW_MODE:-read-only}" in
  edit)
    # EDIT_HARD_TOOLS keeps bare, unscoped "Bash" in the hard --tools set
    # (verified empirically: --tools only recognizes plain tool names like
    # "Bash", not "Bash(cmd:*)" patterns — passing a patterned entry there
    # silently drops the whole Bash tool, which would break go build/test/vet
    # too). That means --allowedTools' toolchain scoping alone does not stop
    # an out-of-allowlist Bash command: verified empirically that under
    # --permission-mode acceptEdits, a `curl` call not present in
    # --allowedTools still executed with zero permission_denials, because
    # acceptEdits only guarantees auto-accept for file edits, not a hard deny
    # for everything else (same gap already documented above for
    # manual/dontAsk/default). --disallowedTools is what actually closes it:
    # verified empirically that adding `Bash(curl:*),Bash(gh:*)` there blocks
    # both with an explicit permission denial even under acceptEdits, while
    # leaving allowlisted commands like `go build` unaffected. Applied
    # unconditionally (not just when networked), since bare Bash already
    # permits curl/gh regardless of PALLIUM_WORKFLOW_NETWORK — that env var is
    # only this wrapper's own tool-scoping signal, not an OS-level network
    # block, so the exfiltration channel exists whether or not this run
    # requested network.
    PERM_ARGS=("${HARDENING_ARGS[@]}" --tools "$EDIT_HARD_TOOLS" --permission-mode acceptEdits --allowedTools "$EDIT_TOOLS$NET_TOOLS" --disallowedTools "Bash(curl:*),Bash(gh:*)")
    ;;
  test|check)
    # NOTE: npm test/pytest execute the repo's own test code, which is
    # inherently capable of doing anything the language runtime can do
    # (writing files, deleting things, network calls) — that's true of any
    # test suite, isolated or not, and no allowlist or permission-mode can
    # change it without breaking the ability to actually run the tests.
    # Isolation (isolation: "worktree") limits the blast radius by giving
    # the run a disposable checkout; it doesn't and can't limit the test
    # code itself. Only use test/check against repos whose test code you
    # trust, and prefer isolation: "worktree" for anything you don't.
    PERM_ARGS=("${NON_EDIT_ISOLATION_ARGS[@]}" --allowedTools "$TEST_TOOLS$NET_TOOLS" --disallowedTools "Edit,Write,NotebookEdit")
    ;;
  *)
    PERM_ARGS=("${NON_EDIT_ISOLATION_ARGS[@]}" --allowedTools "$READ_TOOLS$NET_TOOLS" --disallowedTools "Edit,Write,NotebookEdit")
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
    parsed = json.loads(raw)
    # `claude --output-format json` is documented as "single result" (a
    # dict envelope), but verified empirically against the installed CLI
    # (2.1.203) that it actually returns a JSON array of the full event
    # stream (system/assistant/user/result events) instead. Handle both
    # shapes: for the array case, the last event with type "result" carries
    # the same result/total_cost_usd/usage fields the single-envelope shape
    # has at its top level. Without this, the array shape fell through the
    # old `isinstance(envelope, dict)` check and the wrapper wrote the
    # entire raw event stream to the output file instead of the answer.
    envelope = None
    if isinstance(parsed, dict):
        envelope = parsed
    elif isinstance(parsed, list):
        for item in reversed(parsed):
            if isinstance(item, dict) and item.get("type") == "result":
                envelope = item
                break
    if envelope is not None:
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
