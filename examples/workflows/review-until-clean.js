// review-until-clean: a bounded loop cycle for driving review findings to
// zero. Tyler's highest-value recurring cycle — the reference `pallium
// loop` template.
//
// Real usage against an actual GitHub PR: replace the "observe" step below
// with `gh api repos/OWNER/REPO/pulls/{pr}/comments` filtered to unresolved
// threads (a networked agent(), or a plain shell step — see
// providers/README.md's network contract), and the "fix" step's agent with
// one that replies + resolves each addressed thread (see the review-loop
// skill for the full gh polling/reply/resolve mechanics this loop's single
// cycle is meant to drive, repeatedly, via `pallium loop tick`). This
// reference implementation stands in with a repo-local marker so it runs
// standalone, with no live PR required, for testing the loop mechanism
// itself.
//
// Start:  pallium loop start review-until-clean --script examples/workflows/review-until-clean.js --cwd <repo>
// Advance one cycle at a time (no daemon — cron, a trigger, or an agent
// calls this repeatedly): pallium loop tick review-until-clean
export const meta = {
  name: "review-until-clean",
  description: "Bounded loop that fixes review findings each cycle until clean, blocked, or stagnated",
  kind: "loop",
  loop: {
    stagnationThreshold: 3,
    cycleBudgetUsd: 0,
    lifetimeBudgetUsd: 0,
    staleAfterMinutes: 15,
  },
};

const marker = args?.marker ?? "REVIEW-FINDING:";

phase("observe");
const observed = await agent(
  `Search this repo for lines containing "${marker}" (a stand-in for open PR review findings). ` +
    `Return JSON: { findings: [{file, line, text}] }. If none are found, return { findings: [] }.`,
  {
    label: "observe",
    mode: "read-only",
    schema: {
      type: "object",
      properties: { findings: { type: "array" } },
      required: ["findings"],
    },
  }
);

const findings = observed.findings ?? [];
if (findings.length === 0) {
  // The empty string signature is deliberate: AdvanceLoopStagnation treats
  // an empty signature as "this script isn't opting into stagnation
  // detection right now" rather than a false repeat — appropriate here
  // since "clean" isn't a stuck state, it's the terminal one.
  return { state: "success", signature: "", summary: "no outstanding findings" };
}

phase("fix");
// One fix agent per cycle keeps a single cycle's blast radius small and its
// own budget predictable — this loop converges over MANY cycles, not in one
// giant edit that would defeat the point of ticking at all.
const target = findings[0];
await agent(
  `Resolve this review finding and remove its "${marker}" marker once fixed: ${JSON.stringify(target)}`,
  { label: "fix", mode: "edit" }
);

// The signature is the script's own stagnation-detection CONTRACT (see
// AdvanceLoopStagnation's doc comment: this is NOT an implicit hash of the
// whole return value) — the sorted list of still-outstanding finding
// locations. If the SAME set survives cycle after cycle despite a "fix"
// agent running every time, this loop is genuinely stuck, not merely slow.
const signature = findings
  .map((f) => f.file + ":" + f.line)
  .sort()
  .join(",");
return { state: "no_op", signature, summary: `fixed 1 of ${findings.length} findings this cycle` };
