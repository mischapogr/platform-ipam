"""Check that committed synthetic pilot bundles preserve their source hashes."""

import hashlib
import json
from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
DEMOS = ("HUNDRED_ACCOUNT_DEMO", "FIVE_HUNDRED_ACCOUNT_DEMO")


class MigrationWorkspaceFixtureTest(unittest.TestCase):
    def test_committed_demo_csv_hashes_match_execution_evidence(self):
        for name in DEMOS:
            with self.subTest(demo=name):
                source = ROOT / "examples/migration-workspace" / name
                networks = (source / "networks.csv").read_bytes()
                failures = (source / "failures.csv").read_bytes()
                self.assertNotIn(b"\r\n", networks, "generated networks.csv must use LF")
                self.assertNotIn(b"\r\n", failures, "generated failures.csv must use LF")
                overview = json.loads((source / "pilot-overview.json").read_text())
                execution = json.loads((source / "execution.json").read_text())
                self.assertEqual(overview["input_sha256"]["networks"],
                                 hashlib.sha256(networks).hexdigest())
                self.assertEqual(overview["input_sha256"]["run"], hashlib.sha256(
                    (source / "run.json").read_bytes()).hexdigest())
                source_by_hash = {
                    "approved_plan": "approved-plan.json",
                    "matrix": "matrix.yaml",
                    "migration_plan": "migration.yaml",
                    "pilot_scope": "pilot-scope.json",
                    "protected": "fixed.yaml",
                }
                for key, filename in source_by_hash.items():
                    self.assertEqual(overview["input_sha256"][key], hashlib.sha256(
                        (source / filename).read_bytes()).hexdigest(), key)
                self.assertEqual(execution["input_sha256"], overview["input_sha256"])
                self.assertEqual(overview["execution_evidence"]["reported_moves"],
                                 len(execution["moves"]))


if __name__ == "__main__":
    unittest.main()
