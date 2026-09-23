# Knowledge upgrade release contract

Target: the next unused minor npm version after 0.9.23 (currently 0.10.0),
with the exact tested merged commit tagged on GitHub. Implementation and
release/deployment are authorized by the project owner.

## Required behavior

- Workspace-scoped indexes, current/stale/legacy read labels, source-bound
  publication, preserved historical documents and migration backup.
- Exact module/receiver citations, non-colliding directory IDs, parser quality
  disclosure and Go/TypeScript/Swift regression fixtures.
- Tracked project documentation/configuration plus explicit untracked file
  opt-ins. Authored linked decisions survive reindexing and expose conflicts.
- Bounded version-2 search/context, section reads, full-response compatibility,
  CLI/MCP agreement and health reporting.
- Durable maintenance through the existing global kernel database. Repositories
  opt in; 40 model attempts per rolling day globally, two concurrent calls,
  three-minute deadlines, one transient retry, auth/quota pause and recovery.
- Loopback browser with evidence, source views and maintenance controls. Host,
  origin, mutation-token and source-path restrictions; safe text rendering.
- Controlled 30-question retrieval corpus, 2,000-file performance fixture and
  an explicitly opt-in real-provider paired evaluation with honest limits.

## Release sequence

1. Final candidate: Go tests, vet, race tests (`-short` excludes disk timing),
   vulnerability scan, npm wrapper tests and workflow acceptance.
2. Push reviewed implementation and release metadata through PRs; required CI
   must pass on their exact heads. Recheck merged main before tagging.
3. Create a draft GitHub release. Build all four macOS/Linux archives from the
   clean tagged commit, verify version/revision metadata and SHA-256 manifest,
   and upload all assets before making the release public.
4. Published-release automation verifies prepared artifacts without replacing
   them. Dispatch the existing trusted npm workflow for the same tag; it requires
   public release assets and verifies their checksums before publishing.
5. Verify npm version/dist-tag/provenance and a fresh isolated npm installation
   that uses release binaries rather than hiding asset failure through Go fallback.
6. Update the local install, verify binary version/revision/checksum, install
   the background service and enable only the Pallium repository.
7. Smoke-test CLI/MCP/browser, incremental refresh, restart recovery and quota
   visibility with the released binary. Initial auditing remains bounded by the
   same global cap; any remaining queue must be visible.

## Rollback

Stop knowledge maintenance before rollback. Keep the previous installed binary
and the migration backup. If an older release cannot interpret the new schema,
restore the backup while no Pallium process has the repo database open; preserve
current database/WAL files separately first. The original main-worktree repo row
is retained; linked worktrees must be indexed separately. Never repoint a published
tag or replace published archives. Correct a public defect in a new patch release.

## Evidence location during execution

The local progress page and `/tmp/pallium-upgrade-progress.md` hold current gate
results. A PR or a passing test alone is not completion: the GitHub release,
npm installation and local running service each require separate verification.
