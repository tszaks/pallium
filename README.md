# pallium

[![npm version](https://img.shields.io/npm/v/pallium.svg)](https://www.npmjs.com/package/pallium)
[![npm downloads](https://img.shields.io/npm/dm/pallium.svg)](https://www.npmjs.com/package/pallium)
[![GitHub release](https://img.shields.io/github/v/release/tszaks/pallium?sort=semver)](https://github.com/tszaks/pallium/releases/latest)
[![GitHub Packages](https://img.shields.io/badge/GitHub%20Packages-%40tszaks%2Fpallium-24292f?logo=github)](https://github.com/tszaks/pallium/pkgs/npm/pallium)

`pallium` is a local-first CLI for AI-powered coding workflows.

It gives an LLM fast repo context before, during, and after edits:

- what files are risky
- what else is likely to move
- what tests are most relevant
- what focused test command to run first, plus the safer fallback
- what fast, safe, and full verification steps to run
- how fresh the local index is, and what evidence the guidance is based on
- what the blast radius probably is
- what action an agent should take next
- whether the current task drifted outside its planned scope
- what related agent sessions may explain the current repo or files
- what verification commands actually passed or failed recently
- what changed in the working tree right now

## Why It Matters

LLMs are good at writing code and bad at remembering repository context.

That leads to common mistakes:

- editing a risky file in isolation
- missing related files
- skipping the most useful tests
- handing work off without a clean summary

`pallium` exists to lower those surprises.

## Core Commands

```bash
pallium index
pallium doctor
pallium version
pallium explain <path>
pallium safe <path>
pallium plan <path>
pallium changed-now
pallium review [base-ref]
pallium verify <fast|safe|full>
pallium handoff [base-ref]
pallium task start "Tighten auth flow" src/auth cmd
pallium task show
pallium workflow run "review this branch with a workflow"
pallium workflow show <run-id>
```

Use `--json` with any command for agent-friendly output.

## Agent Session Memory

`pallium` can also index Codex CLI transcripts from `~/.codex/sessions/**/*.jsonl` plus metadata from `~/.codex/state_5.sqlite`, and Claude Code transcripts from `~/.claude/projects/**/*.jsonl`.

```bash
pallium sessions live --details
pallium sessions index
pallium sessions index --safety-buffer 30m
pallium sessions index --force
pallium sessions index --provider claude
pallium sessions index --provider codex
pallium sessions list --limit 20
pallium sessions search "MCP auth" --limit 10
pallium sessions search "MCP auth" --hybrid --limit 10
pallium sessions related . --file cmd/app.go --limit 5
pallium sessions grep "Timed out waiting for PGLite lock" --limit 20
pallium sessions show <session-id> --transcript
pallium sessions embed --batch-size 8
pallium sessions semantic "find the session where we debugged MCP startup failures"
pallium sessions stats
```

## Console Coordination

`pallium console` is an experimental local control plane for agent sessions.
It can inspect live Codex and Claude Code sessions, coordinate manifests,
handoffs, file claims, action requests, authority gates, and review gates.

This release also adds Pallium-owned process control. Pallium can spawn a
foreground or background PTY-backed session, persist its metadata, capture its
log, read its output later, and interrupt it as a process group.

```bash
pallium console ls --details
pallium console run --id owned-demo -- /bin/sh -c 'echo hello'
pallium console run --background --id owned-worker -- /bin/sh -c 'sleep 30'
pallium console owned list
pallium console owned show owned-worker
pallium console read owned-worker --tail 50
pallium console interrupt owned-worker
```

The control boundary is intentionally conservative: this release controls
sessions that Pallium spawned itself. It does not inject commands into arbitrary
existing Codex or Claude Code sessions.

## Dynamic Workflows

`pallium workflow` is a local Codex workflow runner inspired by Claude Code
dynamic workflows. A workflow is a JavaScript file with a `meta` block plus
helpers such as `phase()`, `await agent()`, `await check()`, `parallel()`, and
`await pipeline()`. The runtime stores the run, phases, worker outputs,
generated script, and patches in Pallium's local control-plane database and
`~/.pallium/workflow-runs/`, so normal workflow runs do not dirty the target
repository.

```bash
pallium workflow run "review this branch for correctness issues"
pallium workflow run --script .pallium/workflows/review.js "review this branch"
pallium workflow run --workflow review-branch "review this branch"
pallium workflow run /review-branch "review this branch"
pallium workflow run --background "audit route handlers for missing auth"
pallium workflow list
pallium workflow show <run-id>
pallium workflow read <run-id>
pallium workflow watch <run-id>
pallium workflow save <run-id> --name review-branch
pallium workflow apply <run-id>
```

Workers run through `codex exec`. Read-only agents use a read-only sandbox;
edit agents run in isolated git worktrees under `~/.pallium/workflow-runs/` and
produce patches that are applied back to the target checkout automatically when
the workflow completes successfully. `workflow apply` remains as an idempotent
retry command for older or interrupted runs. `workflow save` is the only command
in this group that intentionally writes a reusable workflow into the target
repo. Set `PALLIUM_WORKFLOW_AGENT_STUB` in tests to return deterministic worker
output without launching Codex.

Workflow scripts run as async JavaScript, matching Claude's saved workflow
shape: top-level `await` is supported, `pipeline()` fans one worker per item in
parallel for each stage, and completed agents are reused when the same run id is
relaunched. Runs default to 16 concurrent agents and 1,000 total agents.
Use `await check("test command")` for objective verification loops. It spawns a
dedicated test agent, runs the command as ground truth, and returns structured
JSON with `ok`, `summary`, `output_tail`, and `failures`, so scripts can keep
fixing until checks pass or progress stalls.
Saved workflows resolve by name from the nearest `.pallium/workflows/` or
`.claude/workflows/` directory while walking up from the current working
directory, then from `~/.pallium/workflows/` or `~/.claude/workflows/`.

Session-memory indexing is incremental by default: unchanged transcript files are skipped using their last indexed timestamp, with a hash check only when the file looks newer. After a global `sessions embed` pass completes and no embedding backlog remains for the model, Pallium records a model-specific embedding cursor. Later `sessions index` runs scan from that cursor minus `--safety-buffer` instead of walking historical session memory every time. This makes scheduled automation cadence-independent: hourly runs should touch about the last hour plus buffer, six-hour runs should touch about six hours plus buffer, and on-demand runs use the same cursor path. Files modified in the last two minutes are skipped so Pallium does not chase active agent logs. Use `--force` only when you intentionally want to rebuild existing session rows after parser or redaction changes.

Session-memory data is stored outside any one repo at `~/.pallium/codex-sessions.sqlite`. If an existing legacy database is present, Pallium falls back to `~/.codex-memory/codex-sessions.sqlite` so older indexed memory keeps working. It includes redacted raw agent events, transcript/tool-call rows, FTS indexes, chunks, OpenAI embeddings, and brute-force cosine semantic search. Use `OPENAI_API_KEY` or `OPENAI_ADMIN_API_KEY` for embedding commands.

For another machine's sessions:

```bash
pallium sessions index --include /path/to/other/.codex/sessions --machine tylers-macbook
pallium sessions index --provider claude --include /path/to/other/.claude/projects --machine tylers-macbook
```

## Typical Agent Loop

```bash
pallium index
pallium explain path/to/file --json
pallium safe path/to/file --json
pallium plan path/to/file --json
pallium task start "Tighten auth flow" src/auth cmd --json
pallium changed-now --json
pallium handoff origin/main --json
pallium verify fast --json
```

`verify` records each run in the repo-local Pallium database so future `review` and `handoff` output can show recent verification history.

## Install

The primary install path is npm:

```bash
npm install -g pallium
pallium version
```

The npm package installs the matching Pallium release binary from GitHub and
keeps it in `~/.pallium/npm/<version>/`. Release assets include macOS and Linux
binaries for arm64 and x64, plus a packed npm tarball for direct install from
GitHub Releases.

If npm registry access is unavailable, install the release tarball directly:

```bash
npm install -g https://github.com/tszaks/pallium/releases/download/v0.9.3/pallium-0.9.3.tgz
```

Or install with Go:

```bash
go install github.com/tszaks/pallium@latest
```

GitHub Packages is also published as `@tszaks/pallium`. GitHub Packages
requires GitHub npm registry authentication, so this is mainly a backup channel
for GitHub-authenticated environments:

```bash
npm config set @tszaks:registry https://npm.pkg.github.com
npm install -g @tszaks/pallium
```

Or from source:

```bash
git clone https://github.com/tszaks/pallium.git
cd pallium
go test ./...
go run . --help
```

## What Each Command Does

- `doctor`: checks git, local DB, repo index, session index, embeddings backlog, and environment readiness
- `version`: prints the installed Pallium build information
- `explain`: best pre-edit briefing for a file
- `safe`: tells an agent how cautious it should be, with confidence
- `plan`: gives a lightweight edit plan plus likely test commands and verification tiers
- `changed-now`: shows the live working tree, even before the repo has been indexed
- `review`: reviews branch diff plus working-tree changes with confidence, task drift, boundary warnings, related sessions, verification history, and the riskiest files first
- `verify`: runs Pallium's inferred fast, safe, or full verification command and records the result
- `handoff`: generates a final summary before handoff with related sessions and recent verification history
- `task`: stores the current goal and planned scope so drift shows up in review and handoff
- `sessions related`: ranks prior sessions by current repo, git origin, touched files, query terms, and recency
- `sessions search --hybrid`: mixes lexical search with repo and file-aware ranking
- `workflow`: runs Codex dynamic workflows with tracked phases, clean-context workers, saved scripts, top-level await, parallel pipeline stages, cached completed agents, and automatic edit application

It also handles brand-new files better now by inferring likely related files and tests even before they have indexed history, adds lightweight Go, JS/TS, and Python dependency signals including nested `tsconfig` aliases and Python `src/` layouts, prefers real repo verification commands when they exist across `package.json`, Python project files, and common `Makefile` targets, and surfaces boundary warnings for areas like auth, config, DB, API, payments, and jobs.

## Example

```bash
pallium explain src/auth/session.ts --json
pallium safe src/auth/session.ts --json
pallium handoff origin/main --json
```

## Development

```bash
go test ./...
go run . index
go run . explain README.md
go run . changed-now
go run . verify fast
go run . handoff HEAD~1
```

## Notes

- Local data lives in `.pallium/`
- Existing `.codex-memory/` indexes are still read when no `.pallium/` index exists
- If the repo has not been indexed yet, `changed-now` still reports live working-tree files and recommends `pallium index`

## License

MIT
