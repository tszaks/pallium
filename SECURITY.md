# Security Policy

## Supported Versions

Security fixes target the latest released version of Pallium. If a fix affects package installation, release artifacts, or workflow execution safety, the fix will be shipped as a patch release.

## Reporting a Vulnerability

Please report security issues through GitHub's private vulnerability reporting for this repository.

If private reporting is unavailable, open a public issue with only a short description of the affected area. Do not include exploit details, secrets, tokens, private logs, or proof-of-concept payloads in a public issue.

Useful reports include:

- Pallium version and install method
- Operating system
- Affected command or API endpoint
- Whether the issue requires a malicious workflow script, malicious repo contents, or only normal CLI usage
- Minimal reproduction steps that do not expose private data

## Scope

High-priority areas:

- Workflow runtime escape, arbitrary command execution, or unsafe patch application
- HTTP API, MCP, and local control-plane authorization
- npm wrapper and release asset integrity
- Secret handling and redaction
- SQLite persistence that can corrupt or leak local session data

## Response

Valid security reports will be triaged before public disclosure. Fixes may include code changes, documentation updates, release revocation, or package metadata changes depending on severity.
