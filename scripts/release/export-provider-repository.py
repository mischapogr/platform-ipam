#!/usr/bin/env python3
"""Export the reviewed provider source into a new, empty repository directory."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
SOURCE_PREFIX = "providers/terraform/"


def git(*args):
    result = subprocess.run(["git", "-C", str(ROOT), *args], capture_output=True)
    if result.returncode:
        raise ValueError(result.stderr.decode(errors="replace").strip() or "git command failed")
    return result.stdout


def export(source_ref, destination, preview=False):
    if not re.fullmatch(r"[0-9a-f]{40}", source_ref):
        raise ValueError("source-ref must be a full lowercase commit SHA")
    if git("rev-parse", "--verify", f"{source_ref}^{{commit}}").decode().strip() != source_ref:
        raise ValueError("source-ref is not a commit")
    if not preview:
        try:
            git("merge-base", "--is-ancestor", source_ref, "refs/heads/main")
        except ValueError as error:
            raise ValueError("source-ref must be on local main; --preview exports an unpromoted bootstrap only") from error
    if destination.is_symlink():
        raise ValueError("destination must not be a symlink")
    destination = destination.resolve()
    if destination.exists() and (not destination.is_dir() or any(destination.iterdir())):
        raise ValueError("destination must be absent or an empty directory")
    if not destination.parent.is_dir():
        raise ValueError("destination parent directory does not exist")

    entries = git("ls-tree", "-r", "-z", source_ref, "--", "providers/terraform", "LICENSE", "NOTICE").split(b"\0")
    files = []
    for entry in entries:
        if not entry:
            continue
        metadata, raw_path = entry.split(b"\t", 1)
        mode, kind, object_id = metadata.decode().split()
        path = raw_path.decode()
        if kind != "blob" or mode not in ("100644", "100755"):
            raise ValueError(f"unsupported source entry: {path}")
        if path in ("LICENSE", "NOTICE"):
            target = Path(path)
        elif path.startswith(SOURCE_PREFIX):
            target = Path(path.removeprefix(SOURCE_PREFIX))
            if target in (Path("LICENSE"), Path("NOTICE"), Path("README.md"), Path("SOURCE_COMMIT")):
                raise ValueError(f"provider source conflicts with generated release metadata: {path}")
            if preview and target == Path(".github/PREVIEW_SOURCE.json"):
                raise ValueError(f"provider source conflicts with generated preview metadata: {path}")
        else:
            raise ValueError(f"unexpected source entry: {path}")
        if target.is_absolute() or ".." in target.parts:
            raise ValueError(f"unsafe source entry: {path}")
        files.append((target, object_id, mode))
    required = {Path(path) for path in (
        "LICENSE", "NOTICE", "go.mod", "main.go", "terraform-registry-manifest.json",
        "docs/index.md", "docs/resources/allocation.md",
        "docs/data-sources/allocation.md", "docs/data-sources/pool.md",
        "scripts/check-replacement-plan.py",
    )}
    if not required <= {item[0] for item in files}:
        raise ValueError("reviewed source is missing required provider files")

    with tempfile.TemporaryDirectory(prefix="provider-export-", dir=destination.parent) as temporary:
        stage = Path(temporary) / "repository"
        stage.mkdir()
        for target, object_id, mode in files:
            output = stage / target
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_bytes(git("cat-file", "blob", object_id))
            output.chmod(0o755 if mode == "100755" else 0o644)
        (stage / "SOURCE_COMMIT").write_text(source_ref + "\n", encoding="ascii")
        (stage / "README.md").write_text(
            "# Platform-IPAM Terraform provider\n\n"
            "This source mirror was exported from `mischapogr/platform-ipam` commit "
            f"`{source_ref}`. The authoritative provider source is `providers/terraform` "
            "at that commit. Do not edit this mirror by hand.\n\n" +
            ("Registry publication, main promotion, signer custody and named review "
             "remain separate release gates.\n" if preview else
             "Registry publication, the source namespace, signing key and sandbox "
             "acceptance remain separate release gates.\n"),
            encoding="utf-8",
        )
        if preview:
            metadata = {
                "schema_version": 1,
                "export_kind": "preview_bootstrap",
                "source_commit": source_ref,
                "source_repository": "mischapogr/platform-ipam",
                "source_status": "committed_branch_preview",
                "main_ancestry_verified": False,
                "main_promotion": "REQUIRED_FOR_RELEASE",
                "provider_repository": "mischapogr/terraform-provider-platformipam",
                "provider_address": "registry.terraform.io/mischapogr/platformipam",
                "release_status": "NOT_A_RELEASE",
                "release_eligible": False,
                "maintainer_release_attestation": False,
                "registry_publication": "NOT_PUBLISHED",
                "signing": "UNSIGNED",
                "evidence_boundary": "simulation",
                "live_aws": "NOT_READY",
                "aws_readiness_notice": "AWS API/AWS integration is not yet ready",
            }
            metadata_path = stage / ".github/PREVIEW_SOURCE.json"
            metadata_path.parent.mkdir(parents=True, exist_ok=True)
            metadata_path.write_text(json.dumps(metadata, sort_keys=True, indent=2) + "\n", encoding="utf-8")
            with (stage / "README.md").open("a", encoding="utf-8") as readme:
                readme.write(
                    "\n## Unpromoted preview bootstrap\n\n"
                    "This is an unpromoted preview source export, not a provider release. "
                    "It uses committed branch source and does not verify main ancestry.\n\n"
                    "The approved target repository is `mischapogr/terraform-provider-platformipam`; "
                    "the selected public provider identity is "
                    "`registry.terraform.io/mischapogr/platformipam`. Registry publication has not occurred.\n\n"
                    "Promotion of the reviewed source to monorepo `main`, named maintainer review, "
                    "Registry enrollment, registered signer custody and release qualification "
                    "remain required before a signed release.\n\n"
                    "**AWS API/AWS integration is not yet ready.**\n"
                )
        os.replace(stage, destination)
    return len(files)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-ref", required=True, help="reviewed full commit SHA; local main required by default")
    parser.add_argument("--out", required=True, type=Path, help="absent or empty destination directory")
    parser.add_argument("--preview", action="store_true",
                        help="bootstrap committed branch source with explicit unpromoted, non-release metadata")
    args = parser.parse_args()
    try:
        count = export(args.source_ref, args.out, preview=args.preview)
    except (OSError, ValueError) as error:
        print(f"provider export: {error}", file=sys.stderr)
        return 1
    print(f"exported {count} tracked source files from {args.source_ref} to {args.out}")
    if args.preview:
        print("preview bootstrap only: unpromoted, unsigned, not published; main promotion required for release")
    return 0


if __name__ == "__main__":
    sys.exit(main())
