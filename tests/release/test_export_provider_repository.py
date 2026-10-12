"""The provider mirror must be byte-for-byte sourced from one reviewed commit."""

import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/release/export-provider-repository.py"
spec = importlib.util.spec_from_file_location("export_provider_repository", SCRIPT)
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)
CHECK_SCRIPT = ROOT / "scripts/release/check-provider-mirror.py"
check_spec = importlib.util.spec_from_file_location("check_provider_mirror", CHECK_SCRIPT)
checker = importlib.util.module_from_spec(check_spec)
check_spec.loader.exec_module(checker)


class ProviderExportTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repository = self.root / "source"
        self.repository.mkdir()
        subprocess.run(["git", "init", "-q", str(self.repository)], check=True)
        subprocess.run(["git", "checkout", "-q", "-b", "main"], cwd=self.repository, check=True)
        module = self.repository / "providers/terraform"
        module.mkdir(parents=True)
        for path, body in {
            "LICENSE": "Test licence\n",
            "NOTICE": "Test attribution\n",
            "providers/terraform/go.mod": "module example.invalid/provider\n",
            "providers/terraform/main.go": "package main\n",
            "providers/terraform/terraform-registry-manifest.json": "{}\n",
            "providers/terraform/docs/index.md": "# Provider\n",
            "providers/terraform/docs/resources/allocation.md": "# Allocation resource\n",
            "providers/terraform/docs/data-sources/allocation.md": "# Allocation data source\n",
            "providers/terraform/docs/data-sources/pool.md": "# Pool data source\n",
            "providers/terraform/scripts/check-replacement-plan.py": "#!/usr/bin/env python3\n# test guard fixture\n",
        }.items():
            (self.repository / path).parent.mkdir(parents=True, exist_ok=True)
            (self.repository / path).write_text(body, encoding="utf-8")
        subprocess.run(["git", "add", "LICENSE", "NOTICE", "providers/terraform"], cwd=self.repository, check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "-q", "-m", "fixture"], cwd=self.repository, check=True)
        self.source_ref = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repository,
                                                  text=True).strip()
        original_root = exporter.ROOT
        exporter.ROOT = self.repository
        self.addCleanup(setattr, exporter, "ROOT", original_root)

    def test_export_tracks_exact_committed_provider_and_refuses_nonempty_destination(self):
        destination = self.root / "provider"
        source_ref = self.source_ref
        count = exporter.export(source_ref, destination)
        self.assertGreater(count, 0)
        self.assertEqual((destination / "SOURCE_COMMIT").read_text(), source_ref + "\n")
        for path in ("go.mod", "main.go", "terraform-registry-manifest.json", "scripts/check-replacement-plan.py"):
            expected = subprocess.check_output(
                ["git", "show", f"{source_ref}:providers/terraform/{path}"], cwd=self.repository)
            self.assertEqual((destination / path).read_bytes(), expected)
        self.assertEqual((destination / "LICENSE").read_bytes(),
                         subprocess.check_output(["git", "show", f"{source_ref}:LICENSE"], cwd=self.repository))
        self.assertEqual((destination / "NOTICE").read_bytes(),
                         subprocess.check_output(["git", "show", f"{source_ref}:NOTICE"], cwd=self.repository))
        self.assertEqual(checker.check(destination, self.repository / "providers/terraform", source_ref),
                         count - 2)
        (destination / "main.go").write_text("// altered mirror\n")
        with self.assertRaisesRegex(ValueError, "differs from reviewed source: main.go"):
            checker.check(destination, self.repository / "providers/terraform", source_ref)
        (destination / "main.go").unlink()
        (destination / "main.go").symlink_to("go.mod")
        with self.assertRaisesRegex(ValueError, "contains a symlink"):
            checker.check(destination, self.repository / "providers/terraform", source_ref)
        with self.assertRaisesRegex(ValueError, "empty directory"):
            exporter.export(source_ref, destination)

    def test_invalid_ref_does_not_create_destination(self):
        destination = self.root / "provider"
        with self.assertRaisesRegex(ValueError, "full lowercase commit SHA"):
            exporter.export("main", destination)
        self.assertFalse(destination.exists())

    def test_verification_uses_committed_bytes_even_when_source_worktree_changes(self):
        destination = self.root / "provider"
        count = exporter.export(self.source_ref, destination)
        (self.repository / "providers/terraform/main.go").write_text("// dirty source\n")
        (self.repository / "LICENSE").write_text("dirty licence\n")
        (self.repository / "NOTICE").write_text("dirty attribution\n")
        (self.repository / "providers/terraform/untracked.go").write_text("// untracked\n")
        self.assertEqual(checker.check(destination, self.repository / "providers/terraform", self.source_ref),
                         count - 2)
        (destination / "main.go").write_text("// dirty source\n")
        with self.assertRaisesRegex(ValueError, "differs from reviewed source: main.go"):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)

    def test_source_documentation_is_exported_and_verified(self):
        destination = self.root / "provider"
        exporter.export(self.source_ref, destination)
        (destination / "docs/index.md").write_text("# Altered provider docs\n")
        with self.assertRaisesRegex(ValueError, "differs from reviewed source: docs/index.md"):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)

    def test_release_metadata_allowed_but_extra_source_rejected(self):
        destination = self.root / "provider"
        count = exporter.export(self.source_ref, destination)
        (destination / "RELEASE_QUALIFICATION.json").write_text("{}\n")
        (destination / "release-evidence").mkdir()
        (destination / "release-evidence/tests.log").write_text("test evidence\n")
        self.assertEqual(checker.check(destination, self.repository / "providers/terraform", self.source_ref),
                         count - 2)
        (destination / "injected.go").write_text("package main\n")
        with self.assertRaisesRegex(ValueError, "file set differs"):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)

    def test_export_refuses_source_without_registry_documentation(self):
        subprocess.run(["git", "rm", "-q", "providers/terraform/docs/index.md"],
                       cwd=self.repository, check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "-q", "-m", "remove docs"], cwd=self.repository, check=True)
        ref = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repository, text=True).strip()
        destination = self.root / "provider"
        with self.assertRaisesRegex(ValueError, "missing required provider files"):
            exporter.export(ref, destination)
        self.assertFalse(destination.exists())

    def test_export_refuses_source_without_portable_replacement_guard(self):
        subprocess.run(["git", "rm", "-q", "providers/terraform/scripts/check-replacement-plan.py"],
                       cwd=self.repository, check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "-q", "-m", "remove consumer guard"], cwd=self.repository, check=True)
        ref = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repository, text=True).strip()
        destination = self.root / "provider"
        with self.assertRaisesRegex(ValueError, "missing required provider files"):
            exporter.export(ref, destination)
        self.assertFalse(destination.exists())

    def test_portable_replacement_guard_cannot_be_changed_in_mirror(self):
        destination = self.root / "provider"
        exporter.export(self.source_ref, destination)
        (destination / "scripts/check-replacement-plan.py").write_text("# guard disabled\n")
        with self.assertRaisesRegex(ValueError, "differs from reviewed source: scripts/check-replacement-plan.py"):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)

    def test_export_refuses_source_without_notice(self):
        subprocess.run(["git", "rm", "-q", "NOTICE"], cwd=self.repository, check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "-q", "-m", "remove attribution"], cwd=self.repository, check=True)
        ref = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repository, text=True).strip()
        destination = self.root / "provider"
        with self.assertRaisesRegex(ValueError, "missing required provider files"):
            exporter.export(ref, destination)
        self.assertFalse(destination.exists())

    def test_missing_or_changed_notice_cannot_verify(self):
        destination = self.root / "provider"
        exporter.export(self.source_ref, destination)
        (destination / "NOTICE").write_text("altered attribution\n")
        with self.assertRaisesRegex(ValueError, "NOTICE differs from reviewed source"):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)
        (destination / "NOTICE").unlink()
        with self.assertRaises(FileNotFoundError):
            checker.check(destination, self.repository / "providers/terraform", self.source_ref)

    def test_provider_module_cannot_override_root_notice(self):
        (self.repository / "providers/terraform/NOTICE").write_text("module override\n")
        subprocess.run(["git", "add", "providers/terraform/NOTICE"], cwd=self.repository, check=True)
        subprocess.run(["git", "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "-q", "-m", "add conflicting attribution"], cwd=self.repository, check=True)
        ref = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=self.repository, text=True).strip()
        destination = self.root / "provider"
        with self.assertRaisesRegex(ValueError, "conflicts with generated release metadata: providers/terraform/NOTICE"):
            exporter.export(ref, destination)
        self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
