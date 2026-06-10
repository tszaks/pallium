# pallium v2 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `pallium` more useful before edits by improving risk scoring, adding actionable explain output, and safely migrating existing SQLite databases.

**Architecture:** Keep the existing local-first Go CLI and SQLite shape, but enrich indexed file stats with a couple of extra signals. Build the better UX in the analysis and command layers instead of adding a new subsystem.

**Tech Stack:** Go, SQLite, git history

---

## Scope

- Add `author_count` and `last_touched_at` to indexed file stats
- Migrate older `files` tables automatically on open
- Improve `risk` output with reasons, authors, and last-touched context
- Improve `explain` output with a summary and edit checklist
- Keep the release KISS: no embeddings, daemons, or remote services

## Verification

- `go test ./...`
- `go run . explain README.md`
- `go run . risk README.md`
