import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("routing_eval", Path(__file__).with_name("run.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class AccountingTests(unittest.TestCase):
    def row(self, outcome, cost):
        return dict(candidate="model-effort", outcome=outcome, cost_usd=cost, duration_ms=10)

    def test_failures_remain_in_cost_per_success(self):
        result = runner.summarize([self.row("accepted", 2), self.row("failed", 3)])
        self.assertEqual(result["model-effort"]["cost_per_success_usd"], 5)

    def test_unknown_cost_cannot_be_zero_or_savings(self):
        result = runner.summarize([self.row("accepted", 2), self.row("failed", None)])
        self.assertIsNone(result["model-effort"]["cost_per_success_usd"])

    def test_pending_is_not_success(self):
        result = runner.summarize([self.row("pending_review", 1)])
        self.assertEqual(result["model-effort"]["accepted"], 0)
        self.assertIsNone(result["model-effort"]["cost_per_success_usd"])

    def test_zero_success_has_no_finite_cost_per_success(self):
        result = runner.summarize([self.row("failed", 1)])
        self.assertIsNone(result["model-effort"]["cost_per_success_usd"])

    def test_negative_cost_is_invalid(self):
        with self.assertRaises(ValueError):
            runner.summarize([self.row("accepted", -1)])

    def test_dataset_has_unique_ids_and_existing_sources(self):
        tasks = runner.tasks()
        self.assertEqual(len(tasks), 24)
        self.assertEqual(len({t["id"] for t in tasks}), 24)
        for task in tasks:
            self.assertTrue((runner.ROOT / task["source_path"]).is_file())
        groups = {}
        for task in tasks:
            groups.setdefault(task["split_group"], set()).add(task["proposed_split"])
        self.assertTrue(all(len(splits) == 1 for splits in groups.values()))

    def test_execution_signature_includes_effective_configuration(self):
        base = dict(harness_hash="h", pallium_binary_hash="p", routing_config_hash="r",
                    provider="codex", model="gpt-6-astra", reasoning_effort="high",
                    worker_binary_path="/bin/codex", worker_binary_hash="w",
                    isolated_codex_config=True)
        changed = dict(base, model="gpt-5.6-luna")
        self.assertNotEqual(runner.execution_signature(base), runner.execution_signature(changed))

    def test_non_simulation_trials_require_isolated_codex_config(self):
        with self.assertRaises(ValueError):
            runner.require_isolated_trial(False, False)
        runner.require_isolated_trial(False, True)
        runner.require_isolated_trial(True, False)

    def test_invocation_snapshot_must_be_successful_and_observable(self):
        with self.assertRaises(ValueError):
            runner.parse_invocation_snapshot({"exit_code": 1, "timed_out": False, "stdout": "{}"})
        with self.assertRaises(ValueError):
            runner.parse_invocation_snapshot({"exit_code": 0, "timed_out": False, "stdout": "not-json"})
        with self.assertRaises(ValueError):
            runner.parse_invocation_snapshot({"exit_code": 0, "timed_out": False, "stdout": "{}"})
        snapshot, invocations = runner.parse_invocation_snapshot({
            "exit_code": 0, "timed_out": False,
            "stdout": json.dumps({"invocations": [{"provider": "codex"}]})})
        self.assertEqual(snapshot["invocations"], invocations)

    def test_routing_config_hash_changes_with_candidate_mapping(self):
        first = {"candidates": [{"id": "fast", "model": "a"}]}
        second = {"candidates": [{"id": "fast", "model": "b"}]}
        self.assertNotEqual(runner.routing_config_hash(first), runner.routing_config_hash(second))


if __name__ == "__main__":
    unittest.main()
