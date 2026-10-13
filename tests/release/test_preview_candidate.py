"""Frozen preview integrity cannot promote tampered bytes or publication claims."""

import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/release/check-preview-candidate.py"
spec = importlib.util.spec_from_file_location("check_preview_candidate", SCRIPT)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class PreviewCandidateTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.container = Path(temporary.name)
        self.root = self.container / "bundle"
        self.root.mkdir()
        self.version = "0.1.0-rc.1"
        self.candidate_root = self.root / "dist/terraform-provider-platformipam" / self.version
        for name in ("source", "linked-licenses", "assets"):
            (self.candidate_root / name).mkdir(parents=True)
        self.manifest = {}
        for name, body in {"LICENSE": "project licence\n", "NOTICE": "project attribution\n",
                           "providers/terraform/main.go": "package main\n"}.items():
            original = self.root / name
            original.parent.mkdir(parents=True, exist_ok=True)
            original.write_text(body)
            self.manifest[name] = sha(original)
            mirror = name.removeprefix("providers/terraform/")
            (self.candidate_root / "source" / mirror).write_text(body)
        (self.candidate_root / "source/README.md").write_text("generated local preview provenance\n")
        license_file = self.candidate_root / "linked-licenses/fixture/LICENSE"
        license_file.parent.mkdir()
        license_file.write_text("synthetic linked dependency licence fixture\n")
        prefix = f"terraform-provider-platformipam_{self.version}"
        self.assets = {}
        for name, body in {f"{prefix}_linux_amd64.zip": b"synthetic package integrity fixture",
                           f"{prefix}_manifest.json": b'{"version":1}',
                           f"{prefix}_SHA256SUMS": b"synthetic checksum integrity fixture\n"}.items():
            asset = self.candidate_root / "assets" / name
            asset.write_bytes(body)
            self.assets[name] = sha(asset)
        self.evidence = self.root / ".work-plan-runs/preview-20261010/tests.log"
        self.evidence.parent.mkdir(parents=True)
        self.evidence.write_text("synthetic test evidence; no runtime acceptance claimed\n")
        common = {
            "evidence_boundary": "simulation", "source_status": "uncommitted_working_tree",
            "platform": "linux_amd64", "provider_address": "registry.example.com/platform/platformipam",
            "registry_publication": "NOT_PUBLISHED", "signing": "UNSIGNED", "live_aws": "NOT_READY",
            "aws_readiness_notice": "AWS API/AWS integration is not yet ready", "base_commit": "a" * 40,
        }
        self.candidate = {
            **common, "version": self.version, "qualification_status": "QUALIFIED_FOR_LOCAL_DEVELOPMENT_SIMULATION",
            "source_files_sha256": {p.name: sha(p) for p in (self.candidate_root / "source").iterdir()},
            "linked_licenses_sha256": {"fixture/LICENSE": sha(license_file)},
            "assets_sha256": self.assets.copy(), "tested_terraform_versions": ["1.14.9"],
            "report": "../../../docs/PREVIEW_CANDIDATE.md",
        }
        self.record = {
            **common, "schema_version": 1, "candidate": self.version,
            "qualification": "QUALIFIED_FOR_LOCAL_DEVELOPMENT_SIMULATION", "immutable_source_commit": None,
            "maintainer_release_attestation": False,
            "source_review": {"maintainer_publication_approval": False},
            "assets_sha256": self.assets.copy(),
            "evidence": {"fixture.log": {"path": self.evidence.relative_to(self.root).as_posix(),
                                         "sha256": sha(self.evidence)}},
            "checks": {"synthetic_fixture": {"status": "PASSED"}},
        }
        self.manifest_path = self.root / "docs/qualification/PREVIEW_2026_10_10_SOURCE.json"
        self.manifest_path.parent.mkdir(parents=True)
        self.candidate_path = self.candidate_root / "CANDIDATE.json"
        self.write_manifest()
        self.write_candidate()
        self.write_record()

    def write_record(self):
        (self.root / checker.DEFAULT_RECORD).write_text(json.dumps(self.record))

    def write_manifest(self):
        self.manifest_path.write_text(json.dumps(self.manifest))
        self.record["source_manifest"] = {"path": self.manifest_path.relative_to(self.root).as_posix(),
                                          "sha256": sha(self.manifest_path)}

    def write_candidate(self):
        self.candidate_path.write_text(json.dumps(self.candidate))
        self.record["candidate_metadata"] = {"path": self.candidate_path.relative_to(self.root).as_posix(),
                                             "sha256": sha(self.candidate_path)}

    def verify(self):
        self.write_record()
        return checker.verify(self.root)

    def test_valid_tiny_fixture_is_integrity_only_and_retains_boundaries(self):
        result = self.verify()
        self.assertEqual(result["status"], "PASSED")
        self.assertEqual(result["verified"], {"manifest_source_files": 3, "candidate_source_files": 4,
                                              "linked_license_files": 1, "assets": 3, "evidence_files": 1})
        self.assertIs(result["publication_acceptance"], False)
        self.assertIs(result["maintainer_release_attestation"], False)
        self.assertEqual(result["signing"], "UNSIGNED")
        self.assertEqual(result["live_aws"], "NOT_READY")
        # Legacy report prose is unbound information and is never opened by the checker.
        self.assertFalse((self.root / "docs/PREVIEW_CANDIDATE.md").exists())

    def test_tampered_manifest_source_is_rejected(self):
        (self.root / "providers/terraform/main.go").write_text("altered source\n")
        with self.assertRaisesRegex(ValueError, "manifest source file SHA-256 mismatch"):
            self.verify()

    def test_tampered_evidence_is_rejected(self):
        self.evidence.write_text("altered evidence\n")
        with self.assertRaisesRegex(ValueError, "evidence binding SHA-256 mismatch"):
            self.verify()

    def test_tampered_asset_is_rejected(self):
        (self.candidate_root / "assets" / next(iter(self.assets))).write_text("altered package\n")
        with self.assertRaisesRegex(ValueError, "preview asset SHA-256 mismatch"):
            self.verify()

    def test_tampered_linked_license_is_rejected(self):
        (self.candidate_root / "linked-licenses/fixture/LICENSE").write_text("altered licence\n")
        with self.assertRaisesRegex(ValueError, "linked license file SHA-256 mismatch"):
            self.verify()

    def test_tampered_candidate_source_is_rejected(self):
        (self.candidate_root / "source/main.go").write_text("altered candidate source\n")
        with self.assertRaisesRegex(ValueError, "candidate source file SHA-256 mismatch"):
            self.verify()

    def test_rebound_candidate_source_must_still_match_root_manifest(self):
        path = self.candidate_root / "source/main.go"
        path.write_text("new separately hashed candidate source\n")
        self.candidate["source_files_sha256"]["main.go"] = sha(path)
        self.write_candidate()
        with self.assertRaisesRegex(ValueError, "not bound to the qualified source manifest"):
            self.verify()

    def test_manifest_and_candidate_metadata_bindings_are_enforced(self):
        for path, label in ((self.manifest_path, "source manifest binding"),
                            (self.candidate_path, "candidate metadata binding")):
            with self.subTest(label=label):
                original = path.read_bytes()
                path.write_bytes(original + b" ")
                with self.assertRaisesRegex(ValueError, label + " SHA-256 mismatch"):
                    self.verify()
                path.write_bytes(original)

    def test_rebound_candidate_metadata_must_match_version_platform_and_boundary(self):
        original = copy.deepcopy(self.candidate)
        for field, value in {"version": "0.2.0-rc.1", "base_commit": "b" * 40,
                             "platform": "linux_arm64", "signing": "SIGNED", "live_aws": "READY",
                             "evidence_boundary": "live", "source_status": "committed",
                             "registry_publication": "PUBLISHED", "qualification_status": "PRODUCTION_READY",
                             "provider_address": "registry.terraform.io/fixture/platformipam",
                             "maintainer_release_attestation": True, "immutable_source_commit": "a" * 40,
                             "publication_acceptance": True, "aws_mode": "live"}.items():
            with self.subTest(field=field):
                self.candidate = copy.deepcopy(original)
                self.candidate[field] = value
                self.write_candidate()
                with self.assertRaises(ValueError):
                    self.verify()
        self.candidate = original

    def test_record_cannot_promote_simulation_to_publication_or_stable_acceptance(self):
        original = copy.deepcopy(self.record)
        for field, value in {"candidate": "0.1.0", "schema_version": True,
                             "qualification": "QUALIFIED_FOR_PRODUCTION", "signing": "SIGNED",
                             "evidence_boundary": "live", "source_status": "committed",
                             "platform": "linux_arm64", "registry_publication": "PUBLISHED",
                             "live_aws": "READY", "immutable_source_commit": "a" * 40,
                             "maintainer_release_attestation": True, "base_commit": "main",
                             "source_review": {"maintainer_publication_approval": True}}.items():
            with self.subTest(field=field):
                self.record = copy.deepcopy(original)
                self.record[field] = value
                with self.assertRaises(ValueError):
                    self.verify()
        self.record = original

    def test_unsupported_and_malformed_json_reports_fail_closed(self):
        record_path = self.root / checker.DEFAULT_RECORD
        for body in ('[]', 'null', '{}', '{"a":', '{"a":1,"a":2}', '{"a":NaN}'):
            with self.subTest(body=body):
                record_path.write_text(body)
                with self.assertRaises(ValueError):
                    checker.verify(self.root)

    def test_malformed_references_digests_and_collections_are_rejected(self):
        original = copy.deepcopy(self.record)
        for field, value in {"source_manifest": [], "candidate_metadata": {"path": 7, "sha256": "a" * 64},
                             "evidence": {"fixture": {"path": "file", "sha256": "not-a-digest"}},
                             "assets_sha256": [], "checks": {"fixture": {"status": []}}}.items():
            with self.subTest(field=field):
                self.record = copy.deepcopy(original)
                self.record[field] = value
                with self.assertRaises(ValueError):
                    self.verify()
        self.record = original
        for field, value in {"source_files_sha256": {}, "linked_licenses_sha256": [],
                             "tested_terraform_versions": ["1.14"]}.items():
            with self.subTest(candidate_field=field):
                original_candidate = copy.deepcopy(self.candidate)
                self.candidate[field] = value
                self.write_candidate()
                with self.assertRaises(ValueError):
                    self.verify()
                self.candidate = original_candidate

    def test_record_and_evidence_paths_reject_traversal_absolute_and_noncanonical_forms(self):
        for value in ("../outside.json", "/tmp/outside.json", "C:/outside.json", "C:\\outside.json",
                      "docs//report.json", "docs/./report.json", "", "docs\0report.json"):
            with self.subTest(path=value):
                with self.assertRaises(ValueError):
                    checker.verify(self.root, value)
                self.record["evidence"]["fixture.log"]["path"] = value
                with self.assertRaises(ValueError):
                    self.verify()

    def test_hash_map_paths_cannot_escape_source_or_license_bases(self):
        self.manifest["../outside"] = "a" * 64
        self.write_manifest()
        with self.assertRaisesRegex(ValueError, "relative paths"):
            self.verify()
        del self.manifest["../outside"]
        self.write_manifest()
        for field in ("source_files_sha256", "linked_licenses_sha256"):
            with self.subTest(field=field):
                previous = copy.deepcopy(self.candidate[field])
                self.candidate[field]["../outside"] = "a" * 64
                self.write_candidate()
                with self.assertRaisesRegex(ValueError, "relative paths"):
                    self.verify()
                self.candidate[field] = previous

    def test_symlink_source_escape_is_rejected_even_for_identical_bytes(self):
        source = self.root / "providers/terraform/main.go"
        outside = self.container / "outside.go"
        outside.write_bytes(source.read_bytes())
        source.unlink()
        source.symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "symlinks"):
            self.verify()

    def test_symlink_candidate_directory_escape_is_rejected(self):
        original = self.candidate_root / "linked-licenses"
        outside = self.container / "licenses"
        original.rename(outside)
        original.symlink_to(outside, target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "symlinks"):
            self.verify()

    def test_symlink_metadata_reference_is_rejected(self):
        outside = self.container / "candidate.json"
        outside.write_bytes(self.candidate_path.read_bytes())
        self.candidate_path.unlink()
        self.candidate_path.symlink_to(outside)
        with self.assertRaisesRegex(ValueError, "symlinks"):
            self.verify()

    def test_references_require_existing_regular_files(self):
        self.evidence.unlink()
        with self.assertRaises(FileNotFoundError):
            self.verify()
        self.evidence.mkdir()
        with self.assertRaisesRegex(ValueError, "wrong file type"):
            self.verify()
        self.evidence.rmdir()
        os.mkfifo(self.evidence)
        with self.assertRaisesRegex(ValueError, "wrong file type"):
            self.verify()

    def test_candidate_and_record_asset_maps_must_agree_and_use_versioned_target_names(self):
        self.candidate["assets_sha256"][next(iter(self.assets))] = "b" * 64
        self.write_candidate()
        with self.assertRaisesRegex(ValueError, "digests contradict"):
            self.verify()
        self.candidate["assets_sha256"] = {"../outside.zip": "b" * 64}
        self.record["assets_sha256"] = copy.deepcopy(self.candidate["assets_sha256"])
        self.write_candidate()
        with self.assertRaisesRegex(ValueError, "match candidate version"):
            self.verify()

    def test_cli_reports_json_and_exit_code_for_valid_and_tampered_bundle(self):
        result = subprocess.run([sys.executable, str(SCRIPT), "--root", str(self.root)],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "PASSED")
        self.evidence.write_text("altered evidence\n")
        result = subprocess.run([sys.executable, str(SCRIPT), "--root", str(self.root)],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual(json.loads(result.stdout)["status"], "FAILED")
        self.assertIs(json.loads(result.stdout)["publication_acceptance"], False)
        self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()
