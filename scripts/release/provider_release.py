"""Registry asset validation shared by the unsigned packager and release check."""

import hashlib
import json
from pathlib import Path, PurePosixPath
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


def safe_notice_path(name):
    if (not isinstance(name, str) or not name or "\\" in name
            or any(ord(char) < 32 for char in name)
            or name.startswith("/") or any(part in ("", ".", "..") for part in name.split("/"))
            or str(PurePosixPath(name)) != name):
        raise ValueError("unsafe notice path")
    return name


def json_object(data):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError("duplicate JSON field")
            result[key] = value
        return result
    try:
        value = json.loads(data, object_pairs_hook=pairs)
    except (UnicodeError, ValueError) as error:
        raise ValueError("invalid notice metadata JSON") from error
    if not isinstance(value, dict):
        raise ValueError("notice metadata must be an object")
    return value


def check_notice_packet(files, binary_data, version):
    if "PUBLISHED_BINARY.json" not in files:
        raise ValueError("notice packet lacks PUBLISHED_BINARY.json")
    record = json_object(files["PUBLISHED_BINARY.json"])
    if (record.get("binary_sha256") != hashlib.sha256(binary_data).hexdigest()
            or record.get("provider_version") != version):
        raise ValueError("notice packet binary/version binding mismatch")
    hashes = record.get("files_sha256")
    if not isinstance(hashes, dict) or set(hashes) != set(files) - {"PUBLISHED_BINARY.json"}:
        raise ValueError("notice packet has missing or unlisted files")
    for name, expected in hashes.items():
        safe_notice_path(name)
        if hashlib.sha256(files[name]).hexdigest() != expected:
            raise ValueError(f"notice packet checksum mismatch: {name}")

    if not {"INVENTORY.json", "BUILD_INFO.txt"} <= set(files):
        raise ValueError("notice packet requires inventory and build information")
    inventory = json_object(files["INVENTORY.json"])
    modules = inventory.get("modules")
    if (inventory.get("missing_root_license_modules") != []
            or not isinstance(modules, list) or not modules
            or type(record.get("linked_module_count")) is not int
            or record["linked_module_count"] != len(modules)):
        raise ValueError("notice packet inventory is incomplete")
    identities = set()
    for module in modules:
        if not isinstance(module, dict):
            raise ValueError("invalid notice inventory module")
        identity = (module.get("module"), module.get("version"))
        if any(not isinstance(value, str) or not value or any(char.isspace() for char in value)
               for value in identity):
            raise ValueError("invalid notice inventory module identity")
        if identity in identities:
            raise ValueError("duplicate notice inventory module")
        identities.add(identity)
        copied = module.get("copied_root_license_notice_files")
        if not isinstance(copied, list) or not copied:
            raise ValueError("inventory module lacks licence/notice texts")
        for item in copied:
            if not isinstance(item, dict):
                raise ValueError("invalid inventory licence/notice entry")
            name = safe_notice_path(item.get("file"))
            if name not in hashes or not files[name] or item.get("sha256") != hashes[name]:
                raise ValueError("inventory licence/notice digest mismatch or missing text")
    try:
        build_lines = files["BUILD_INFO.txt"].decode("utf-8").splitlines()
    except UnicodeError as error:
        raise ValueError("invalid build information text") from error
    go_version = record.get("build_go_version")
    if (not isinstance(go_version, str) or not re.fullmatch(r"go[0-9]+\.[0-9]+\.[0-9]+", go_version)
            or not build_lines or not build_lines[0].endswith(": " + go_version)):
        raise ValueError("notice packet Go version does not match build information")
    go_licence = "GO_" + go_version.removeprefix("go").replace(".", "_") + "_LICENSE"
    if not files.get(go_licence):
        raise ValueError("notice packet lacks the matching Go licence")
    dependencies = []
    for line in build_lines[1:]:
        parts = line.split()
        if parts and parts[0] == "dep":
            if len(parts) < 3:
                raise ValueError("invalid dependency build information")
            dependencies.append((parts[1], parts[2]))
    if len(set(dependencies)) != len(dependencies) or set(dependencies) != identities:
        raise ValueError("notice inventory does not match linked build modules")


