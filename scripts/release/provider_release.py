"""Registry asset validation shared by the unsigned packager and release check."""

import hashlib
import json
from pathlib import Path
import re
import subprocess
import zipfile


VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?")


def names(version):
    if not VERSION.fullmatch(version):
        raise ValueError("version must be a canonical semantic version without v")
    if "-" in version and any(part.isdigit() and len(part) > 1 and part.startswith("0")
                              for part in version.partition("-")[2].split(".")):
        raise ValueError("numeric prerelease identifiers must not contain leading zeros")
    prefix = f"terraform-provider-platformipam_{version}"
    return {
        "binary": f"terraform-provider-platformipam_v{version}",
        "archive": f"{prefix}_linux_amd64.zip",
        "manifest": f"{prefix}_manifest.json",
        "checksums": f"{prefix}_SHA256SUMS",
        "signature": f"{prefix}_SHA256SUMS.sig",
    }


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def check_manifest(data):
    try:
        manifest = json.loads(data)
    except (ValueError, UnicodeError) as error:
        raise ValueError("Registry manifest is not valid JSON") from error
    if (not isinstance(manifest, dict) or type(manifest.get("version")) is not int
            or manifest != {"version": 1, "metadata": {"protocol_versions": ["6.0"]}}):
        raise ValueError("Registry manifest must declare format 1 and protocol 6.0")


def check_binary(data):
    if (len(data) < 64 or data[:6] != b"\x7fELF\x02\x01"
            or int.from_bytes(data[16:18], "little") not in (2, 3)
            or int.from_bytes(data[18:20], "little") != 62):
        raise ValueError("provider binary is not a linux_amd64 ELF executable")


def check_assets(directory, version):
    assets = names(version)
    expected = {assets["archive"], assets["manifest"]}
    sums = {}
    for line in (directory / assets["checksums"]).read_text(encoding="ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([^/\\]+)", line)
        if not match or match[2] in sums:
            raise ValueError("invalid or duplicate release checksum entry")
        sums[match[2]] = match[1]
    if set(sums) != expected:
        raise ValueError("release checksums must cover exactly the package and manifest")
    for name, checksum in sums.items():
        path = directory / name
        if path.is_symlink() or not path.is_file() or digest(path) != checksum:
            raise ValueError(f"release checksum mismatch: {name}")
    check_manifest((directory / assets["manifest"]).read_bytes())
    with zipfile.ZipFile(directory / assets["archive"]) as archive:
        entries = archive.infolist()
        if len(entries) != 1 or entries[0].filename != assets["binary"]:
            raise ValueError("release ZIP must contain only the correctly named provider binary")
        if entries[0].external_attr >> 16 & 0o170777 != 0o100755:
            raise ValueError("release ZIP provider binary must be a regular executable file")
        check_binary(archive.read(entries[0]))
    return assets


def check_signature(directory, version, fingerprint):
    if not re.fullmatch(r"[0-9A-Fa-f]{40}", fingerprint):
        raise ValueError("signing fingerprint must be a full RSA or DSA key fingerprint")
    assets = names(version)
    signature = directory / assets["signature"]
    if signature.read_bytes().startswith(b"-----BEGIN"):
        raise ValueError("Registry checksum signature must be binary, not ASCII armored")
    result = subprocess.run([
        "gpg", "--batch", "--status-fd", "1", "--verify", str(signature),
        str(directory / assets["checksums"]),
    ], capture_output=True, text=True)
    valid = [line.split() for line in result.stdout.splitlines() if line.startswith("[GNUPG:] VALIDSIG ")]
    if result.returncode or len(valid) != 1:
        raise ValueError("release checksum signature did not verify")
    fields = valid[0]
    if len(fields) < 12 or fields[-1].upper() != fingerprint.upper() or fields[8] not in {"1", "3", "17"}:
        raise ValueError("release signature must use the registered RSA or DSA key")
