"""Mirror options reject unsafe inputs and retain failed installer evidence."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import zipfile


SCRIPT = Path(__file__).resolve().parents[2] / "tests/e2e/run-provider-mirror.sh"


class ProviderMirrorOptionsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.calls = self.root / "docker-calls.jsonl"
        docker = self.bin / "docker"
        docker.write_text(
            "#!/usr/bin/env python3\n"
            "import json,os,sys\n"
            "from pathlib import Path\n"
            "with open(os.environ['MIRROR_TEST_CALLS'],'a') as stream:\n"
            " stream.write(json.dumps(sys.argv[1:])+'\\n')\n"
            "if '--entrypoint' in sys.argv:\n"
            " entry=sys.argv[sys.argv.index('--entrypoint')+1]\n"
            " if entry=='terraform' and sys.argv[-2:]==['version','-json']:\n"
            "  print(json.dumps({'terraform_version':'1.14.9'}));sys.exit(0)\n"
            " if entry=='terraform':\n"
            "  print('synthetic installer failure',file=sys.stderr);sys.exit(9)\n"
            " if entry=='rm': sys.exit(0)\n"
            "sys.exit('unexpected Docker invocation in unit fixture')\n"
        )
        docker.chmod(0o755)
        self.environment = dict(os.environ)
        for name in ("IPAM_PROVIDER_VERSION", "IPAM_PROVIDER_PACKAGE", "IPAM_PROVIDER_SOURCE",
                     "IPAM_TERRAFORM_IMAGE", "IPAM_PROVIDER_MIRROR_EVIDENCE"):
            self.environment.pop(name, None)
        self.environment.update(PATH=str(self.bin) + os.pathsep + self.environment["PATH"],
                                MIRROR_TEST_CALLS=str(self.calls))

    def run_script(self, **options):
        return subprocess.run(["sh", str(SCRIPT)], env=dict(self.environment, **options),
                              text=True, capture_output=True, timeout=15)

    def test_unsafe_provider_source_is_rejected_before_installer_runs(self):
        for source in ("registry.terraform.io/../platformipam", "/registry.terraform.io/owner/platformipam",
                       "registry.terraform.io/owner/platformipam/extra", "registry.terraform.io//platformipam",
                       'registry.terraform.io/owner/platformipam"', "registry..terraform.io/owner/platformipam"):
            with self.subTest(source=source):
                result = self.run_script(IPAM_PROVIDER_SOURCE=source)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("safe hostname/namespace/type", result.stderr)
                self.assertFalse(self.calls.exists())

    def test_image_options_cannot_inject_docker_flags(self):
        for image in ("--privileged", "hashicorp/terraform:1.14.9 --privileged", "bad\nimage"):
            with self.subTest(image=image):
                result = self.run_script(IPAM_TERRAFORM_IMAGE=image)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("safe container image", result.stderr)
                self.assertFalse(self.calls.exists())

    def test_evidence_directory_does_not_overwrite_existing_or_follow_symlinks(self):
        existing = self.root / "existing"
        existing.mkdir()
        original = existing / "original.log"
        original.write_text("keep this evidence\n")
        result = self.run_script(IPAM_PROVIDER_MIRROR_EVIDENCE=str(existing))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("absent or empty", result.stderr)
        self.assertEqual(original.read_text(), "keep this evidence\n")
        self.assertFalse(self.calls.exists())
        link = self.root / "link"
        link.symlink_to(existing, target_is_directory=True)
        result = self.run_script(IPAM_PROVIDER_MIRROR_EVIDENCE=str(link / "child"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlinks", result.stderr)
        self.assertFalse((existing / "child").exists())

    def test_public_source_package_hash_tool_version_and_failure_log_are_retained(self):
        version = "0.1.0-rc.1"
        package = self.root / "provider.zip"
        with zipfile.ZipFile(package, "w") as archive:
            header = bytearray(64)
            header[:6] = b"\x7fELF\x02\x01"
            header[16:18] = (2).to_bytes(2, "little")
            header[18:20] = (62).to_bytes(2, "little")
            entry = zipfile.ZipInfo(f"terraform-provider-platformipam_v{version}")
            entry.create_system = 3
            entry.external_attr = 0o100755 << 16
            archive.writestr(entry, header + b"synthetic executable fixture")
        evidence = self.root / "evidence"
        source = "registry.terraform.io/mischapogr/platformipam"
        result = self.run_script(IPAM_PROVIDER_VERSION=version, IPAM_PROVIDER_PACKAGE=str(package),
                                 IPAM_PROVIDER_SOURCE=source, IPAM_PROVIDER_MIRROR_EVIDENCE=str(evidence),
                                 IPAM_TERRAFORM_IMAGE="hashicorp/terraform:1.14.9")
        self.assertNotEqual(result.returncode, 0)  # The installer double intentionally fails.
        metadata = json.loads((evidence / "package-metadata.json").read_text())
        self.assertEqual(metadata["package_sha256"], hashlib.sha256(package.read_bytes()).hexdigest())
        self.assertEqual(metadata["provider_source"], source)
        self.assertEqual(metadata["provider_version"], version)
        self.assertEqual(metadata["terraform_version"], "1.14.9")
        self.assertIs(metadata["published_origin_acceptance"], False)
        self.assertIn(metadata["package_sha256"], result.stdout)
        self.assertIn("1.14.9", result.stdout)
        self.assertIn("synthetic installer failure", (evidence / "init.log").read_text())
        self.assertTrue((evidence / "terraform-version.json").is_file())
        for line in self.calls.read_text().splitlines():
            self.assertIn("hashicorp/terraform:1.14.9", json.loads(line))


if __name__ == "__main__":
    unittest.main()