def notice_files(directory, project_license, project_notice, binary_data, version):
    files = {}
    if any(path.is_symlink() for path in (directory, *directory.parents)) or not directory.is_dir():
        raise ValueError("notice directory must be a regular directory")
    for path in sorted(directory.rglob("*")):
        if path.is_symlink() or not (path.is_dir() or path.is_file()):
            raise ValueError("notice inputs must be regular files or directories")
        if path.is_file():
            name = safe_notice_path(path.relative_to(directory).as_posix())
            if name == "CONTENTS.json":
                raise ValueError("CONTENTS.json is reserved for generated metadata")
            files[name] = path.read_bytes()
    check_notice_packet(files, binary_data, version)
    result = {"THIRD_PARTY_NOTICES/" + name: data for name, data in files.items()}
    for name, path in (("LICENSE", project_license), ("NOTICE", project_notice)):
        if any(parent.is_symlink() for parent in (path, *path.parents)) or not path.is_file():
            raise ValueError("project licence and notice must be regular files")
        result[name] = path.read_bytes()
    contents = {
        "schema_version": 1, "provider_version": version,
        "binary_sha256": hashlib.sha256(binary_data).hexdigest(),
        "files_sha256": {name: hashlib.sha256(data).hexdigest() for name, data in sorted(result.items())},
    }
    result["THIRD_PARTY_NOTICES/CONTENTS.json"] = (json.dumps(contents, sort_keys=True, indent=2) + "\n").encode()
    return result


def check_archive(path, version):
    """Validate both historical binary-only and notice-bearing release archives."""
    binary_name = names(version)["binary"]
    contents_name = "THIRD_PARTY_NOTICES/CONTENTS.json"
    with zipfile.ZipFile(path) as archive:
        entries = archive.infolist()
        filenames = [entry.filename for entry in entries]
        if len(set(filenames)) != len(filenames):
            raise ValueError("duplicate release ZIP entry")
        if binary_name not in filenames:
            raise ValueError("release ZIP lacks correctly named provider binary")
        for entry in entries:
            safe_notice_path(entry.filename)
            expected_mode = 0o100755 if entry.filename == binary_name else 0o100644
            if entry.external_attr >> 16 != expected_mode:
                raise ValueError("release ZIP entries must have regular file permissions")
        binary_data = archive.read(binary_name)
        check_binary(binary_data)
        if filenames == [binary_name]:
            return binary_data
        added = set(filenames) - {binary_name}
        if (not {"LICENSE", "NOTICE", contents_name} <= added
                or any(name not in {"LICENSE", "NOTICE"} and not name.startswith("THIRD_PARTY_NOTICES/")
                       for name in added)):
            raise ValueError("release ZIP must contain only the correctly named binary and complete notices")
        contents = json_object(archive.read(contents_name))
        if (set(contents) != {"schema_version", "provider_version", "binary_sha256", "files_sha256"}
                or type(contents.get("schema_version")) is not int or contents["schema_version"] != 1
                or contents.get("provider_version") != version
                or contents.get("binary_sha256") != hashlib.sha256(binary_data).hexdigest()):
            raise ValueError("embedded notice binary/version binding mismatch")
        hashes = contents.get("files_sha256")
        if not isinstance(hashes, dict) or set(hashes) != added - {contents_name}:
            raise ValueError("embedded notice hashes have missing or unlisted files")
        for name, expected in hashes.items():
            if hashlib.sha256(archive.read(name)).hexdigest() != expected:
                raise ValueError(f"embedded notice checksum mismatch: {name}")
        packet = {name.removeprefix("THIRD_PARTY_NOTICES/"): archive.read(name)
                  for name in added - {contents_name} if name.startswith("THIRD_PARTY_NOTICES/")}
        check_notice_packet(packet, binary_data, version)
        return binary_data


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
    check_archive(directory / assets["archive"], version)
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
