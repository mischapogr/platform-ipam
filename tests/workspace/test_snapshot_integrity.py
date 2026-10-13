"""Host-runnable check for saved report corruption detection."""

import hashlib
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/compose/netbox/ui/platform_ipam_workspace/integrity.py"
spec = importlib.util.spec_from_file_location("workspace_integrity", SCRIPT)
integrity = importlib.util.module_from_spec(spec)
spec.loader.exec_module(integrity)


class SnapshotIntegrityTest(unittest.TestCase):
    def test_valid_report_and_changed_report(self):
        report = {"version": 1, "scope": {"accounts": ["111111111111"]}}
        body = json.dumps(report, sort_keys=True, indent=2) + "\n"
        snapshot = SimpleNamespace(report=report,
                                   report_sha256=hashlib.sha256(body.encode()).hexdigest())
        self.assertTrue(integrity.snapshot_digest_matches(snapshot))
        snapshot.report["scope"]["accounts"] = []
        self.assertFalse(integrity.snapshot_digest_matches(snapshot))


if __name__ == "__main__":
    unittest.main()
