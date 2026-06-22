# pallium

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
```

Use `--json` with any command for agent-friendly output.

## Agent Session Memory

`pallium` can also index Codex CLI transcripts from `~/.codex/sessions/**/*.jsonl` plus metadata from `~/.codex/state_5.sqlite`, and Claude Code transcripts from `~/.claude/projects/**/*.jsonl`.

```bash
pallium sessions live --details
pallium sessions index
pallium sessions index --force
pallium sessions index --provider claude
pallium sessions index --provider codex
pallium sessions list --limit 20
pallium sessions search "MCP auth" --limit 10
pallium sessions search "MCP auth" --hybrid --limit 10
pallium sessions related . --file cmd/app.go --limit 5
pallium sessions grep "Timed out waiting for PGLite lock" --limit 20
pallium sessions show <session-id> --transcript
pallium sessions embed
pallium sessions semantic "find the session where we debugged MCP startup failures"
pallium sessions stats
```

Session-memory indexing is incremental by default: unchanged transcript files are skipped using their last indexed timestamp, with a hash check only when the file looks newer. Files modified in the last two minutes are skipped so Pallium does not chase active agent logs. Use `--force` only when you intentionally want to rebuild existing session rows after parser or redaction changes.

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

```bash
go install github.com/tszaks/pallium@latest
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
