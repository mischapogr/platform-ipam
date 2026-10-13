"""Pending matrices cannot bypass version, pairing or stable release qualification."""

import copy
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "providers/terraform/scripts/check-compatibility.py"
spec = importlib.util.spec_from_file_location("provider_compatibility", SCRIPT)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


class ProviderCompatibilityTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.version = "0.1.0-rc.1"
        self.matrix = {
            "schema_version": 1, "provider_version": self.version, "platform_api_major": 1,
            "platform_release_versions": [], "aws_integration": "not_ready",
            "qualification": {"status": "pending", "scope": "simulation",
                              "terraform_cli_versions": ["1.14.9"], "platforms": ["linux_amd64"]},
        }
        self.manifest = {"version": 1, "metadata": {"protocol_versions": ["6.0"]}}
        self.attestation = {"provider_version": self.version, "evidence_boundary": "simulation",
                            "tested_terraform_versions": ["1.14.9"], "platform": "linux_amd64"}
        self.attestation_path = self.root / "RELEASE_QUALIFICATION.json"
        self.save()

    def save(self):
        (self.root / "VERSION").write_text(self.version + "\n")
        (self.root / "COMPATIBILITY.json").write_text(json.dumps(self.matrix))
        (self.root / "terraform-registry-manifest.json").write_text(json.dumps(self.manifest))
        self.attestation_path.write_text(json.dumps(self.attestation))

    def check(self, *, release=False, expected=None, attestation=False):
        self.save()
        return checker.check(self.root, expected, release=release,
                             qualification=self.attestation_path if attestation else None)

    def stable(self):
        self.version = "0.1.0"
        self.matrix.update(provider_version=self.version, platform_release_versions=["0.2.0"], aws_integration="ready")
        self.matrix["qualification"].update(status="qualified", scope="authorized_aws_sandbox")
        self.attestation.update(provider_version=self.version, evidence_boundary="authorized_aws_sandbox",
                                tested_platform_versions=["0.2.0"])

    def test_pending_rc_is_structurally_valid_without_claiming_qualification(self):
        result = self.check(expected=self.version)
        self.assertEqual(result["qualification_status"], "pending")
        self.assertEqual(result["platform_release_versions"], [])
        self.assertIs(result["publication_acceptance"], False)
        self.assertIs(result["execution_authenticated"], False)

    def test_pending_rc_cannot_pass_release_mode_even_with_matching_attestation(self):
        with self.assertRaisesRegex(ValueError, "pending metadata is structural only"):
            self.check(release=True, attestation=True)

    def test_qualified_simulated_rc_can_match_empty_product_list(self):
        self.matrix["qualification"]["status"] = "qualified"
        result = self.check(release=True, expected=self.version, attestation=True)
        self.assertEqual(result["qualification_scope"], "simulation")
        self.assertEqual(result["aws_integration"], "not_ready")
        self.assertIs(result["publication_acceptance"], False)

    def test_release_mode_requires_attestation(self):
        self.matrix["qualification"]["status"] = "qualified"
        with self.assertRaisesRegex(ValueError, "requires a source-bound qualification"):
            self.check(release=True)

    def test_stable_requires_qualified_live_pairings_and_ready_aws(self):
        self.stable()
        self.assertEqual(self.check(release=True, attestation=True)["qualification_status"], "qualified")
        original = copy.deepcopy(self.matrix)
        for field, value in (("status", "pending"), ("scope", "simulation"),
                             ("platform_release_versions", []), ("aws_integration", "not_ready")):
            with self.subTest(field=field):
                self.matrix = copy.deepcopy(original)
                if field in {"status", "scope"}:
                    self.matrix["qualification"][field] = value
                else:
                    self.matrix[field] = value
                with self.assertRaisesRegex(ValueError, "authorized AWS sandbox scope"):
                    self.check()

    def test_rc_must_retain_simulation_scope_and_aws_warning(self):
        self.matrix["qualification"]["scope"] = "authorized_aws_sandbox"
        with self.assertRaisesRegex(ValueError, "retain simulation scope"):
            self.check()
        self.matrix["qualification"]["scope"] = "simulation"
        self.matrix["aws_integration"] = "ready"
        with self.assertRaisesRegex(ValueError, "retain simulation scope"):
            self.check()

    def test_version_file_matrix_and_tag_must_match(self):
        with self.assertRaisesRegex(ValueError, "expected release version"):
            self.check(expected="0.1.0-rc.2")
        self.matrix["provider_version"] = "0.1.0-rc.2"
        with self.assertRaisesRegex(ValueError, "differs from VERSION"):
            self.check()

    def test_provider_version_has_canonical_supported_release_channel(self):
        for value in ("v0.1.0", "00.1.0", "0.01.0", "0.1", "0.1.0-rc.01", "0.1.0-alpha.1",
                      "0.1.0-beta", "0.1.0+build", "0.1.0-rc.1+build", "0.1.0-rc.1\n"):
            with self.subTest(version=value):
                self.version = value
                self.matrix["provider_version"] = value
                with self.assertRaises(ValueError):
                    self.check()

    def test_registry_manifest_requires_numeric_format_and_protocol6(self):
        for value in ({"version": True, "metadata": {"protocol_versions": ["6.0"]}},
                      {"version": 1, "metadata": {"protocol_versions": ["5.0"]}},
                      {"version": 1, "metadata": {"protocol_versions": [6]}}, []):
            with self.subTest(manifest=value):
                self.manifest = value
                with self.assertRaises(ValueError):
                    self.check()

    def test_schema_api_major_and_metadata_types_are_strict(self):
        original = copy.deepcopy(self.matrix)
        for field, value in (("schema_version", True), ("platform_api_major", True),
                             ("platform_api_major", 2), ("provider_version", None),
                             ("platform_release_versions", "0.1.0"), ("qualification", []),
                             ("aws_integration", []), ("unexpected", True)):
            with self.subTest(field=field):
                self.matrix = copy.deepcopy(original)
                self.matrix[field] = value
                with self.assertRaises(ValueError):
                    self.check()
        self.matrix = copy.deepcopy(original)
        del self.matrix["platform_api_major"]
        with self.assertRaises(ValueError):
            self.check()

    def test_qualification_types_cli_versions_and_targets_are_strict(self):
        original = copy.deepcopy(self.matrix["qualification"])
        for field, value in (("status", []), ("status", "passed"), ("scope", "live"),
                             ("terraform_cli_versions", []), ("terraform_cli_versions", ["1.14"]),
                             ("terraform_cli_versions", ["1.15.0-beta.1"]),
                             ("terraform_cli_versions", ["1.14.9", "1.14.9"]),
                             ("platforms", "linux_amd64"), ("platforms", ["linux_arm64"]),
                             ("platforms", ["linux_amd64", "linux_amd64"]), ("unexpected", True)):
            with self.subTest(field=field):
                self.matrix["qualification"] = copy.deepcopy(original)
                self.matrix["qualification"][field] = value
                with self.assertRaises(ValueError):
                    self.check()

    def test_product_release_versions_are_exact_semver_not_ranges(self):
        for value in ([">=0.2.0"], ["v0.2.0"], ["0.2"], [42], ["0.2.0", "0.2.0"], ["0.2.0-rc.01"]):
            with self.subTest(versions=value):
                self.matrix["platform_release_versions"] = value
                with self.assertRaises(ValueError):
                    self.check()

    def test_attestation_version_scope_cli_and_platform_must_match_matrix(self):
        self.matrix["qualification"]["status"] = "qualified"
        original = copy.deepcopy(self.attestation)
        for field, value in (("provider_version", "0.1.0-rc.2"), ("evidence_boundary", "authorized_aws_sandbox"),
                             ("tested_terraform_versions", ["1.14.0"]), ("platform", "linux_arm64"),
                             ("tested_platform_versions", ["0.2.0"])):
            with self.subTest(field=field):
                self.attestation = copy.deepcopy(original)
                self.attestation[field] = value
                with self.assertRaisesRegex(ValueError, "differ"):
                    self.check(release=True, attestation=True)

    def test_stable_attestation_requires_matching_product_release_versions(self):
        self.stable()
        for value in (None, [], ["0.3.0"], ["0.2.0", "0.2.0"]):
            with self.subTest(products=value):
                self.attestation["tested_platform_versions"] = value
                with self.assertRaises(ValueError):
                    self.check(release=True, attestation=True)

    def test_attested_version_lists_match_as_sets_without_ordering_requirement(self):
        self.stable()
        self.matrix["qualification"]["terraform_cli_versions"] = ["1.14.9", "1.15.0"]
        self.matrix["platform_release_versions"] = ["0.2.0", "0.2.1"]
        self.attestation["tested_terraform_versions"] = ["1.15.0", "1.14.9"]
        self.attestation["tested_platform_versions"] = ["0.2.1", "0.2.0"]
        self.assertEqual(self.check(release=True, attestation=True)["status"], "PASSED")

    def test_duplicate_and_nonstandard_json_fail_closed(self):
        for body in ('[]', '{"schema_version":1,"schema_version":1}', '{"a":NaN}', '{"a":'):
            with self.subTest(body=body):
                (self.root / "COMPATIBILITY.json").write_text(body)
                with self.assertRaises(ValueError):
                    checker.check(self.root)

    def test_script_is_portable_and_cli_rejects_pending_publication(self):
        portable = self.root / "scripts/check-compatibility.py"
        portable.parent.mkdir()
        shutil.copyfile(SCRIPT, portable)
        result = subprocess.run([sys.executable, str(portable)], cwd=self.root.parent,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["qualification_status"], "pending")
        result = subprocess.run([sys.executable, str(portable), "--release", "--expected-version", self.version,
                                 "--qualification", str(self.attestation_path)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "FAILED")


if __name__ == "__main__":
    unittest.main()
