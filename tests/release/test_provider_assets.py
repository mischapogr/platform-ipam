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
from provider_release import check_archive, check_assets, check_signature, digest, names

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
        with self.assertRaisesRegex(ValueError, "unsafe notice path"):
            check_assets(self.destination, self.version)

    def notices(self):
        packet = self.root / "notices"
        packet.mkdir()
        (packet / "module").mkdir()
        (packet / "module/LICENSE").write_bytes(b"upstream licence bytes\n")
        (packet / "README.md").write_bytes(b"Review remains pending.\n")
        (packet / "GO_1_26_9_LICENSE").write_bytes(b"Go licence text\n")
        (packet / "BUILD_INFO.txt").write_text("/artifacts/provider: go1.26.9\n\tdep\texample.com/module\tv1.0.0\th1:fixture\n")
        inventory = {"missing_root_license_modules": [], "modules": [{
            "module": "example.com/module", "version": "v1.0.0",
            "copied_root_license_notice_files": [{"file": "module/LICENSE", "sha256": digest(packet / "module/LICENSE")}],
        }]}
        (packet / "INVENTORY.json").write_text(json.dumps(inventory))
        record = {"provider_version": self.version, "binary_sha256": digest(self.binary),
                  "linked_module_count": 1, "build_go_version": "go1.26.9",
                  "files_sha256": {path.relative_to(packet).as_posix(): digest(path)
                                   for path in packet.rglob("*") if path.is_file()}}
        (packet / "PUBLISHED_BINARY.json").write_text(json.dumps(record))
        licence = self.root / "LICENSE"
        licence.write_bytes(b"project licence\n")
        notice = self.root / "NOTICE"
        notice.write_bytes(b"project notice\n")
        return packet, licence, notice

    def rewrite_zip(self, assets, mutate):
        path = self.destination / assets["archive"]
        with zipfile.ZipFile(path) as archive:
            entries = [(entry, archive.read(entry)) for entry in archive.infolist()]
        with zipfile.ZipFile(path, "w") as archive:
            for entry, data in mutate(entries):
                archive.writestr(entry, data)
        self.rewrite_checksums(assets)

    def test_notice_package_is_deterministic_and_preserves_all_input_bytes(self):
        packet, licence, notice = self.notices()
        assets = packager.package(self.binary, self.manifest, self.version, self.destination, packet, licence, notice)
        other = self.root / "other"
        packager.package(self.binary, self.manifest, self.version, other, packet, licence, notice)
        self.assertEqual((self.destination / assets["archive"]).read_bytes(), (other / assets["archive"]).read_bytes())
        self.assertEqual(check_archive(self.destination / assets["archive"], self.version), self.binary.read_bytes())
        with zipfile.ZipFile(self.destination / assets["archive"]) as archive:
            for path in packet.rglob("*"):
                if path.is_file():
                    self.assertEqual(archive.read("THIRD_PARTY_NOTICES/" + path.relative_to(packet).as_posix()), path.read_bytes())
            self.assertEqual(archive.read("LICENSE"), licence.read_bytes())
            for entry in archive.infolist():
                self.assertEqual(entry.date_time, (1980, 1, 1, 0, 0, 0))
                self.assertEqual(entry.external_attr >> 16, 0o100755 if entry.filename == assets["binary"] else 0o100644)

    def test_notice_package_cli_accepts_complete_packet_options(self):
        packet, licence, notice = self.notices()
        subprocess.run([sys.executable, str(RELEASE / "package-provider.py"), "--binary", str(self.binary),
                        "--manifest", str(self.manifest), "--version", self.version, "--out", str(self.destination),
                        "--notices", str(packet), "--project-license", str(licence), "--project-notice", str(notice)],
                       check=True, capture_output=True)
        check_assets(self.destination, self.version)

    def test_notices_require_all_three_inputs(self):
        packet, _, _ = self.notices()
        with self.assertRaisesRegex(ValueError, "required together"):
            packager.package(self.binary, self.manifest, self.version, self.destination, packet)
        self.assertFalse(self.destination.exists())

    def test_stale_altered_missing_unlisted_and_symlink_notice_inputs_are_refused(self):
        packet, licence, notice = self.notices()
        record_path = packet / "PUBLISHED_BINARY.json"
        original_record = record_path.read_bytes()
        original_licence = (packet / "module/LICENSE").read_bytes()
        for kind in ("binary", "version", "altered", "missing", "unlisted", "symlink"):
            with self.subTest(kind=kind):
                record = json.loads(original_record)
                if kind in ("binary", "version"):
                    record["binary_sha256" if kind == "binary" else "provider_version"] = "stale"
                    record_path.write_text(json.dumps(record))
                elif kind == "altered":
                    (packet / "module/LICENSE").write_bytes(b"changed")
                elif kind == "missing":
                    (packet / "module/LICENSE").unlink()
                elif kind == "unlisted":
                    (packet / "extra").write_bytes(b"extra")
                else:
                    (packet / "module/LICENSE").unlink()
                    (packet / "module/LICENSE").symlink_to(licence)
                with self.assertRaises(ValueError):
                    packager.package(self.binary, self.manifest, self.version, self.destination, packet, licence, notice)
                self.assertFalse(self.destination.exists())
                record_path.write_bytes(original_record)
                (packet / "module/LICENSE").unlink(missing_ok=True)
                (packet / "module/LICENSE").write_bytes(original_licence)
                (packet / "extra").unlink(missing_ok=True)

    def test_notice_packet_rejects_incomplete_or_stale_inventory_even_with_updated_hashes(self):
        packet, licence, notice = self.notices()
        originals = {path.relative_to(packet).as_posix(): path.read_bytes()
                     for path in packet.rglob("*") if path.is_file()}
        for kind in ("missing-inventory", "missing-build", "empty-modules", "duplicate-modules",
                     "stale-module", "missing-licences", "missing-go-licence", "wrong-go", "wrong-count",
                     "wrong-licence-hash", "unresolved-licence"):
            with self.subTest(kind=kind):
                inventory = json.loads(originals["INVENTORY.json"])
                record = json.loads(originals["PUBLISHED_BINARY.json"])
                if kind == "missing-inventory": (packet / "INVENTORY.json").unlink()
                elif kind == "missing-build": (packet / "BUILD_INFO.txt").unlink()
                elif kind == "empty-modules": inventory["modules"] = []
                elif kind == "duplicate-modules":
                    inventory["modules"] *= 2
                    record["linked_module_count"] = 2
                elif kind == "stale-module": inventory["modules"][0]["version"] = "v2.0.0"
                elif kind == "missing-licences": inventory["modules"][0]["copied_root_license_notice_files"] = []
                elif kind == "missing-go-licence": (packet / "GO_1_26_9_LICENSE").unlink()
                elif kind == "wrong-go": record["build_go_version"] = "go1.26.8"
                elif kind == "wrong-count": record["linked_module_count"] = 2
                elif kind == "wrong-licence-hash":
                    inventory["modules"][0]["copied_root_license_notice_files"][0]["sha256"] = "0" * 64
                else: inventory["missing_root_license_modules"] = ["example.com/module"]
                if kind != "missing-inventory": (packet / "INVENTORY.json").write_text(json.dumps(inventory))
                record["files_sha256"] = {path.relative_to(packet).as_posix(): digest(path)
                                          for path in packet.rglob("*")
                                          if path.is_file() and path.name != "PUBLISHED_BINARY.json"}
                (packet / "PUBLISHED_BINARY.json").write_text(json.dumps(record))
                with self.assertRaises(ValueError):
                    packager.package(self.binary, self.manifest, self.version, self.destination, packet, licence, notice)
                self.assertFalse(self.destination.exists())
                for name, data in originals.items(): (packet / name).write_bytes(data)

    def test_notice_inputs_refuse_symlink_ancestors(self):
        packet, licence, notice = self.notices()
        alias = self.root / "alias"
        alias.symlink_to(self.root, target_is_directory=True)
        for inputs in ((alias / packet.name, licence, notice),
                       (packet, alias / licence.name, notice),
                       (packet, licence, alias / notice.name)):
            with self.subTest(inputs=inputs), self.assertRaises(ValueError):
                packager.package(self.binary, self.manifest, self.version, self.destination, *inputs)
            self.assertFalse(self.destination.exists())

    def test_notice_archive_rejects_tampering_even_after_outer_checksums_are_recomputed(self):
        packet, licence, notice = self.notices()
        assets = packager.package(self.binary, self.manifest, self.version, self.destination, packet, licence, notice)
        original = (self.destination / assets["archive"]).read_bytes()
        for kind in ("changed", "missing", "duplicate", "executable", "symlink", "setuid", "binding", "unlisted"):
            with self.subTest(kind=kind):
                def mutate(entries):
                    output = []
                    for entry, data in entries:
                        if entry.filename == "LICENSE":
                            if kind == "changed": data = b"changed"
                            if kind == "missing": continue
                            if kind == "executable": entry.external_attr = 0o100755 << 16
                            if kind == "symlink": entry.external_attr = 0o120644 << 16
                            if kind == "setuid": entry.external_attr = 0o104644 << 16
                        if entry.filename == "THIRD_PARTY_NOTICES/CONTENTS.json" and kind == "binding":
                            contents = json.loads(data)
                            contents["binary_sha256"] = "0" * 64
                            data = json.dumps(contents).encode()
                        output.append((entry, data))
                    if kind == "duplicate": output.append(output[0])
                    if kind == "unlisted":
                        extra = zipfile.ZipInfo("THIRD_PARTY_NOTICES/extra")
                        extra.external_attr = 0o100644 << 16
                        output.append((extra, b"extra"))
                    return output
                self.rewrite_zip(assets, mutate)
                with self.assertRaises(ValueError): check_assets(self.destination, self.version)
                (self.destination / assets["archive"]).write_bytes(original)
                self.rewrite_checksums(assets)

    def test_archive_rejects_absolute_backslash_and_traversal_paths(self):
        assets = self.package()
        original = (self.destination / assets["archive"]).read_bytes()
        for path in ("/LICENSE", "THIRD_PARTY_NOTICES/../LICENSE", "THIRD_PARTY_NOTICES\\LICENSE", "THIRD_PARTY_NOTICES//LICENSE"):
            with self.subTest(path=path):
                extra = zipfile.ZipInfo(path)
                extra.external_attr = 0o100644 << 16
                self.rewrite_zip(assets, lambda entries: entries + [(extra, b"extra")])
                with self.assertRaisesRegex(ValueError, "unsafe notice path"):
                    check_assets(self.destination, self.version)
                (self.destination / assets["archive"]).write_bytes(original)
                self.rewrite_checksums(assets)

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
