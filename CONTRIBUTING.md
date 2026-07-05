# Contributing

Pallium is a local-first CLI for codebase memory, verification, sessions, and dynamic workflows for AI coding agents.

## Local Setup

Requirements:

- Go version from `go.mod`
- Node 24 or newer for the npm wrapper tests
- Git

Run the standard checks:

```bash
go vet ./...
go test ./... -count=1
node npm/scripts/test.js
```

Workflow-runtime changes should also run:

```bash
go test ./internal/workflow ./cmd ./pkg/workflowclient -count=1
bash scripts/workflow-verify.sh
```

## Pull Requests

Keep PRs focused. A good PR contains:

- A clear problem statement
- Tests for changed behavior
- Docs updates when CLI flags, workflow primitives, JSON output, or install behavior changes
- No unrelated formatting churn

For workflow runtime changes, include tests for failure paths, not only happy paths.

## Design Rules

- Prefer local-first behavior.
- Keep workflow runs inspectable and resumable.
- Treat verification commands, test output, and stored run state as stronger evidence than model self-assessment.
- Do not add provider-specific behavior unless the provider is selected explicitly.
- Do not add human approval gates to workflow scripts. Pallium uses autonomous agent gates by default.

## Security

Do not file public issues containing exploit details or secrets. Use the repository security policy instead.
