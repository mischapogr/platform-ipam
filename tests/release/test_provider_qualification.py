"""Attestations cannot promote simulated evidence or altered logs to stable acceptance."""

import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location(
    "check_provider_qualification", ROOT / "scripts/release/check-provider-qualification.py")
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class ProviderQualificationTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / "release-evidence").mkdir()
        self.evidence = self.root / "release-evidence/tests.log"
        self.evidence.write_bytes(b"synthetic unit-test evidence, not an acceptance report\n")
        checksum = hashlib.sha256(self.evidence.read_bytes()).hexdigest()
        self.source = "a" * 40
        self.version = "0.1.0-rc.1"
        self.record = {
            "schema_version": 1, "source_commit": self.source,
            "provider_version": self.version, "platform": "linux_amd64",
            "reviewer": "Test Reviewer", "authorization_reference": "synthetic fixture",
            "tested_terraform_versions": ["1.14.0"], "release_channel": "rc",
            "evidence_boundary": "simulation", "live_aws": "NOT_READY",
            "aws_readiness_notice": checker.AWS_NOTICE,
            "checks": {name: {"status": "passed", "evidence_file": "release-evidence/tests.log",
                              "sha256": checksum} for name in checker.CHECKS},
        }

    def check(self):
        (self.root / "RELEASE_QUALIFICATION.json").write_text(json.dumps(self.record))
        return checker.check(self.root, self.source, self.version)

    def test_simulated_rc_requires_explicit_aws_readiness_notice(self):
        self.assertEqual(self.check(), len(checker.CHECKS))
        del self.record["aws_readiness_notice"]
        with self.assertRaisesRegex(ValueError, "disclose simulation"):
            self.check()

    def test_simulation_cannot_qualify_stable_version(self):
        self.version = "0.1.0"
        self.record["provider_version"] = self.version
        with self.assertRaisesRegex(ValueError, "authorized live AWS"):
            self.check()
        self.record.update(release_channel="stable", evidence_boundary="authorized_aws_sandbox", aws_mode="live")
        self.assertEqual(self.check(), len(checker.CHECKS))

    def test_record_must_bind_source_and_version(self):
        self.record["source_commit"] = "b" * 40
        with self.assertRaisesRegex(ValueError, "source commit differs"):
            self.check()
        self.record["source_commit"] = self.source
        self.record["provider_version"] = "0.2.0-rc.1"
        with self.assertRaisesRegex(ValueError, "version or target differs"):
            self.check()

    def test_missing_or_failed_acceptance_check_is_refused(self):
        del self.record["checks"]["create_timeout_recovery"]
        with self.assertRaisesRegex(ValueError, "every required"):
            self.check()
        self.record["checks"]["create_timeout_recovery"] = {"status": "unknown"}
        with self.assertRaisesRegex(ValueError, "did not pass"):
            self.check()

    def test_altered_evidence_is_refused(self):
        self.evidence.write_bytes(b"altered report\n")
        with self.assertRaisesRegex(ValueError, "checksum differs"):
            self.check()

    def test_path_traversal_and_symlink_evidence_are_refused(self):
        check = self.record["checks"]["apply"]
        check["evidence_file"] = "release-evidence/../outside.log"
        with self.assertRaisesRegex(ValueError, "within release-evidence"):
            self.check()
        check["evidence_file"] = "release-evidence/tests.log"
        outside = self.root / "outside.log"
        outside.write_bytes(self.evidence.read_bytes())
        self.evidence.unlink()
        self.evidence.symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "symlinks"):
            self.check()

    def test_reviewer_authorization_and_exact_terraform_version_are_required(self):
        self.record["reviewer"] = " "
        with self.assertRaisesRegex(ValueError, "named reviewer"):
            self.check()
        self.record["reviewer"] = "Test Reviewer"
        self.record["authorization_reference"] = ""
        with self.assertRaisesRegex(ValueError, "named authorization_reference"):
            self.check()
        self.record["authorization_reference"] = "synthetic fixture"
        self.record["tested_terraform_versions"] = ["1.14"]
        with self.assertRaisesRegex(ValueError, "exact tested"):
            self.check()


if __name__ == "__main__":
    unittest.main()
