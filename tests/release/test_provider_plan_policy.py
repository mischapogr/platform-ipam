"""Consumer replacement policy ships and runs without the monorepo."""

import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "providers/terraform/scripts/check-replacement-plan.py"
spec = importlib.util.spec_from_file_location("consumer_plan_policy", SCRIPT)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


def plan(old="orders-v1", new="orders-v2", actions=None):
    return {"format_version": "1.2", "planned_values": {}, "resource_changes": [{
        "mode": "managed", "type": "platformipam_allocation",
        "address": 'module.network.platformipam_allocation.vpc["orders"]',
        "change": {"actions": actions or ["delete", "create"],
                   "before": {"allocation_key": old},
                   "after": {"allocation_key": new}, "after_unknown": {}},
    }]}


class ProviderPlanPolicyTest(unittest.TestCase):
    def test_actual_refresh_only_saved_plan_without_resource_changes_is_allowed(self):
        # Terraform 1.14.9's retained local refresh plan omitted resource_changes
        # and resource_drift, while preserving these plan-only fields and outputs.
        document = {
            "format_version": "1.2", "terraform_version": "1.14.9",
            "planned_values": {"outputs": {"cidr": {
                "sensitive": False, "type": "string", "value": "10.64.8.0/22"}},
                "root_module": {}},
            "prior_state": {"format_version": "1.0", "values": {
                "root_module": {"resources": [{"address": "platformipam_allocation.vpc",
                    "mode": "managed", "type": "platformipam_allocation",
                    "values": {"id": "alloc_01", "cidr": "10.64.8.0/22"}}]}}},
            "configuration": {"root_module": {"resources": [{
                "address": "platformipam_allocation.vpc", "mode": "managed",
                "type": "platformipam_allocation"}]}},
            "timestamp": "2026-10-10T13:49:48Z", "complete": True,
            "errored": False, "applyable": True,
            "output_changes": {"cidr": {"actions": ["no-op"],
                "before": "10.64.8.0/22", "after": "10.64.8.0/22"}},
        }
        self.assertEqual(policy.replacement_errors(document), [])
        for fields in ({"complete": False}, {"errored": True},
                       {"deferred_changes": [{}]}, {"resource_changes": None},
                       {"configuration": None}, {"timestamp": None}):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                policy.replacement_errors(dict(document, **fields))

    def test_known_new_replacement_key_is_allowed_in_both_orders(self):
        for actions in (["delete", "create"], ["create", "delete"]):
            with self.subTest(actions=actions):
                self.assertEqual(policy.replacement_errors(plan(actions=actions)), [])

    def test_same_missing_empty_or_unknown_replacement_key_is_rejected(self):
        for new in ("orders-v1", "", None, 42):
            with self.subTest(new=new):
                self.assertEqual(policy.replacement_errors(plan(new=new)),
                                 ['module.network.platformipam_allocation.vpc["orders"]'])
        for unknown in (True, {"allocation_key": True}):
            document = plan()
            document["resource_changes"][0]["change"]["after_unknown"] = unknown
            self.assertTrue(policy.replacement_errors(document))

    def test_incomplete_deferred_or_errored_plan_is_rejected_even_with_new_key(self):
        for fields in ({"complete": False}, {"deferred_changes": [{"reason": "unknown"}]},
                       {"errored": True}):
            with self.subTest(fields=fields), self.assertRaises(ValueError):
                policy.replacement_errors(dict(plan(), **fields))

    def test_state_or_future_format_cannot_pass(self):
        for document in ({"format_version": "1.0", "values": {}},
                         {"format_version": "1.2", "planned_values": {}},
                         dict(plan(), format_version="2.0"),
                         dict(plan(), format_version="1.2future")):
            with self.subTest(document=document), self.assertRaises(ValueError):
                policy.replacement_errors(document)

    def test_unknown_modes_and_malformed_replacement_shapes_fail_closed(self):
        variants = []
        for mode in ("future", None, 42):
            document = plan()
            document["resource_changes"][0]["mode"] = mode
            variants.append(document)
        for change in (None, {"actions": ["migrate"]},
                       {"actions": ["delete", "create"], "before": []},
                       {"actions": ["delete", "create"], "after_unknown": "false"}):
            document = plan()
            document["resource_changes"][0]["change"] = change
            variants.append(document)
        for document in variants:
            with self.subTest(document=document), self.assertRaises(ValueError):
                policy.replacement_errors(document)

    def test_initial_create_metadata_update_and_data_reads_are_not_replacements(self):
        for actions in (["create"], ["update"], ["no-op"], ["delete"]):
            self.assertEqual(policy.replacement_errors(plan(new="orders-v1", actions=actions)), [])
        document = plan(new="orders-v1")
        document["resource_changes"][0]["mode"] = "data"
        self.assertEqual(policy.replacement_errors(document), [])
        document["resource_changes"][0]["mode"] = "managed"
        document["resource_changes"][0]["type"] = "aws_vpc"
        self.assertEqual(policy.replacement_errors(document), [])

    def test_monorepo_check_uses_the_exported_policy_implementation(self):
        check_spec = importlib.util.spec_from_file_location("monorepo_provider_checks", ROOT / "scripts/ai/checks.py")
        checks = importlib.util.module_from_spec(check_spec)
        check_spec.loader.exec_module(checks)
        self.assertEqual(Path(checks.replacement_errors.__code__.co_filename).resolve(), SCRIPT)
        self.assertEqual(checks.replacement_errors(plan(new="orders-v1")),
                         policy.replacement_errors(plan(new="orders-v1")))

    def test_portable_cli_runs_from_other_directory_with_absolute_or_relative_paths(self):
        with tempfile.TemporaryDirectory(prefix="provider policy ") as temporary:
            root = Path(temporary)
            portable = root / "exported provider/scripts/check-replacement-plan.py"
            portable.parent.mkdir(parents=True)
            shutil.copyfile(SCRIPT, portable)
            consumer = root / "consumer deployment"
            consumer.mkdir()
            saved = consumer / "saved plan.json"
            for unsafe in (False, True):
                saved.write_text(json.dumps(plan(new="orders-v1" if unsafe else "orders-v2")))
                for path in (saved, Path(saved.name)):
                    result = subprocess.run([sys.executable, "-B", str(portable), "--plan-json", str(path)],
                                            cwd=consumer, capture_output=True, text=True)
                    with self.subTest(unsafe=unsafe, path=path):
                        self.assertEqual(result.returncode, int(unsafe), result.stderr)
                        report = json.loads(result.stdout)
                        self.assertEqual(report["status"], "FAILED" if unsafe else "PASSED")
                        self.assertEqual(bool(report["unsafe_allocations"]), unsafe)

    def test_cli_rejects_unreadable_or_malformed_plan_with_compact_diagnostic(self):
        with tempfile.TemporaryDirectory() as temporary:
            missing = Path(temporary) / "missing.json"
            invalid = Path(temporary) / "invalid.json"
            invalid.write_text("not JSON")
            for path in (missing, invalid):
                result = subprocess.run([sys.executable, "-B", str(SCRIPT), "--plan-json", str(path)],
                                        capture_output=True, text=True)
                self.assertEqual(result.returncode, 1)
                self.assertEqual(json.loads(result.stdout)["status"], "FAILED")
                self.assertNotIn("Traceback", result.stderr)


if __name__ == "__main__":
    unittest.main()
