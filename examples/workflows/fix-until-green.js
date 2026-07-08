// Fix with objective verification: repo-memory scoping, then a single
// test-fix loop that only ends when the suite is green (or provably
// stalled), then a gate. verify.untilGreen() is the sole fix+verify
// mechanism here — see the note in the "verify" phase for why a separate
// standalone edit-mode fix agent does NOT chain correctly with it.
// Run: pallium workflow run --script examples/workflows/fix-until-green.js "fix the failing date parser" --args '{"task":"fix the failing date parser","test_command":"go test ./..."}' --json
// Note: the positional task string only becomes run metadata (visible via
// `pallium workflow inspect`) — it is NOT injected into the script's `args`
// global. Pass task/test_command through --args as well so `args?.task` and
// `args?.test_command` resolve.
export const meta = {
  name: "fix-until-green",
  description: "Repo-memory scoping, verify.untilGreen fix+verify loop, safety gate",
  phases: ["scope", "verify", "gate"],
};

const task = args?.task ?? "Fix the reported defect";
const testCommand = args?.test_command ?? "go test ./...";

phase("scope");
// Repo memory narrows what matters before the fix loop starts; it does not
// make any edits itself.
const preflight = await pallium.preflight(task);
log("scoped files:", (preflight.files_to_inspect ?? []).length);

phase("verify");
// Edit-mode agents (mode: "edit") run in isolated worktrees; their patches
// are only applied to the real repo after this whole script returns
// successfully (see internal/workflow/runtime.go ApplyPatches). That means
// a standalone edit-mode "fix" agent run before verify.untilGreen would be
// invisible to untilGreen's check step, which runs directly against the
// real repo. So verify.untilGreen must be the sole fix+verify mechanism:
// its internal loop repeats check (mode: "test") and fix (mode: "edit",
// isolated worktree) rounds against the failing command itself, with stall
// detection, until it converges or gives up.
//
// Known limitation: today each fix round's isolated-worktree patch is also
// not visible to the *next* round's check (same ApplyPatches timing as
// above), so a genuinely failing initial command can still end this loop
// with `ok: false` / stalled rather than converging within one run. A fix
// (a persistent loop worktree shared by check and fix rounds) is in
// progress on the `feat/workflow-reliability` branch (PR #15, commit
// a573216); once merged this recipe converges without changes here.
const green = await verify.untilGreen(testCommand, { maxRounds: 3, label: "tests" });
if (!green.ok) {
  // Fail before the gate/return so no patches are applied on an
  // unverified or still-failing result.
  throw new Error("verification did not converge: " + JSON.stringify(green));
}

phase("gate");
// Autonomous checkpoint: a verifier agent approves or fails the run.
await gate("patch-safety", "Verify the produced patches before they apply", {
  criteria: "tests pass, no secrets introduced, change scope matches the task",
});

return { green };
