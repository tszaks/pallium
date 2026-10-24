# Pallium

A local-first control plane for coding agents, written in Go with an npm wrapper. One binary, one SQLite database, seven services on a shared kernel.

This file is for AI agents that work **on** Pallium's code. If you are an agent that **uses** Pallium, read [PALLIUM.md](PALLIUM.md) instead. Workflow-script authoring is in [PALLIUM_WORKFLOW.md](PALLIUM_WORKFLOW.md). The kernel and services ruling is in [ARCHITECTURE.md](ARCHITECTURE.md): read it before adding a capability.

## Build and test
- Go version comes from `go.mod`. Node 24 or newer runs the npm wrapper tests.
- Vet: `go vet ./...`
- Test: `go test ./... -count=1`. Run the whole suite before you say something works.
- npm wrapper: `node npm/scripts/test.js`
- Workflow runtime changes also run: `go test -race ./internal/workflow -count=1`, `go test ./internal/workflow ./cmd ./pkg/workflowclient -count=1` and `bash scripts/workflow-verify.sh`.
- Run from source: `go run . --help`
- CI (`.github/workflows/ci.yml`, required check `test`) runs the same steps plus `govulncheck`.

## Code map
- `main.go`, `cmd/`: the CLI. One file per command group (`route.go`, `workflow.go`, `loop.go`, `team.go`, `sessions.go`, `knowledge*.go`, `verify.go`, `update.go`, and so on) with tests beside them.
- `internal/`: the services and the kernel. `db` (SQLite store), `workflow` (runtime, worktrees, patches, gates), `router` and `routing` (task and model routing), `index`, `codeindex`, `analysis`, `gitlog` (repo intelligence), `codexsessions` and `sessionmemory` (session memory), `knowledge`, `verification`, `console`, `agentdocs`, `output`.
- `pkg/workflowclient`: the public Go client for the workflow API.
- `npm/`: the `pallium` npm wrapper. It downloads the release binary on install.
- `providers/`: one-line wrapper scripts that plug other agent CLIs in as workers.
- `eval/`: routing, adoption and knowledge evals. `examples/workflows/`: sample workflow scripts.
- `scripts/`: release asset and workflow verification scripts.

## Conventions
- Match the surrounding code: naming, comment density, idiom.
- Services compose through each other's public front doors (`workflow run`, the team API). Never reach into another service's tables or internals.
- Prefer local-first behavior. Keep workflow runs inspectable and resumable.
- Verification output and stored run state outrank a model's self-assessment.
- Do not hardcode a model or add provider-specific behavior unless the provider is selected explicitly.
- Do not add human approval gates to workflow scripts.
- Most commands take `--json`. Treat that output as a stable contract: do not change its shape without updating the docs and tests.
- Workflow runtime changes need failure-path tests, not only happy paths.
- Update the docs (`README.md`, `PALLIUM.md`, `PALLIUM_WORKFLOW.md`) and add a note under `docs/releases/` for a release when you change CLI flags, workflow primitives, JSON output or install behavior.

## Do not
- Do not publish to npm, push tags or cut releases unless asked.
- Do not commit `.pallium/`, SQLite files or run artifacts.
- Do not put secrets, tokens or private logs in issues, tests or fixtures.
