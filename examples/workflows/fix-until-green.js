// Fix with objective verification: scoped edit workers, then a test-fix loop
// that only ends when the suite is green (or provably stalled), then a gate.
// Run: pallium workflow run --script examples/workflows/fix-until-green.js "fix the failing date parser" --json
export const meta = {
  name: "fix-until-green",
  description: "Scoped edit agents, verify.untilGreen loop, safety gate",
  phases: ["scope", "fix", "verify", "gate"],
};

const task = args?.task ?? "Fix the reported defect";
const testCommand = args?.test_command ?? "go test ./...";

phase("scope");
const preflight = await pallium.preflight(task);

phase("fix");
// Edit-mode workers run in isolated worktrees; their patches auto-apply
// only after the whole run completes successfully.
const fix = await agent(
  `${task}\n\nScope from repo analysis: ${JSON.stringify(preflight.files_to_inspect ?? [])}\n` +
    `Make the smallest correct change and add a regression test that fails without the fix.`,
  {
    label: "fix",
    mode: "edit",
    schema: {
      type: "object",
      properties: {
        status: { type: "string", enum: ["fixed", "failed"] },
        files_changed: { type: "array", items: { type: "string" } },
        notes: { type: "string" },
      },
      required: ["status", "files_changed", "notes"],
    },
  },
);
log("fix agent:", fix.status);

phase("verify");
// Objective loop: check agent runs the command, fix agent repairs failures,
// stall detection breaks repeats. Never trust "done" without this.
const green = await verify.untilGreen(testCommand, { maxRounds: 3, label: "tests" });
if (!green.ok) {
  log("verification did not converge:", JSON.stringify(green));
}

phase("gate");
// Autonomous checkpoint: a verifier agent approves or fails the run.
await gate("patch-safety", "Verify the produced patches before they apply", {
  criteria: "tests pass, no secrets introduced, change scope matches the task",
});

return { fix, green };
