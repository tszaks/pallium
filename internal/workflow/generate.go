package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

type GenerateOptions struct {
	Task        string
	Style       string
	TestCommand string
	MaxRounds   int
}

func GenerateScript(opts GenerateOptions) (string, error) {
	task := strings.TrimSpace(opts.Task)
	if task == "" {
		return "", fmt.Errorf("workflow generate requires a task")
	}
	style := strings.TrimSpace(opts.Style)
	if style == "" || style == "auto" {
		if strings.TrimSpace(opts.TestCommand) != "" {
			style = "test-fix"
		} else {
			style = "review"
		}
	}
	if opts.MaxRounds <= 0 {
		opts.MaxRounds = 3
	}
	switch style {
	case "review":
		return generateReviewWorkflow(opts), nil
	case "test-fix", "fix-until-green":
		return generateTestFixWorkflow(opts), nil
	case "research":
		return generateResearchWorkflow(opts), nil
	default:
		return "", fmt.Errorf("unknown workflow style %q", style)
	}
}

func generateReviewWorkflow(opts GenerateOptions) string {
	task := jsString(opts.Task)
	return `export const meta = {
  name: "pallium-review",
  description: "Repo-grounded parallel review workflow",
  phases: ["scope", "inspect", "synthesize"]
};

const task = ` + task + `;

phase("scope");
const preflight = await pallium.preflight(task);

phase("inspect");
const angles = ["correctness", "regression-risk", "verification-plan"];
const reviews = await pipeline(angles, angle =>
  agent("Review this task from the " + angle + " angle. Task: " + task + "\nPreflight context: " + JSON.stringify(preflight) + "\nReturn JSON with keys angle, findings, risks, next_steps.", {
    label: angle,
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        angle: { type: "string" },
        findings: { type: "array", items: { type: "string" } },
        risks: { type: "array", items: { type: "string" } },
        next_steps: { type: "array", items: { type: "string" } }
      },
      required: ["angle", "findings", "risks", "next_steps"]
    }
  })
);

phase("synthesize");
const drift = await pallium.review();
const summary = await agent("Synthesize the parallel reviews into a concise action plan. Task: " + task + "\nReviews: " + JSON.stringify(reviews) + "\nDrift report: " + JSON.stringify(drift), {
  label: "synthesizer",
  mode: "read-only",
  schema: {
    type: "object",
    properties: {
      decision: { type: "string" },
      top_findings: { type: "array", items: { type: "string" } },
      next_prompt: { type: "string" }
    },
    required: ["decision", "top_findings", "next_prompt"]
  }
});

return { preflight, reviews, drift, summary };
`
}

func generateResearchWorkflow(opts GenerateOptions) string {
	task := jsString(opts.Task)
	return `export const meta = {
  name: "pallium-research",
  description: "Cross-checked research workflow",
  phases: ["research", "verify", "synthesize"]
};

const task = ` + task + `;

phase("research");
const angles = ["source-discovery", "implementation-patterns", "risks-and-gaps"];
const reports = await pipeline(angles, angle =>
  agent("Research this task from the " + angle + " angle. Use available repo context and web/search tools only when useful. Task: " + task + "\nReturn JSON with keys angle, claims, sources, open_questions.", {
    label: angle,
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        angle: { type: "string" },
        claims: { type: "array", items: { type: "string" } },
        sources: { type: "array", items: { type: "string" } },
        open_questions: { type: "array", items: { type: "string" } }
      },
      required: ["angle", "claims", "sources", "open_questions"]
    }
  })
);

phase("verify");
const verification = await agent("Cross-check these reports. Flag unsupported claims, contradictions, and missing primary evidence. Reports: " + JSON.stringify(reports), {
  label: "cross-checker",
  mode: "read-only",
  schema: {
    type: "object",
    properties: {
      verdict: { type: "string" },
      corrections: { type: "array", items: { type: "string" } },
      strongest_sources: { type: "array", items: { type: "string" } }
    },
    required: ["verdict", "corrections", "strongest_sources"]
  }
});

phase("synthesize");
return { reports, verification };
`
}

func generateTestFixWorkflow(opts GenerateOptions) string {
	task := jsString(opts.Task)
	testCommand := jsString(firstNonEmpty(opts.TestCommand, "pallium verify fast"))
	maxRounds := opts.MaxRounds
	return `export const meta = {
  name: "pallium-fix-until-green",
  description: "Claude-style implement, verify, fix loop",
  phases: ["scope", "baseline", "fix-loop", "finalize"]
};

const task = ` + task + `;
const testCommand = ` + testCommand + `;
const maxRounds = ` + fmt.Sprintf("%d", maxRounds) + `;

phase("scope");
await pallium.task.start(task);
const baselineContext = await pallium.preflight(task);

phase("baseline");
let checkResult = await check(testCommand, { label: "baseline-check" });
let rounds = [];

phase("fix-loop");
for (let round = 1; round <= maxRounds && !checkResult.ok; round++) {
  const previous = JSON.stringify(checkResult);
  const fix = await agent("Fix the failing verification for this task.\nTask: " + task + "\nCommand: " + testCommand + "\nFailure JSON: " + previous + "\nMake the smallest correct code change. Do not hide, skip, or weaken tests.", {
    label: "fix-round-" + round,
    mode: "edit",
    isolation: "worktree",
    schema: {
      type: "object",
      properties: {
        summary: { type: "string" },
        files_changed: { type: "array", items: { type: "string" } },
        confidence: { type: "string" }
      },
      required: ["summary", "files_changed", "confidence"]
    }
  });
  const review = await pallium.review();
  const nextCheck = await check(testCommand, { label: "check-round-" + round });
  rounds.push({ round, fix, review, check: nextCheck });
  if (JSON.stringify(nextCheck.failures) === JSON.stringify(checkResult.failures) && !nextCheck.ok) {
    break;
  }
  checkResult = nextCheck;
}

phase("finalize");
const finalReview = await pallium.review();
const handoff = await pallium.handoff("HEAD~1");
return { task, baselineContext, final: checkResult, rounds, finalReview, handoff };
`
}

func jsString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
