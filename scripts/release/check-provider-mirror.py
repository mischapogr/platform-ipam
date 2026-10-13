#!/usr/bin/env python3
"""Verify a provider release repository mirrors the reviewed source commit."""

import argparse
from pathlib import Path
import re
import subprocess
import sys


IGNORED = {Path("README.md"), Path("SOURCE_COMMIT"), Path("LICENSE"), Path("NOTICE"), Path(".gitignore"),
           Path("RELEASE_QUALIFICATION.json")}
IGNORED_DIRECTORIES = {".git", ".github", "release-evidence"}


def check(mirror, source, source_ref):
    if not re.fullmatch(r"[0-9a-f]{40}", source_ref):
        raise ValueError("source-ref must be a full lowercase commit SHA")
    repository = source.resolve().parents[1]

    def git(*args):
        result = subprocess.run(["git", "-C", str(repository), *args], capture_output=True)
        if result.returncode:
            raise ValueError(result.stderr.decode(errors="replace").strip() or "git command failed")
        return result.stdout

    if git("rev-parse", "--verify", f"{source_ref}^{{commit}}").decode().strip() != source_ref:
        raise ValueError("source-ref is not a commit")
    if source.resolve() != repository / "providers/terraform":
        raise ValueError("source must be the authoritative providers/terraform module")
    if (mirror / "SOURCE_COMMIT").read_text(encoding="ascii") != source_ref + "\n":
        raise ValueError("provider mirror names a different source commit")
    if (mirror / "LICENSE").read_bytes() != git("show", f"{source_ref}:LICENSE"):
        raise ValueError("provider mirror licence differs from reviewed source")
    if (mirror / "NOTICE").read_bytes() != git("show", f"{source_ref}:NOTICE"):
        raise ValueError("provider mirror NOTICE differs from reviewed source")
    for path in mirror.rglob("*"):
        if path.is_symlink() and path.relative_to(mirror).parts[0] != ".git":
            raise ValueError(f"provider mirror contains a symlink: {path.relative_to(mirror)}")
    expected = {}
    for entry in git("ls-tree", "-r", "-z", source_ref, "--", "providers/terraform").split(b"\0"):
        if not entry:
            continue
        metadata, raw_path = entry.split(b"\t", 1)
        mode, kind, object_id = metadata.decode().split()
        if kind != "blob" or mode not in ("100644", "100755"):
            raise ValueError(f"unsupported source entry: {raw_path.decode()}")
        path = Path(raw_path.decode().removeprefix("providers/terraform/"))
        expected[path] = object_id
    actual = {path.relative_to(mirror) for path in mirror.rglob("*") if path.is_file()
              and path.relative_to(mirror) not in IGNORED
              and path.relative_to(mirror).parts[0] not in IGNORED_DIRECTORIES}
    if set(expected) != actual:
        missing = sorted(str(path) for path in set(expected) - actual)
        extra = sorted(str(path) for path in actual - set(expected))
        raise ValueError(f"provider mirror file set differs: missing={missing}, extra={extra}")
    for path in sorted(expected):
        if (mirror / path).read_bytes() != git("cat-file", "blob", expected[path]):
            raise ValueError(f"provider mirror differs from reviewed source: {path}")
    return len(expected)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mirror", required=True, type=Path)
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--source-ref", required=True)
    args = parser.parse_args()
    try:
        count = check(args.mirror, args.source, args.source_ref)
    except (OSError, UnicodeError, ValueError) as error:
        print(f"provider mirror: {error}", file=sys.stderr)
        return 1
    print(f"provider mirror matches {count} files from {args.source_ref}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
