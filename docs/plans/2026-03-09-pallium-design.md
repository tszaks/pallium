# Pallium Design

## Scope
Build a local-first CLI that indexes repository history and optional Codex session context into a lightweight memory database so developers and agents can ask better “what should I know before touching this?” questions.

## Product Goal
Turn missing codebase context into a queryable local tool. `pallium` should help answer:
- what files are risky
- what files usually change together
- what recently changed in an area
- what past commits or sessions explain why something exists

## User Experience
The tool should feel fast, local, and inspectable.

Primary commands:
- `pallium index`
- `pallium explain <path>`
- `pallium risk <path>`
- `pallium neighbors <path>`
- `pallium decisions <query>`

## Architecture
- Runtime: Go CLI
- Storage: local SQLite database per repo
- Data sources:
  - git history
  - filesystem snapshot
  - optional Codex local state/session exports later
- Layout:
  - `main.go` CLI entry
  - `cmd/` command handlers
  - `internal/gitlog/` git ingestion
  - `internal/index/` indexing pipeline
  - `internal/db/` SQLite schema and queries
  - `internal/analysis/` risk, co-change, and explanation logic
  - `internal/output/` table/json/text formatters

## v1 Data Model
### Repositories
- repo root
- current branch
- last indexed commit

### Files
- path
- extension
- existence status
- churn score
- recent touch count

### Commits
- sha
- author
- timestamp
- subject
- body summary

### File Commits
- file path ↔ commit sha join table

### Co-Change Edges
- source file
- related file
- co-change count
- recency weight

### Decision Notes
Derived summaries from commit messages and, later, session summaries.

## Core Behaviors
### `index`
- scan git history incrementally
- update file stats
- recompute co-change edges
- store summaries in SQLite

### `explain <path>`
- show recent commits touching the path
- show top related files
- show churn/risk hints
- show any matching decision notes

### `risk <path>`
- calculate a simple score from:
  - churn
  - recent change frequency
  - number of frequent neighbors

### `neighbors <path>`
- show files that commonly change with the given path

### `decisions <query>`
- search commit subjects/bodies and stored summaries for likely rationale

## KISS Boundaries
- no embeddings in v1
- no remote backend
- no daemon
- no IDE extension
- no PR API integration
- no complex NLP summarization pipeline

## Optional v1.5
- ingest Codex session memory from local state exports
- attach session-derived notes to files and repos

## Public Packaging
- publish as a small Go CLI
- simple install path similar to `codex-sessions`
- JSON output support for LLM/tool use
