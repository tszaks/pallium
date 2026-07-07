// Package agentdocs embeds agent-facing documentation into the binary.
//
// The canonical source of truth is PALLIUM.md at the repository root; the
// copy in this directory exists only because go:embed cannot reach parent
// directories. A test in cmd/agents_test.go asserts the two stay identical.
package agentdocs

import _ "embed"

//go:embed PALLIUM.md
var Guide string
