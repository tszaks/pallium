package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Report struct {
	ID        string        `json:"id"`
	Task      string        `json:"task"`
	Status    string        `json:"status"`
	OwnedID   string        `json:"owned_session_id,omitempty"`
	Summary   string        `json:"summary"`
	Findings  []string      `json:"findings,omitempty"`
	Risks     []string      `json:"risks,omitempty"`
	NextSteps []string      `json:"next_steps,omitempty"`
	Patches   []string      `json:"patches,omitempty"`
	Agents    []AgentReport `json:"agents"`
	Failures  []RunFailure  `json:"failures,omitempty"`
	Error     string        `json:"error,omitempty"`
}

type AgentReport struct {
	Label            string   `json:"label"`
	Phase            string   `json:"phase,omitempty"`
	Provider         string   `json:"provider,omitempty"`
	Repo             string   `json:"repo,omitempty"`
	Mode             string   `json:"mode"`
	Status           string   `json:"status"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd,omitempty"`
	Summary          string   `json:"summary,omitempty"`
	Risks            []string `json:"risks,omitempty"`
	Error            string   `json:"error,omitempty"`
}

func BuildReport(snapshot Snapshot) Report {
	report := Report{
		ID:       snapshot.Run.ID,
		Task:     snapshot.Run.Task,
		Status:   snapshot.Run.Status,
		OwnedID:  snapshot.Run.OwnedID,
		Failures: snapshot.Run.Failures,
		Error:    snapshot.Run.Error,
		Summary:  defaultReportSummary(snapshot),
	}
	for _, agent := range snapshot.Agents {
		if agent.PatchPath != "" {
			report.Patches = appendUnique(report.Patches, agent.PatchPath)
		}
		parsed := parseJSONish(agent.Output)
		agentReport := AgentReport{
			Label:            firstNonEmpty(agent.Label, agent.ID),
			Phase:            agent.Phase,
			Provider:         agent.Provider,
			Repo:             agent.Repo,
			Mode:             agent.Mode,
			Status:           agent.Status,
			EstimatedCostUSD: agent.EstimatedCostUSD,
			Error:            agent.Error,
		}
		for _, value := range collectStrings(parsed, "summary", "decision", "verdict") {
			if agentReport.Summary == "" {
				agentReport.Summary = value
			}
		}
		agentReport.Risks = collectStrings(parsed, "risks", "risk")
		report.Findings = appendUnique(report.Findings, collectStrings(parsed, "findings", "top_findings", "observations", "claims", "corrections")...)
		report.Risks = appendUnique(report.Risks, agentReport.Risks...)
		report.NextSteps = appendUnique(report.NextSteps, collectStrings(parsed, "next_steps", "next_step", "next_prompt")...)
		if agentReport.Summary == "" && strings.TrimSpace(agent.Output) != "" {
			agentReport.Summary = trimForReport(agent.Output, 240)
		}
		report.Agents = append(report.Agents, agentReport)
	}
	if snapshot.Run.Result != "" {
		parsed := parseJSONish(snapshot.Run.Result)
		for _, value := range collectStrings(parsed, "summary", "decision", "verdict") {
			if report.Summary == "" || strings.HasPrefix(report.Summary, "Workflow ") {
				report.Summary = value
				break
			}
		}
		report.Findings = appendUnique(report.Findings, collectStrings(parsed, "findings", "top_findings", "observations", "claims", "corrections")...)
		report.Risks = appendUnique(report.Risks, collectStrings(parsed, "risks", "risk")...)
		report.NextSteps = appendUnique(report.NextSteps, collectStrings(parsed, "next_steps", "next_step", "next_prompt")...)
	}
	return report
}

func defaultReportSummary(snapshot Snapshot) string {
	return fmt.Sprintf("Workflow %s %s with %d agents across %d phases.", snapshot.Run.ID, snapshot.Run.Status, len(snapshot.Agents), len(snapshot.Phases))
}

func parseJSONish(raw string) any {
	var out any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &out) == nil {
		return out
	}
	return raw
}

func collectStrings(value any, keys ...string) []string {
	keySet := map[string]struct{}{}
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	var out []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, ok := keySet[key]; ok {
					out = append(out, scalarStrings(child)...)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return out
}

func scalarStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			out = append(out, scalarStrings(item)...)
		}
		return out
	case map[string]any:
		if msg, ok := typed["message"].(string); ok {
			return []string{msg}
		}
		if name, ok := typed["name"].(string); ok {
			return []string{name}
		}
	}
	return nil
}

func appendUnique(existing []string, values ...string) []string {
	seen := map[string]struct{}{}
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		existing = append(existing, value)
	}
	return existing
}

func trimForReport(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return strings.TrimSpace(value[:max]) + "..."
}
