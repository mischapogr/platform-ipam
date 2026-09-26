"""Guards for the work-plan runner's model spend and stop decisions."""

import importlib.machinery
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("run-work-plan")
LOADER = importlib.machinery.SourceFileLoader("work_plan_runner", str(SCRIPT))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
RUNNER = importlib.util.module_from_spec(SPEC)
LOADER.exec_module(RUNNER)


def item():
    return {
        "id": "GATE_ONE", "tier": "M", "title": "Verify one package",
        "prompt": "Check the package", "check_first": True,
        "allow_existing_changes": True, "checks": [["test-command"]],
    }


class WorkPlanRunnerTest(unittest.TestCase):
    def test_rejects_duplicate_ids_before_spending_tokens(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "queue.json"
            path.write_text(json.dumps({"schema_version": 1, "items": [item(), item()]}))
            with self.assertRaisesRegex(ValueError, "Duplicate queue ID"):
                RUNNER.load_queue(path)

    def test_clean_precheck_uses_no_model(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(RUNNER, "git_dirty", return_value=True), \
                mock.patch.object(RUNNER, "execute", return_value=0) as execute, \
                mock.patch.object(RUNNER, "agent_turn") as agent:
            state = {}
            status, _ = RUNNER.run_item(item(), state, Path(directory))
            self.assertEqual(status, "complete")
            self.assertTrue(RUNNER.is_complete(item(), state))
            self.assertEqual(execute.call_count, 1)
            agent.assert_not_called()

    def test_incomplete_precheck_stops_without_model(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(RUNNER, "git_dirty", return_value=False), \
                mock.patch.object(RUNNER, "execute", return_value=2), \
                mock.patch.object(RUNNER, "agent_turn") as agent:
            status, _ = RUNNER.run_item(item(), {}, Path(directory))
            self.assertEqual(status, "blocked")
            agent.assert_not_called()

    def test_failed_gate_gets_one_escalated_repair_then_stops(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(RUNNER, "git_dirty", return_value=False), \
                mock.patch.object(RUNNER, "execute", side_effect=[1, 1, 1]), \
                mock.patch.object(RUNNER, "agent_turn", return_value=("ready_for_checks", "fixed")) as agent:
            status, _ = RUNNER.run_item(item(), {}, Path(directory))
            self.assertEqual(status, "failed")
            self.assertEqual(agent.call_count, 2)
            self.assertEqual(agent.call_args_list[0].args[1:4], ("codex", "gpt-6-sol", "medium"))
            self.assertEqual(agent.call_args_list[1].args[1:4], ("codex", "gpt-6-sol", "high"))

    def test_claude_route_uses_same_queue_and_escalates_model(self):
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(RUNNER, "git_dirty", return_value=False), \
                mock.patch.object(RUNNER, "execute", side_effect=[1, 1, 0]), \
                mock.patch.object(RUNNER, "agent_turn", return_value=("ready_for_checks", "fixed")) as agent:
            status, _ = RUNNER.run_item(item(), {}, Path(directory), tool="claude")
            self.assertEqual(status, "complete")
            self.assertEqual(agent.call_args_list[0].args[1:4], ("claude", "sonnet", "medium"))
            self.assertEqual(agent.call_args_list[1].args[1:4], ("claude", "sonnet", "high"))

    def test_claude_structured_result_is_read_from_envelope(self):
        with tempfile.TemporaryDirectory() as directory:
            def fake_execute(command, log_path, stderr_path):
                self.assertEqual(command[:2], ["claude", "-p"])
                self.assertIn("--json-schema", command)
                self.assertIn("--permission-mode", command)
                log_path.write_text(json.dumps({"structured_output": {
                    "status": "ready_for_checks", "summary": "ready",
                }}))
                return 0

            with mock.patch.object(RUNNER, "execute", side_effect=fake_execute):
                status, summary = RUNNER.agent_turn(
                    item(), "claude", "sonnet", "medium", Path(directory),
                )
            self.assertEqual((status, summary), ("ready_for_checks", "ready"))


if __name__ == "__main__":
    unittest.main()
