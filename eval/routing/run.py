#!/usr/bin/env python3
"""Isolated, explicit model/effort trials. Preparation never invokes a model.

Success requires an external rubric decision as well as any objective checks;
a provider exit of zero is not a benchmark pass. All artifacts stay local.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import signal
import shlex
import shutil
import subprocess
import tarfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
SPECS = ROOT / "docs/model-routing/tasks.jsonl"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def resolved_executable(command):
    resolved = shutil.which(command)
    if not resolved:
        return {"path": None, "hash": None}
    path = Path(resolved).resolve()
    return {"path": str(path), "hash": digest(path.read_bytes())}


def execution_signature(record):
    return (record.get("harness_hash"), record.get("pallium_binary_hash"),
            record.get("routing_config_hash"), record.get("provider"),
            record.get("model"), record.get("reasoning_effort"),
            record.get("worker_binary_path"), record.get("worker_binary_hash"),
            record.get("isolated_codex_config"))


def comparison_signature(record):
    return (record.get("harness_hash"), record.get("pallium_binary_hash"),
            record.get("routing_config_hash"), record.get("worker_binary_path"),
            record.get("worker_binary_hash"), record.get("isolated_codex_config"))


def working_tree_paths(work):
    # Treat renames as a deletion plus an addition so policy checks see both
    # paths, including a test file renamed to a non-test filename.
    status = subprocess.check_output(
        ["git", "status", "--porcelain=v1", "--no-renames", "--untracked-files=all"],
        cwd=work).decode().splitlines()
    return [line[3:] for line in status if len(line) >= 4]


def require_isolated_trial(simulation, isolated_codex_config):
    if not simulation and not isolated_codex_config:
        raise ValueError("non-simulation trials require --isolated-codex-config so ambient Codex settings cannot mix execution identities")


def routing_config_hash(config):
    return digest(json.dumps(config, sort_keys=True, separators=(",", ":")).encode())


def parse_invocation_snapshot(report):
    if report["exit_code"] != 0 or report["timed_out"]:
        raise ValueError("workflow invocation snapshot could not be inspected")
    try:
        snapshot = json.loads(report["stdout"])
    except json.JSONDecodeError as error:
        raise ValueError("workflow invocation snapshot was not valid JSON") from error
    invocations = snapshot.get("invocations")
    if not isinstance(invocations, list) or not invocations:
        raise ValueError("workflow invocation snapshot contained no provider invocation")
    return snapshot, invocations


def validate_trial_invocations(invocations, candidate):
    expected = (candidate["provider"], candidate["model"], candidate["reasoning_effort"])
    for invocation in invocations:
        actual = (invocation.get("provider"), invocation.get("model"), invocation.get("reasoning_effort"))
        if actual != expected or invocation.get("configuration_status") != "sent_to_provider":
            raise ValueError("workflow invocation did not match the requested trial candidate")


def require_isolated_records(records):
    if any(record.get("isolated_codex_config") is not True for record in records):
        raise ValueError("suggest requires isolated Codex configuration for every trial")


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def tasks(path=SPECS):
    return [json.loads(line) for line in Path(path).read_text().splitlines() if line]


def command(args, cwd, timeout=120, env=None):
    # Kill the process group, including worker-spawned tools, on timeout.
    start = time.monotonic()
    p = subprocess.Popen(args, cwd=cwd, env=env, stdout=subprocess.PIPE,
                         stderr=subprocess.PIPE, start_new_session=True)
    timed_out = False
    try:
        stdout, stderr = p.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(p.pid, signal.SIGKILL)
        stdout, stderr = p.communicate()
    return {"exit_code": p.returncode, "timed_out": timed_out,
            "duration_ms": round((time.monotonic() - start) * 1000),
            "stdout": stdout.decode(errors="replace"),
            "stderr": stderr.decode(errors="replace")}


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)


def replace_once(work, path, old, new):
    p = work / path
    content = p.read_text()
    if content.count(old) != 1:
        raise ValueError(f"fixture drift: {path}: expected one occurrence of {old!r}")
    p.write_text(content.replace(old, new, 1))


def mutate(work, task_id):
    mutations = {
        "error-wording": ("internal/workflow/provider_error.go",
                          '%s failed: %w (no stderr output captured)', '%s failed: %w'),
        "structured-prompt": ("providers/gemini.sh", 'Respond with ONLY valid JSON conforming',
                              'Respond with ONLY a single JSON object conforming'),
        "usage-retry": ("internal/workflow/runtime.go", 'usage = mergeAgentUsage(usage, retryUsage)',
                        'usage = retryUsage'),
        "read-only-permission": ("internal/workflow/claude_provider.go",
                                 'Bash(wc:*)"', 'Bash(wc:*),Bash(pallium:*)"'),
        "session-capture": ("internal/workflow/codex_provider.go",
                            'if onSessionCaptured != nil {', 'if false && onSessionCaptured != nil {'),
        "cache-identity": ("internal/workflow/store.go", "AND COALESCE(model,'')=?",
                           "AND ? IS NOT NULL"),
    }
    if task_id == "read-only-permission":
        # The same suffix also appears in the edit allowlist; constrain to its line.
        p = work / "internal/workflow/claude_provider.go"
        lines = p.read_text().splitlines(keepends=True)
        selected = [i for i, line in enumerate(lines) if line.startswith("\tclaudeReadOnlyAllowedTools ")]
        if len(selected) != 1:
            raise ValueError("read-only allowlist fixture drift")
        i = selected[0]
        lines[i] = lines[i].replace('Bash(wc:*)"', 'Bash(wc:*),Bash(pallium:*)"')
        p.write_text("".join(lines))
    elif task_id == "provider-precedence":
        p = work / "internal/workflow/provider.go"
        original = p.read_text()
        start = original.index("func ResolveProvider(")
        end = original.index("\n}", start)
        body = original[start:end]
        first = '\tif p := normalizeProvider(agentProvider); p != "" {\n\t\treturn p\n\t}\n'
        second = '\tif p := normalizeProvider(optsProvider); p != "" {\n\t\treturn p\n\t}\n'
        env = '\tif p := normalizeProvider(os.Getenv("PALLIUM_WORKFLOW_PROVIDER")); p != "" {\n\t\treturn p\n\t}\n'
        if first + second + env not in body:
            raise ValueError("provider precedence fixture drift")
        body = body.replace(first + second + env, env + first + second)
        p.write_text(original[:start] + body + original[end:])
    elif task_id in mutations:
        replace_once(work, *mutations[task_id])


def tree_hash(work):
    entries = []
    for p in sorted(work.rglob("*")):
        if p.is_file() and ".git" not in p.relative_to(work).parts:
            entries.append(str(p.relative_to(work)) + ":" + digest(p.read_bytes()))
    return digest("\n".join(entries).encode())


def prepare(args):
    destination = Path(args.output).resolve()
    if destination.exists():
        raise ValueError("output already exists; use a fresh experiment directory")
    selected = [t for t in tasks(args.tasks) if not args.task or t["id"] in args.task]
    if not selected or (args.task and set(args.task) - {t["id"] for t in selected}):
        raise ValueError("unknown or empty task selection")
    destination.mkdir(parents=True)
    for task in selected:
        directory = destination / task["id"]
        work = directory / "work"
        work.mkdir(parents=True)
        revision = task["source_revision"]
        if task["id"] == "team-timeout":
            revision = "b986618^"
        elif task["id"] == "completion-gate":
            revision = "49c0afa^"
        sha = git("rev-parse", revision).decode().strip()
        archive = git("archive", sha)
        with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
            tar.extractall(work, filter="data")
        # Fixed source snapshots do not include this new evaluation directory.
        mutate(work, task["id"])
        prompt = task["prompt"]
        if task["id"] == "session-capture":
            prompt = "Review a change that suppresses native session ID capture while a worker is running."
        if task["family"] == "research":
            # A stable, citable packet rather than live-web drift.
            packet = ROOT / "docs/model-routing/catalog.md"
            (work / "research-packet.md").write_bytes(packet.read_bytes())
            prompt += " Use research-packet.md as the supplied evidence packet; distinguish its hypotheses from observations."
        prompt += "\nWork only in this checkout. Do not inspect sibling directories or git history. Do not modify tests. State what you verified and any uncertainty."
        mode = "edit" if task["family"] in {"bounded-edit", "debugging"} else "read-only"
        record = dict(task, source_revision=sha, prompt=prompt, mode=mode,
                      fixture_hash=tree_hash(work), prompt_hash=digest(prompt.encode()),
                      status="prepared", result=None,
                      grading="objective checks where applicable plus independent rubric review")
        write(directory / "task.json", record)
        # Git state is local to this artifact, with no remote/history of solutions.
        subprocess.run(["git", "init", "-q"], cwd=work, check=True)
        subprocess.run(["git", "add", "-f", "."], cwd=work, check=True)
        subprocess.run(["git", "-c", "user.name=Pallium Eval", "-c", "user.email=eval@localhost",
                        "-c", "commit.gpgsign=false", "commit", "-qm", "Frozen task fixture"], cwd=work, check=True)
    write(destination / "experiment.json", {"version": 1, "tasks": [t["id"] for t in selected],
          "status": "prepared_no_inference", "created_at": time.time(),
          "note": "Independent rubric grading is required. This set includes research-packet-assisted tasks, not unaided research."})
    print(json.dumps({"prepared": len(selected), "output": str(destination)}))


def run(args):
    experiment = Path(args.experiment).resolve()
    harness_hash = digest(Path(__file__).read_bytes())
    config = json.loads(Path(args.config).read_text())
    candidates = {c["id"]: c for c in config["candidates"] if c["enabled"]}
    if args.candidate not in candidates:
        raise ValueError("candidate not enabled")
    candidate = candidates[args.candidate]

    require_isolated_trial(args.simulation, args.isolated_codex_config)

    if candidate["provider"] != "codex" or candidate["provider"] not in config["allowed_providers"]:
        raise ValueError("initial evaluation harness supports permitted Codex candidates only")
    task_dir = experiment / args.task
    task = json.loads((task_dir / "task.json").read_text())
    run_dir = task_dir / "runs" / (args.candidate + "-" + uuid.uuid4().hex[:10])
    work = run_dir / "work"
    work.mkdir(parents=True)
    # Archive the frozen fixture, not a previous trial's changed checkout.
    archive = subprocess.check_output(["git", "archive", "HEAD"], cwd=task_dir / "work")
    with tarfile.open(fileobj=io.BytesIO(archive)) as tar:
        tar.extractall(work, filter="data")
    if tree_hash(work) != task["fixture_hash"]:
        raise ValueError("fixture hash changed")
    subprocess.run(["git", "init", "-q"], cwd=work, check=True)
    subprocess.run(["git", "add", "-f", "."], cwd=work, check=True)
    subprocess.run(["git", "-c", "user.name=Pallium Eval", "-c", "user.email=eval@localhost",
                    "-c", "commit.gpgsign=false", "commit", "-qm", "Trial fixture"], cwd=work, check=True)
    binary = str(Path(args.pallium).resolve())
    script = run_dir / "trial.workflow.js"
    # A single worker via Pallium: the same effort transport and usage capture
    # as production, including bounded schema handling and worktree containment.
    options = {"provider": candidate["provider"], "model": candidate["model"],
               "reasoning_effort": candidate["reasoning_effort"], "mode": task["mode"]}
    script.write_text("return await agent(" + json.dumps(task["prompt"]) + "," + json.dumps(options) + ");\n")
    env = dict(os.environ)
    # An explicit off policy makes ambient repo/host routing irrelevant.
    trial_config = dict(config, mode="off")
    write(run_dir / "routing.json", trial_config)
    env["PALLIUM_ROUTING_CONFIG"] = str(run_dir / "routing.json")
    run_id = "wf-eval-" + uuid.uuid4().hex[:16]
    db = run_dir / "pallium.sqlite"
    codex_binary = args.codex
    if args.isolated_codex_config:
        resolved = shutil.which(args.codex)
        if not resolved:
            raise ValueError("Codex executable unavailable")
        wrapper = run_dir / "codex-trial.sh"
        wrapper.write_text("#!/bin/sh\nif [ \"$1\" = exec ]; then\n shift\n exec " + shlex.quote(resolved) + " exec --ignore-user-config \"$@\"\nfi\nexec " + shlex.quote(resolved) + " \"$@\"\n")
        wrapper.chmod(0o700)
        codex_binary = str(wrapper)
    args_cli = [binary, "workflow", "run", task["prompt"], "--script", str(script),
                "--db", str(db), "--id", run_id, "--max-agents", "1",
                "--agent-timeout", str(args.timeout), "--codex", codex_binary, "--json"]
    # Flag names are validated against the built CLI in the harness smoke tests.
    result = command(args_cli, work, args.timeout + 30, env)
    write(run_dir / "process.json", result)
    report = command([binary, "workflow", "inspect", run_id, "--db", str(db), "--json"], work, 30, env)
    (run_dir / "snapshot.json").write_text(report["stdout"])
    snapshot, invocations = parse_invocation_snapshot(report)
    validate_trial_invocations(invocations, candidate)
    costs = [v.get("cost_usd") for v in invocations]
    total_cost = sum(costs) if costs and all(c is not None for c in costs) else None
    objective = None
    if result["exit_code"] == 0 and task["mode"] == "edit":
        checks = command(["go", "test", "./internal/workflow"], work, args.check_timeout, env)
        write(run_dir / "checks.json", checks)
        changed = working_tree_paths(work)
        test_changes = [p for p in changed if p.endswith("_test.go") or p.startswith("eval/")]
        objective = {"passed": checks["exit_code"] == 0 and not test_changes,
                     "test_changes": test_changes, "duration_ms": checks["duration_ms"]}
    worker_identity = resolved_executable(args.codex)
    record = {"harness_hash": harness_hash, "pallium_binary_hash": digest(Path(binary).read_bytes()), "routing_config_hash": routing_config_hash(config), "isolated_codex_config": args.isolated_codex_config, "simulation": args.simulation, "worker_binary": args.codex, "worker_binary_path": worker_identity["path"], "worker_binary_hash": worker_identity["hash"], "objective_checks": objective, "run_id": run_id, "task_id": task["id"], "family": task["family"],
              "split": task["proposed_split"], "split_group": task.get("split_group",task["id"]), "candidate": args.candidate,
              "provider": candidate["provider"], "model": candidate["model"], "reasoning_effort": candidate["reasoning_effort"],
              "fixture_hash": task["fixture_hash"], "prompt_hash": task["prompt_hash"],
              "duration_ms": result["duration_ms"] + (objective["duration_ms"] if objective else 0), "cost_usd": total_cost,
              "invocations": invocations, "provider_exit_code": result["exit_code"],
              "timed_out": result["timed_out"], "outcome": "pending_review" if result["exit_code"] == 0 else "failed",
              "rubric": task["acceptance"], "review": None}
    write(run_dir / "result.json", record)
    print(json.dumps({"result": str(run_dir / "result.json"), "outcome": record["outcome"], "cost_usd": total_cost}))


def grade(args):
    path = Path(args.result)
    r = json.loads(path.read_text())
    if r["provider_exit_code"] != 0 and args.outcome == "accepted":
        raise ValueError("failed execution cannot be accepted")
    if args.outcome == "accepted" and r.get("objective_checks") and not r["objective_checks"]["passed"]:
        raise ValueError("objective checks failed; cannot accept")
    if r.get("review"):
        raise ValueError("already graded; preserve original judgment")
    evidence = Path(args.evidence).resolve()
    if not evidence.is_file() or not evidence.read_text().strip():
        raise ValueError("nonempty independent grading evidence is required")
    r["outcome"] = args.outcome
    r["review"] = {"reviewer": args.reviewer, "evidence_path": str(evidence),
                   "evidence_hash": digest(evidence.read_bytes()), "graded_at": time.time()}
    write(path, r)


def summarize(records):
    groups = {}
    for r in records:
        key = r["candidate"]
        g = groups.setdefault(key, {"attempts": 0, "accepted": 0, "pending": 0,
                                   "known_cost_usd": 0, "unknown_cost_attempts": 0, "durations": []})
        g["attempts"] += 1
        g["accepted"] += r["outcome"] == "accepted"
        g["pending"] += r["outcome"] == "pending_review"
        cost = r.get("cost_usd")
        if cost is None:
            g["unknown_cost_attempts"] += 1
        else:
            if cost < 0:
                raise ValueError("negative cost")
            g["known_cost_usd"] += cost
        g["durations"].append(r["duration_ms"])
    for g in groups.values():
        g["acceptance_rate"] = g["accepted"] / g["attempts"]
        g["cost_per_success_usd"] = g["known_cost_usd"] / g["accepted"] if g["accepted"] and not g["unknown_cost_attempts"] and not g["pending"] else None
        durations = sorted(g.pop("durations"))
        g["p50_ms"] = durations[(len(durations) - 1) // 2]
        g["p95_ms"] = durations[min(len(durations) - 1, __import__("math").ceil(len(durations) * .95) - 1)]
    return groups


def report(args):
    all_records = [json.loads(p.read_text()) for p in Path(args.experiment).glob("*/runs/*/result.json")]
    records = [r for r in all_records if not r.get("simulation", True)]
    groups = {}
    for r in records:
        key = json.dumps(comparison_signature(r), separators=(",", ":"))
        groups.setdefault(key, []).append(r)
    print(json.dumps({"simulations_excluded": len(all_records) - len(records), "comparison_groups": {key: summarize(values) for key, values in groups.items()}, "promotion": "not_established",
                      "note": "No automatic non-inferiority claim; inspect paired tasks, independent grading, and confidence intervals."}, indent=2))


def suggest(args):
    """Produce a shadow policy proposal from complete paired calibration data."""
    config = json.loads(Path(args.config).read_text())
    all_records = [json.loads(p.read_text()) for p in Path(args.experiment).glob("*/runs/*/result.json")]
    records = [r for r in all_records if r.get("simulation") is False and r.get("split") == "calibration"]
    require_isolated_records(records)
    comparison_signatures = {comparison_signature(r) for r in records}
    candidate_signatures = {}
    for r in records:
        candidate_signatures.setdefault(r["candidate"], set()).add(execution_signature(r))
    if (len(comparison_signatures) != 1 or
            any(None in signature for signature in comparison_signatures) or
            any(len(signatures) != 1 or any(None in signature for signature in signatures)
                for signatures in candidate_signatures.values())):
        raise ValueError("suggest requires one pinned harness/binary/config group and one execution identity per candidate")
    if next(iter(comparison_signatures))[2] != routing_config_hash(config):
        raise ValueError("suggest config does not match the routing configuration recorded by the trials")
    if any(r["outcome"] == "pending_review" for r in records):
        raise ValueError("grade calibration outcomes before suggesting rules")
    enabled = {c["id"] for c in config["candidates"] if c["enabled"] and c["provider"] in config["allowed_providers"]}
    if args.baseline not in enabled:
        raise ValueError("baseline is not enabled in the policy")
    proposals = {}
    for family in sorted({r["family"] for r in records}):
        family_rows = [r for r in records if r["family"] == family]
        baseline = [r for r in family_rows if r["candidate"] == args.baseline]
        baseline_tasks = {r["task_id"] for r in baseline}
        if len({r.get("split_group",r["task_id"]) for r in baseline}) < args.min_tasks:
            continue
        best = None
        for candidate in sorted(enabled - {args.baseline}):
            rows = [r for r in family_rows if r["candidate"] == candidate]
            if {r["task_id"] for r in rows} != baseline_tasks:
                continue
            paired = True
            for task_id in baseline_tasks:
                left = [r for r in baseline if r["task_id"] == task_id]
                right = [r for r in rows if r["task_id"] == task_id]
                if len(left) != len(right) or sum(r["outcome"] == "accepted" for r in right) < sum(r["outcome"] == "accepted" for r in left):
                    paired = False
            if not paired:
                continue
            b = summarize(baseline)[args.baseline]
            c = summarize(rows)[candidate]
            if c["accepted"] == 0:
                continue
            known = b["cost_per_success_usd"] is not None and c["cost_per_success_usd"] is not None
            cheaper = known and c["cost_per_success_usd"] <= .8 * b["cost_per_success_usd"] and c["p95_ms"] <= 1.1 * b["p95_ms"]
            # Unknown billing cannot establish no-cost-increase for a speed win.
            faster = known and c["p95_ms"] <= .8 * b["p95_ms"] and c["cost_per_success_usd"] <= b["cost_per_success_usd"]
            if cheaper or faster:
                rank = (c["cost_per_success_usd"], c["p95_ms"], candidate)
                if best is None or rank < best:
                    best = rank
        if best:
            proposals[family] = best[2]
    config["mode"] = "shadow"
    config["rules"].update(proposals)
    path = Path(args.output)
    with path.open("x") as f:
        json.dump(config, f, indent=2)
        f.write("\n")
    write(path.with_suffix(path.suffix + ".evidence.json"), {
        "baseline": args.baseline, "proposed_rules": proposals, "min_independent_tasks": args.min_tasks,
        "outcomes_hash": digest(json.dumps(records, sort_keys=True).encode()),
        "promotion": "shadow_only", "note": "Calibration proposal, not held-out proof or permission to enable default Auto."})
    print(json.dumps({"proposal": str(path), "rules": proposals, "mode": "shadow"}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="action", required=True)
    p = sub.add_parser("prepare"); p.add_argument("--output", required=True); p.add_argument("--task", action="append"); p.add_argument("--tasks", default=str(SPECS))
    p = sub.add_parser("run"); p.add_argument("--experiment", required=True); p.add_argument("--task", required=True); p.add_argument("--candidate", required=True); p.add_argument("--config", required=True); p.add_argument("--pallium", required=True); p.add_argument("--timeout", type=int, default=180); p.add_argument("--check-timeout", type=int, default=180); p.add_argument("--codex", default="codex"); p.add_argument("--simulation", action="store_true"); p.add_argument("--isolated-codex-config", action="store_true")
    p = sub.add_parser("grade"); p.add_argument("--result", required=True); p.add_argument("--outcome", choices=["accepted", "rejected"], required=True); p.add_argument("--reviewer", required=True); p.add_argument("--evidence", required=True)
    p = sub.add_parser("report"); p.add_argument("--experiment", required=True)
    p = sub.add_parser("suggest"); p.add_argument("--experiment", required=True); p.add_argument("--config", required=True); p.add_argument("--output", required=True); p.add_argument("--baseline", default="astra-high"); p.add_argument("--min-tasks", type=int, default=5)
    args = parser.parse_args()
    if hasattr(args, "timeout") and args.timeout <= 0:
        parser.error("timeout must be positive")
    if hasattr(args, "min_tasks") and args.min_tasks < 2:
        parser.error("min-tasks must be at least two independent tasks")
    globals()[args.action](args)


if __name__ == "__main__":
    main()
