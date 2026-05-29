# codex-memory

`codex-memory` is a local-first CLI for AI-powered coding workflows.

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
- what changed in the working tree right now

## Why It Matters

LLMs are good at writing code and bad at remembering repository context.

That leads to common mistakes:

- editing a risky file in isolation
- missing related files
- skipping the most useful tests
- handing work off without a clean summary

`codex-memory` exists to lower those surprises.

## Core Commands

```bash
codex-memory index
codex-memory explain <path>
codex-memory safe <path>
codex-memory plan <path>
codex-memory changed-now
codex-memory review [base-ref]
codex-memory handoff [base-ref]
codex-memory task start "Tighten auth flow" src/auth cmd
codex-memory task show
```

Use `--json` with any command for agent-friendly output.

## Codex Session Memory

`codex-memory` can also index Codex CLI session transcripts from `~/.codex/sessions/**/*.jsonl` plus metadata from `~/.codex/state_5.sqlite`.

```bash
codex-memory sessions live --details
codex-memory sessions index
codex-memory sessions list --limit 20
codex-memory sessions search "MCP auth" --limit 10
codex-memory sessions grep "Timed out waiting for PGLite lock" --limit 20
codex-memory sessions show <session-id> --transcript
codex-memory sessions embed
codex-memory sessions semantic "find the session where we debugged MCP startup failures"
codex-memory sessions stats
```

Session-memory data is stored outside any one repo at `~/.codex-memory/codex-sessions.sqlite`. It includes redacted raw rollout events, transcript/tool-call rows, FTS indexes, chunks, OpenAI embeddings, and brute-force cosine semantic search. Use `OPENAI_API_KEY` or `OPENAI_ADMIN_API_KEY` for embedding commands.

For another machine's sessions:

```bash
codex-memory sessions index --include /path/to/other/.codex/sessions --machine tylers-macbook
```

## Typical Agent Loop

```bash
codex-memory index
codex-memory explain path/to/file --json
codex-memory safe path/to/file --json
codex-memory plan path/to/file --json
codex-memory task start "Tighten auth flow" src/auth cmd --json
codex-memory changed-now --json
codex-memory handoff origin/main --json
```

## Install

```bash
go install github.com/tszaks/codex-memory@latest
```

Or from source:

```bash
git clone https://github.com/tszaks/codex-memory.git
cd codex-memory
go test ./...
go run . --help
```

## What Each Command Does

- `explain`: best pre-edit briefing for a file
- `safe`: tells an agent how cautious it should be, with confidence
- `plan`: gives a lightweight edit plan plus likely test commands and verification tiers
- `changed-now`: shows the live working tree
- `review`: reviews branch diff plus working-tree changes with confidence, task drift, boundary warnings, and the riskiest files first
- `handoff`: generates a final summary before handoff
- `task`: stores the current goal and planned scope so drift shows up in review and handoff

It also handles brand-new files better now by inferring likely related files and tests even before they have indexed history, adds lightweight Go, JS/TS, and Python dependency signals including nested `tsconfig` aliases and Python `src/` layouts, prefers real repo verification commands when they exist across `package.json`, Python project files, and common `Makefile` targets, and surfaces boundary warnings for areas like auth, config, DB, API, payments, and jobs.

## Example

```bash
codex-memory explain src/auth/session.ts --json
codex-memory safe src/auth/session.ts --json
codex-memory handoff origin/main --json
```

## Development

```bash
go test ./...
go run . index
go run . explain README.md
go run . changed-now
go run . handoff HEAD~1
```

## Notes

- Local data lives in `.codex-memory/`
- If the repo has not been indexed yet, analysis commands will tell you to run `codex-memory index` first

## License

MIT
