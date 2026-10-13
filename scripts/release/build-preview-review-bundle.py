#!/usr/bin/env python3
"""Create a deterministic, unsigned review bundle from a qualified snapshot.

Only digest-bound source, evidence and candidate files are copied. The
qualification record is preserved; this does not approve or publish a release.
"""

import argparse
import gzip
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile


DEFAULT_RECORD = "docs/qualification/PREVIEW_2026_10_10.json"
CHECKER = Path(__file__).with_name("check-preview-candidate.py")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def bound_path(root, relative):
    if not isinstance(relative, str) or not relative:
        raise ValueError("Expected a nonempty relative file path")
    path = Path(relative)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError("File path must be relative and cannot traverse parents")
    resolved = (root / path).resolve()
    if not resolved.is_relative_to(root.resolve()) or not resolved.is_file():
        raise ValueError("File path escapes its root or does not identify a file")
    return resolved


def verify(root, record):
    result = subprocess.run(
        [sys.executable, str(CHECKER), "--root", str(root), "--record", record],
        capture_output=True, text=True, check=False)
    if result.returncode:
        detail = (result.stdout or result.stderr).strip()
        raise ValueError("Preview integrity check failed: " + detail[:1200])
    return json.loads(result.stdout)


def build(root, record_path, output):
    root = root.resolve()
    checksum_path = output.with_name(output.name + ".sha256")
    if output.exists() or output.is_symlink() or checksum_path.exists() or checksum_path.is_symlink():
        raise ValueError("Refusing to overwrite a bundle or its checksum")
    verification = verify(root, record_path)
    record = json.loads(bound_path(root, record_path).read_text())
    source_manifest = record["source_manifest"]["path"]
    source_files = json.loads(bound_path(root, source_manifest).read_text())
    metadata_path = record["candidate_metadata"]["path"]
    metadata = json.loads(bound_path(root, metadata_path).read_text())
    candidate = Path(metadata_path).parent
    files = set(source_files) | {record_path, source_manifest, metadata_path}
    files.update(item["path"] for item in record["evidence"].values())
    for field, directory in (("source_files_sha256", "source"),
                             ("linked_licenses_sha256", "linked-licenses"),
                             ("assets_sha256", "assets")):
        files.update((candidate / directory / name).as_posix()
                     for name in metadata[field])
    with tempfile.TemporaryDirectory(prefix="platform-ipam-review-") as temporary:
        stage = Path(temporary) / "platform-ipam-preview-review"
        stage.mkdir()
        for name in sorted(files):
            source = bound_path(root, name)
            target = stage / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
        # Include the review tools as separate, explicitly unhashed-by-the-
        # original-qualification additions; the bundle manifest binds them.
        for tool in (CHECKER, Path(__file__)):
            target = stage / "scripts/release" / tool.name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(tool, target)
        verify(stage, record_path)
        review = {
            "schema_version": 1,
            "kind": "unsigned_local_simulation_review_bundle",
            "qualification_record": record_path,
            "qualification_record_sha256": sha256(stage / record_path),
            "qualification": record["qualification"],
            "publication": "NOT_PUBLISHED",
            "notice": "AWS API/AWS integration is not yet ready",
            "scope": "Digest integrity only; no maintainer attestation or signature.",
            "verification": verification,
            "files_sha256": {p.relative_to(stage).as_posix(): sha256(p)
                             for p in sorted(stage.rglob("*")) if p.is_file()},
        }
        (stage / "BUNDLE_MANIFEST.json").write_text(
            json.dumps(review, indent=2, sort_keys=True) + "\n")
        output.parent.mkdir(parents=True, exist_ok=True)
        owns_output = False
        try:
            with output.open("xb") as raw:
                owns_output = True
                with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                    with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                        for path in sorted(stage.rglob("*")):
                            if not path.is_file():
                                continue
                            info = archive.gettarinfo(path, arcname=(
                                Path(stage.name) / path.relative_to(stage)).as_posix())
                            info.uid = info.gid = 0
                            info.uname = info.gname = ""
                            info.mtime = 0
                            info.mode = 0o755 if path.stat().st_mode & 0o111 else 0o644
                            info.pax_headers = {}
                            with path.open("rb") as stream:
                                archive.addfile(info, stream)
        except Exception:
            if owns_output:
                output.unlink(missing_ok=True)
            raise
    checksum = sha256(output)
    with checksum_path.open("x") as stream:
        stream.write(checksum + "  " + output.name + "\n")
    return {"status": "PASSED", "bundle": str(output), "sha256": checksum,
            "qualification": record["qualification"], "publication": "NOT_PUBLISHED"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--record", default=DEFAULT_RECORD)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    try:
        result = build(args.root, args.record, args.out)
    except (OSError, ValueError, KeyError, TypeError) as error:
        print(json.dumps({"status": "FAILED", "detail": str(error)}))
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
