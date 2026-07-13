// Adoption eval, run ON Pallium's own workflow engine (dogfood).
// The guiding agent here is Claude, and Pallium is meant to adopt the guiding
// agent's model, so BOTH the blind worker and the judge run on the claude
// provider. Each task: a fresh blind worker gets the AGENTS.md trigger block +
// the task and reports its decision; then a fresh judge scores it against the
// rubric. (The workers can invoke read-only `pallium` to orient — the claude
// wrapper's read-only allowlist includes Bash(pallium:*).)
//
// Run (from the repo checkout you want evaluated):
//   export PALLIUM_WORKFLOW_PROVIDER_CLAUDE_COMMAND=$HOME/bin/pallium-claude-provider.sh
//   pallium workflow run --background --script eval/adoption/eval.workflow.js "adoption baseline" \
//     --args "{\"repo\":\"$PWD\"}" --json
// The repo path is taken from --args {"repo":"..."}; if omitted it defaults to
// "." (the workflow's working directory), so run it from the checkout under test.
export const meta = {
  name: "adoption-eval-baseline",
  description: "Blind fresh worker per task (AGENTS.md trigger in context), claude-provider judge on the 4-axis rubric",
  phases: ["run", "judge"],
};

// Derive the repo under evaluation from --args {"repo":"..."}; default to "."
// (the workflow's working directory) so the dogfood eval is reproducible on any
// checkout. No Date.now/Math.random/argless-new-Date — the value is static.
const REPO = args?.repo ?? ".";

// Kept byte-identical to cmd/agents.go's agentsBlock — this eval measures
// discovery/recall against the REAL trigger a fresh session actually sees,
// not a stale copy. Found live while wiring the team/loop scenarios below:
// the real block only ever mentioned workflows, never team or loop, so a
// fresh agent had no way to discover 2 of Pallium's 6 services from this
// text alone — fixed in cmd/agents.go, mirrored here.
const BLOCK = `<!-- pallium:agents:begin -->
## Pallium

This machine has Pallium: a local control plane for coding agents (workflows, loops, agent teams, repo memory, verification, session state — kept outside your context window).

Reach for it when a task is multi-step, needs tests objectively green, wants parallel workers, must survive the session, needs isolated reviewable edits, needs peers coordinating and messaging each other, or needs to keep retrying across separate future invocations. Skip it for one-shot edits.

- Scope first: \`pallium workflow preflight "<task>"\` (files to inspect, risk, test commands)
- Orchestrate: write an async-JS workflow, then \`pallium workflow validate f.js && pallium workflow run --script f.js "<task>" --json\`
- Primitives: \`agent()\` (schema-validated workers), \`pipeline()\` (streaming stages), \`parallel()\` (barrier), \`verify.untilGreen()\`, \`gate()\` — discover all with \`pallium workflow tools list --json\`
- Peers messaging/disagreeing with each other: \`pallium team start|spawn|send|run\` (or \`team start --template parallel-review|adversarial-debate\` for a known-good shape)
- Bounded cycles that survive across separate invocations: \`pallium loop start|tick|status\` — no daemon, an external scheduler or you decide when to tick again
- Resume and inspect: \`pallium workflow resume|inspect|report <run-id>\`

Full agent guide: \`pallium agents guide\`
<!-- pallium:agents:end -->`;

// M3 addition: team-adversarial-review and recurring-test-fix-loop probe the
// two capabilities this eval never covered before (team, loop). Deliberately
// NOT included here: the two decay_probe tasks in tasks.jsonl
// (multi-phase-audit-decay, sustained-migration-decay). Mid-session decay is
// a BEHAVIORAL fact — did real usage survive into the back half of an
// actually-executed multi-phase task — and this harness only ever asks a
// worker to self-report its INTENDED approach for one task description; a
// worker could self-report "I'd use it throughout" without ever being
// tested against actually doing so. Decay needs run.sh's real-transcript
// path (which walks actual tool calls in order); self-report cannot
// validly measure it, so it isn't asked to here.
const TASKS = [
  { id: "audit-concurrency", task: "Audit this Go codebase for concurrency bugs (races, deadlocks, unguarded shared state) and give me a list of confirmed issues with evidence.", should_use: true, ideal: "workflow" },
  { id: "fix-failing-test", task: "A test in ./internal/workflow is failing. Fix the root cause and make sure the entire test suite passes before you call it done.", should_use: true, ideal: "verify" },
  { id: "consistent-refactor", task: "Rename the Store method WithTx to RunInTx across the whole repo and update every call site and test consistently.", should_use: true, ideal: "workflow" },
  { id: "multi-angle-review", task: "Review the most recent commit on this branch from multiple angles (correctness, error handling, performance) and give me only the findings you're confident are real.", should_use: true, ideal: "workflow" },
  { id: "investigate-slow-index", task: "Figure out why indexing a large repo might be slow and propose concrete fixes backed by evidence from the code.", should_use: true, ideal: "repo-memory" },
  { id: "explain-function", task: "What does the trimText function in cmd/sessions.go do?", should_use: false, ideal: "none" },
  { id: "fix-typo", task: "There's a typo in the README: fix 'wich' to 'which'.", should_use: false, ideal: "none" },
  { id: "add-comment", task: "Add a one-line doc comment above the CurrentBranch function in internal/gitlog/gitlog.go describing what it returns.", should_use: false, ideal: "none" },
  { id: "team-adversarial-review", task: "I'm not sure the last commit on this branch is actually safe to ship. Get me a real second opinion: have someone argue it's fine and someone else actively try to find the case that breaks it, let them push back on each other if they disagree, then give me one final call.", should_use: true, ideal: "team" },
  { id: "recurring-test-fix-loop", task: "Set up something that keeps trying to get `go test ./...` green over time — I'll kick it off myself every so often (think a scheduled job), and each attempt needs to remember how far the last one got instead of starting over, and know when to give up and tell me it's stuck rather than spinning forever.", should_use: true, ideal: "loop" },
];

