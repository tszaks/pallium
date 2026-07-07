// Judge panel: independent angles analyzed in parallel, judged, synthesized.
// Use when the question is wide and one perspective would miss things.
// Run: pallium workflow run --script examples/workflows/research-panel.js "how should we shard the store?"
export const meta = {
  name: "research-panel",
  description: "Parallel perspective analysis, judged and synthesized",
  phases: ["analyze", "judge", "synthesize"],
};

const question = args?.question ?? "Assess the architecture of this repository";
const ANGLES = ["correctness and data integrity", "performance and scale", "maintainability and coupling"];

phase("analyze");
// parallel() is a barrier: use it here because the judge needs ALL reports.
const reports = await parallel(ANGLES, (angle) =>
  agent(`${question}\n\nAnalyze strictly from the ${angle} perspective. Cite files and functions you actually read.`, {
    label: "angle-" + angle.split(" ")[0],
    mode: "read-only",
  }),
);

phase("judge");
const judged = await agent(
  `Score each report 1-10 for evidence quality and insight. Flag claims that contradict each other.\n` +
    JSON.stringify(reports.filter(Boolean)),
  {
    label: "judge",
    mode: "read-only",
    schema: {
      type: "object",
      properties: {
        scores: { type: "array", items: { type: "number" } },
        contradictions: { type: "array", items: { type: "string" } },
      },
      required: ["scores", "contradictions"],
    },
  },
);

phase("synthesize");
const answer = await agent(
  `Synthesize a final answer to: ${question}\nWeight reports by these scores: ${JSON.stringify(judged.scores)}. ` +
    `Resolve or flag these contradictions: ${JSON.stringify(judged.contradictions)}.\n` +
    JSON.stringify(reports.filter(Boolean)),
  { label: "synthesize", mode: "read-only" },
);

// Record the outcome so future sessions can recall why this was decided.
await pallium.decisions.record(`Research: ${question}`, answer.slice ? answer.slice(0, 500) : String(answer), "workflow");
return answer;
