package sessionmemory

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildContinuityChunksIsBoundedAndSessionFocused(t *testing.T) {
	parsed := testParsedSession("bounded-session", "final verified handoff")
	parsed.Session.Title = "Bounded continuity migration"
	for i := 0; i < 100; i++ {
		parsed.Session.FilesTouched = append(parsed.Session.FilesTouched, fmt.Sprintf("src/very/long/path/file-%03d.go", i))
		parsed.Session.Commands = append(parsed.Session.Commands, strings.Repeat(fmt.Sprintf("command-%03d ", i), 40))
		parsed.Messages = append(parsed.Messages,
			Message{LineNo: 10 + i*2, Role: "user", Text: fmt.Sprintf("user evidence %03d %s", i, strings.Repeat("detail ", 50))},
			Message{LineNo: 11 + i*2, Role: "assistant", Text: fmt.Sprintf("assistant evidence %03d %s", i, strings.Repeat("outcome ", 50))},
		)
	}
	capsule := buildSessionCapsule(parsed)
	chunks := buildContinuityChunks(parsed, capsule)
	if len(chunks) != 1 {
		t.Fatalf("chunks=%d, want exactly one continuity vector per session", len(chunks))
	}
	chunk := chunks[0]
	if chunk.Kind != "continuity" || len(chunk.Text) > 6000 || chunk.TokenEstimate > 1500 {
		t.Fatalf("continuity chunk is not bounded: kind=%s chars=%d tokens=%d", chunk.Kind, len(chunk.Text), chunk.TokenEstimate)
	}
	for _, required := range []string{"Bounded continuity migration", "Goal: test request", "Stopped at: final verified handoff", "Conversation evidence:"} {
		if !strings.Contains(chunk.Text, required) {
			t.Fatalf("continuity chunk missing %q: %s", required, chunk.Text)
		}
	}
}
