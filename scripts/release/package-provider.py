#!/usr/bin/env python3
"""Create deterministic, unsigned linux_amd64 Registry assets from a built binary."""

import argparse
from pathlib import Path
import sys
import zipfile

from provider_release import check_assets, check_binary, check_manifest, digest, names, notice_files


def package(binary, manifest, version, destination, notices=None, project_license=None, project_notice=None):
    assets = names(version)
    binary_data = binary.read_bytes()
    manifest_data = manifest.read_bytes()
    check_binary(binary_data)
    check_manifest(manifest_data)
    provided = (notices, project_license, project_notice)
    if any(value is not None for value in provided) and not all(value is not None for value in provided):
        raise ValueError("--notices, --project-license and --project-notice are required together")
    extra = notice_files(notices, project_license, project_notice, binary_data, version) if notices is not None else {}
    if destination.is_symlink() or (destination.exists() and
                                   (not destination.is_dir() or any(destination.iterdir()))):
        raise ValueError("destination must be absent or an empty directory")
    destination.mkdir(parents=False, exist_ok=True)
    with zipfile.ZipFile(destination / assets["archive"], "w") as archive:
        for name, data in [(assets["binary"], binary_data), *sorted(extra.items())]:
            entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.create_system = 3
            entry.external_attr = (0o100755 if name == assets["binary"] else 0o100644) << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            archive.writestr(entry, data)
    (destination / assets["manifest"]).write_bytes(manifest_data)
    (destination / assets["checksums"]).write_text(
        "".join(f"{digest(destination / assets[name])}  {assets[name]}\n" for name in ("archive", "manifest")),
        encoding="ascii",
    )
    check_assets(destination, version)
    return assets


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--notices", type=Path)
    parser.add_argument("--project-license", type=Path)
    parser.add_argument("--project-notice", type=Path)
    args = parser.parse_args()
    try:
        package(args.binary, args.manifest, args.version, args.out,
                args.notices, args.project_license, args.project_notice)
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        print(f"provider package: {error}", file=sys.stderr)
        return 1
    print(f"unsigned linux_amd64 assets validated: {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
