package sessionmemory

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const sessionCapsuleSchemaVersion = 4

type CapsuleEvidence struct {
	LineNo  int    `json:"line_no"`
	Role    string `json:"role"`
	Kind    string `json:"kind"`
	Summary string `json:"summary"`
}

type SessionCapsule struct {
	SchemaVersion int               `json:"schema_version"`
	SessionID     string            `json:"session_id"`
	Goal          string            `json:"goal"`
	StoppedAt     string            `json:"stopped_at"`
	Completed     []string          `json:"completed"`
	Remaining     []string          `json:"remaining"`
	Blockers      []string          `json:"blockers"`
	NextAction    string            `json:"next_action"`
	Status        string            `json:"status"`
	Coverage      SessionCoverage   `json:"coverage"`
	Evidence      []CapsuleEvidence `json:"evidence"`
	GeneratedAt   string            `json:"generated_at"`
}

func buildSessionCapsule(parsed ParsedSession) SessionCapsule {
	coverage := parsed.Coverage
	if coverage.Mode == "" {
		coverage.Mode = "full"
		coverage.MessagesSeen = len(parsed.Messages)
		coverage.MessagesStored = len(parsed.Messages)
	}
	last := parsed.Session.LastAgentMessage
	capsule := SessionCapsule{
		SchemaVersion: sessionCapsuleSchemaVersion,
		SessionID:     parsed.Session.ID,
		Goal:          short(redact(parsed.Session.FirstUserMessage), 1200),
		StoppedAt:     short(redact(last), 1600),
		Completed:     extractCapsuleSection(last, "completed", "done", "implemented", "what changed", "finished"),
		Remaining:     extractCapsuleSection(last, "remaining", "what remains", "still needed", "next steps"),
		Blockers:      extractCapsuleSection(last, "blocker", "blockers", "blocked by"),
		NextAction:    firstCapsuleSectionItem(last, "next action", "next step", "recommended next action"),
		Status:        parsed.Session.Status,
		Coverage:      coverage,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if capsule.Status == "aborted" {
		for _, item := range parsed.Session.Errors {
			appendUniqueCapsuleItem(&capsule.Blockers, short(redact(item), 500), 8)
		}
	}
	if capsule.NextAction == "" && len(capsule.Remaining) > 0 {
		capsule.NextAction = capsule.Remaining[0]
	}
	if capsule.Status == "aborted" && len(capsule.Blockers) == 0 {
		capsule.Blockers = []string{"The source transcript ended with an aborted turn."}
	}
	capsule.Evidence = capsuleEvidence(parsed.Messages)
	return capsule
}

func capsuleEvidence(messages []Message) []CapsuleEvidence {
	var firstUser *Message
	var lastAssistant *Message
	for i := range messages {
		message := &messages[i]
		if message.Role == "user" && firstUser == nil {
			firstUser = message
		}
		if message.Role == "assistant" {
			lastAssistant = message
		}
	}
	var evidence []CapsuleEvidence
	if firstUser != nil {
		evidence = append(evidence, CapsuleEvidence{LineNo: firstUser.LineNo, Role: firstUser.Role, Kind: firstUser.Kind, Summary: short(redact(firstUser.Text), 400)})
	}
	if lastAssistant != nil && (firstUser == nil || lastAssistant.LineNo != firstUser.LineNo) {
		evidence = append(evidence, CapsuleEvidence{LineNo: lastAssistant.LineNo, Role: lastAssistant.Role, Kind: lastAssistant.Kind, Summary: short(redact(lastAssistant.Text), 600)})
	}
	return evidence
}

func extractCapsuleSection(text string, labels ...string) []string {
	labelSet := map[string]bool{}
	for _, label := range labels {
		labelSet[label] = true
	}
	var items []string
	active := false
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		heading, inline, isHeading := capsuleHeading(line)
		if isHeading {
			active = labelSet[heading]
			if active && inline != "" {
				appendUniqueCapsuleItem(&items, inline, 8)
			}
			continue
		}
		if active {
			appendUniqueCapsuleItem(&items, strings.TrimSpace(strings.TrimLeft(line, "-*0123456789. ")), 8)
		}
	}
	return items
}

func firstCapsuleSectionItem(text string, labels ...string) string {
	items := extractCapsuleSection(text, labels...)
	if len(items) == 0 {
		return ""
	}
	return items[0]
}

func capsuleHeading(line string) (heading, inline string, ok bool) {
	clean := strings.Trim(strings.TrimSpace(strings.TrimLeft(line, "#* ")), "*_ ")
	if index := strings.Index(clean, ":"); index >= 0 {
		heading = strings.ToLower(strings.Trim(strings.TrimSpace(clean[:index]), "*_ "))
		inline = strings.Trim(strings.TrimSpace(clean[index+1:]), "*_ ")
		return heading, inline, true
	}
	if strings.HasPrefix(line, "#") || strings.HasSuffix(clean, ":") {
		return strings.ToLower(strings.TrimSuffix(clean, ":")), "", true
	}
	return "", "", false
}

func appendUniqueCapsuleItem(items *[]string, value string, limit int) {
	value = short(redact(value), 500)
	if value == "" || len(*items) >= limit {
		return
	}
	for _, existing := range *items {
		if strings.EqualFold(existing, value) {
			return
		}
	}
	*items = append(*items, value)
}

func capsuleSearchText(capsule SessionCapsule) string {
	return strings.Join([]string{
		capsule.Goal,
		capsule.StoppedAt,
		strings.Join(capsule.Completed, "\n"),
		strings.Join(capsule.Remaining, "\n"),
		strings.Join(capsule.Blockers, "\n"),
		capsule.NextAction,
	}, "\n")
}

func decodeSessionCapsule(raw string) (SessionCapsule, error) {
	var capsule SessionCapsule
	if err := json.Unmarshal([]byte(raw), &capsule); err != nil {
		return capsule, fmt.Errorf("decode session capsule: %w", err)
	}
	return capsule, nil
}

func ReadCapsule(dbPath, prefix string) (SessionCapsule, error) {
	store, err := Open(dbPath)
	if err != nil {
		return SessionCapsule{}, err
	}
	defer store.Close()
	id, err := store.resolveID(prefix)
	if err != nil {
		return SessionCapsule{}, err
	}
	var raw string
	if err := store.db.QueryRow(`SELECT capsule_json FROM codex_session_capsules WHERE session_id=?`, id).Scan(&raw); err != nil {
		return SessionCapsule{}, err
	}
	return decodeSessionCapsule(raw)
}
