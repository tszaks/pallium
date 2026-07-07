// Adversarial review: find issues per file, then a skeptic tries to refute
// each finding. Only claims that survive refutation reach the synthesis.
// Run: pallium workflow run --script examples/workflows/adversarial-review.js "review the auth layer" --args '{"task":"review the auth layer"}'
// Note: the positional task string only becomes run metadata (visible via
// `pallium workflow inspect`) — it is NOT injected into the script's `args`
// global. Pass the task through --args as well so `args?.task` resolves.
export const meta = {
  name: "adversarial-review",
  description: "Per-file find, per-finding skeptic verify, synthesized report",
  phases: ["scope", "find", "verify", "synthesize"],
};

const task = args?.task ?? "Review this change set for concrete defects";

phase("scope");
// Repo memory narrows the sweep before any worker spawns.
const preflight = await pallium.preflight(task);
const files = (preflight.files_to_inspect ?? []).slice(0, 20);
log("inspecting", files.length, "files");

// pipeline() streams: file A can be in verify while file B is still in find.
const results = await pipeline(
  files,
  (file) =>
    agent(`Find up to 3 concrete issues in ${file}. Task: ${task}. Quote evidence; no style nits.`, {
      label: "find-" + file,
      phase: "find",
      mode: "read-only",
      schema: {
        type: "object",
        properties: {
          file: { type: "string" },
          findings: { type: "array", items: { type: "string" } },
        },
        required: ["file", "findings"],
      },
    }),
  (found, file) => {
    // Skip the skeptic when there is nothing to refute.
    if (!found || found.findings.length === 0) return { file, confirmed: [] };
    return agent(
      `Try to refute each claimed issue. Default to refuted if uncertain or behavior-by-design.\n${JSON.stringify(found)}`,
      {
        label: "skeptic-" + file,
        phase: "verify",
        mode: "read-only",
        schema: {
          type: "object",
          properties: {
            file: { type: "string" },
            confirmed: { type: "array", items: { type: "string" } },
          },
          required: ["file", "confirmed"],
        },
      },
    );
  },
);

phase("synthesize");
// Failed workers become null — filter before trusting the set.
const confirmed = results.filter(Boolean).flatMap((r) => (r.confirmed ?? []).map((c) => ({ file: r.file, c })));
log(confirmed.length, "findings survived refutation");
return await agent(
  `Write a prioritized report from these verified findings. Plain prose.\n${JSON.stringify(confirmed)}`,
  { label: "synthesize", mode: "read-only" },
);