const DECISION_SCHEMA = {
  type: "object",
  properties: {
    reached_for_pallium: { type: "boolean" },
    commands_run: { type: "array", items: { type: "string" } },
    chosen_entry: { type: "string" },
    capability: { type: "string", enum: ["workflow", "verify", "repo-memory", "team", "loop", "none"] },
    approach: { type: "string" },
  },
  required: ["reached_for_pallium", "commands_run", "chosen_entry", "capability", "approach"],
};

const SCORE_SCHEMA = {
  type: "object",
  properties: {
    discovery: { type: "number" },
    appropriateness: { type: "number" },
    capability_match: { type: "number" },
    correct_usage: { type: "number" },
    total: { type: "number" },
    note: { type: "string" },
  },
  required: ["discovery", "appropriateness", "capability_match", "correct_usage", "total", "note"],
};

phase("run");
// NOTE: stages return agent() DIRECTLY (no .then chaining). Pallium's goja
// capture model records agent() as a placeholder marker during a synchronous
// capture pass, so chaining .then() onto it awaits an unresolved promise and
// the stage errors out. (Native Ultracode allows .then here; Pallium does not.)
// We recover the per-task mapping by index after the pipeline instead.
const scores = await pipeline(
  TASKS,
  (t) =>
    agent(
      `${BLOCK}\n\n---\nYou are a coding agent working in the repository at ${REPO}. Your task:\n\n"${t.task}"\n\n` +
        `Work as you naturally would. You MAY run read-only shell commands and read files to orient yourself and decide your approach (including any tools the notes above mention, if useful). ` +
        `Do NOT modify files, commit, or push — once you've decided how you'd actually tackle this, stop and report. ` +
        `Report honestly: whether you reached for Pallium, the exact commands you actually ran to orient, your chosen entry point, which capability bucket it falls in (workflow / verify / repo-memory / team / loop / none), and a 2-3 sentence approach.`,
      { label: "run-" + t.id, mode: "read-only", provider: "claude", schema: DECISION_SCHEMA },
    ),
  (prev, t) =>
    agent(
      `You are scoring one adoption-eval result against the rubric. Be strict and evidence-based; verify any claimed 'pallium' subcommand is real by checking ${REPO}.\n\n` +
        `TASK: "${t.task}"\nLABELS: should_use=${t.should_use}, ideal_capability=${t.ideal}\n\n` +
        `WORKER BEHAVIOR (self-reported): ${JSON.stringify(prev)}\n\n` +
        `Score four axes 0-25 each:\n` +
        `1. discovery: aware of / oriented to Pallium? (should_use=false: awareness-and-correctly-declining = full marks)\n` +
        `2. appropriateness: right use/skip decision? should_use=true→used=25/adhoc=0; should_use=false→skipped=25/over-engineered=0.\n` +
        `3. capability_match: picked ideal_capability? correct=25, pallium-but-suboptimal=12, wrong=0; ideal 'none' & chose none=25.\n` +
        `4. correct_usage: would its concrete first action actually work (valid subcommand/flags/script)? valid=25, right-idea-wrong-invocation=12, broken/absent=0.\n` +
        `total = sum. One-sentence note.`,
      { label: "judge-" + t.id, mode: "read-only", provider: "claude", schema: SCORE_SCHEMA },
    ),
);

phase("judge");
const rows = scores
  .map((score, i) => (score ? { id: TASKS[i].id, should_use: TASKS[i].should_use, ideal: TASKS[i].ideal, score } : null))
  .filter(Boolean);
const mean = (xs) => (xs.length ? xs.reduce((a, b) => a + b, 0) / xs.length : 0);
const overall = mean(rows.map((r) => Number(r.score.total)));
const trueSubset = mean(rows.filter((r) => r.should_use).map((r) => Number(r.score.total)));
const falseSubset = mean(rows.filter((r) => !r.should_use).map((r) => Number(r.score.total)));
log("overall=" + overall.toFixed(1) + " should_use_true=" + trueSubset.toFixed(1) + " should_use_false=" + falseSubset.toFixed(1) + " n=" + rows.length);
return { overall, trueSubset, falseSubset, n: rows.length, rows };
