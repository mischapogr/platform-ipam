"""Release packages refuse tampering and preserve deterministic installation bytes."""

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch
import zipfile


ROOT = Path(__file__).resolve().parents[2]
RELEASE = ROOT / "scripts/release"
sys.path.insert(0, str(RELEASE))
from provider_release import check_assets, check_signature, digest, names

spec = importlib.util.spec_from_file_location("package_provider", RELEASE / "package-provider.py")
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class ProviderAssetsTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.binary = self.root / "provider"
        header = bytearray(64)
        header[:6] = b"\x7fELF\x02\x01"
        header[16:18] = (2).to_bytes(2, "little")
        header[18:20] = (62).to_bytes(2, "little")
        self.binary.write_bytes(header + b"synthetic executable fixture")
        self.manifest = self.root / "manifest.json"
        self.manifest.write_text(json.dumps({"version": 1, "metadata": {"protocol_versions": ["6.0"]}}))
        self.version = "0.1.0-rc.1"
        self.destination = self.root / "dist"

    def package(self):
        return packager.package(self.binary, self.manifest, self.version, self.destination)

    def rewrite_checksums(self, assets):
        (self.destination / assets["checksums"]).write_text(
            "".join(f"{digest(self.destination / assets[name])}  {assets[name]}\n"
                    for name in ("archive", "manifest")))

    def test_unsigned_packages_are_deterministic_and_have_executable_binary(self):
        assets = self.package()
        other = self.root / "other"
        packager.package(self.binary, self.manifest, self.version, other)
        for name in ("archive", "manifest", "checksums"):
            self.assertEqual((self.destination / assets[name]).read_bytes(), (other / assets[name]).read_bytes())
        self.assertFalse((self.destination / assets["signature"]).exists())
        self.assertEqual(check_assets(self.destination, self.version), assets)
        with self.assertRaisesRegex(ValueError, "empty directory"):
            self.package()

    def test_modified_package_cannot_reuse_checksums(self):
        assets = self.package()
        with (self.destination / assets["archive"]).open("ab") as stream:
            stream.write(b"tampered bytes")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            check_assets(self.destination, self.version)

    def test_zip_cannot_contain_extra_files_even_with_correct_checksums(self):
        assets = self.package()
        with zipfile.ZipFile(self.destination / assets["archive"], "a") as archive:
            archive.writestr("../unexpected", b"payload")
        self.rewrite_checksums(assets)
        with self.assertRaisesRegex(ValueError, "only the correctly named"):
            check_assets(self.destination, self.version)

    def test_manifest_must_declare_framework_protocol(self):
        self.manifest.write_text('{"version": 1, "metadata": {"protocol_versions": ["5.0"]}}')
        with self.assertRaisesRegex(ValueError, "protocol 6.0"):
            self.package()
        self.assertFalse(self.destination.exists())
        self.manifest.write_text('{"version": true, "metadata": {"protocol_versions": ["6.0"]}}')
        with self.assertRaisesRegex(ValueError, "format 1"):
            self.package()

    def test_non_amd64_binary_is_refused_before_creating_destination(self):
        data = bytearray(self.binary.read_bytes())
        data[18:20] = (183).to_bytes(2, "little")
        self.binary.write_bytes(data)
        with self.assertRaisesRegex(ValueError, "linux_amd64"):
            self.package()
        self.assertFalse(self.destination.exists())

    def test_duplicate_or_missing_checksum_entries_are_refused(self):
        assets = self.package()
        checksum = self.destination / assets["checksums"]
        original = checksum.read_text()
        checksum.write_text(original + original.splitlines()[0] + "\n")
        with self.assertRaisesRegex(ValueError, "duplicate"):
            check_assets(self.destination, self.version)
        checksum.write_text(original.splitlines()[0] + "\n")
        with self.assertRaisesRegex(ValueError, "cover exactly"):
            check_assets(self.destination, self.version)

    def test_package_cli_and_asset_check_work_without_module_import_setup(self):
        subprocess.run([sys.executable, str(RELEASE / "package-provider.py"), "--binary", str(self.binary),
                        "--manifest", str(self.manifest), "--version", self.version, "--out", str(self.destination)],
                       check=True, capture_output=True)
        subprocess.run([sys.executable, str(RELEASE / "check-provider-assets.py"), "--directory",
                        str(self.destination), "--version", self.version], check=True, capture_output=True)

    def test_prerelease_numeric_identifiers_have_canonical_semver(self):
        for version in ("00.1.0", "0.1.0-rc.01", "v0.1.0", "0.1.0-rc..1"):
            with self.subTest(version=version), self.assertRaises(ValueError):
                names(version)

    def test_signature_verification_requires_registered_primary_key_and_rsa_or_dsa(self):
        assets = self.package()
        (self.destination / assets["signature"]).write_bytes(b"synthetic packet fixture")
        fingerprint = "A" * 40
        status = f"[GNUPG:] VALIDSIG {'B' * 40} 2026-10-10 1791590400 0 4 0 1 10 00 {fingerprint}\n"
        verified = subprocess.CompletedProcess([], 0, status, "")
        with patch("provider_release.subprocess.run", return_value=verified):
            check_signature(self.destination, self.version, fingerprint)
            with self.assertRaisesRegex(ValueError, "registered RSA or DSA"):
                check_signature(self.destination, self.version, "C" * 40)
        ecc = subprocess.CompletedProcess([], 0, status.replace(" 0 1 10 ", " 0 22 10 "), "")
        with patch("provider_release.subprocess.run", return_value=ecc):
            with self.assertRaisesRegex(ValueError, "registered RSA or DSA"):
                check_signature(self.destination, self.version, fingerprint)
        (self.destination / assets["signature"]).write_bytes(b"-----BEGIN PGP SIGNATURE-----")
        with self.assertRaisesRegex(ValueError, "binary, not ASCII armored"):
            check_signature(self.destination, self.version, fingerprint)


if __name__ == "__main__":
    unittest.main()
