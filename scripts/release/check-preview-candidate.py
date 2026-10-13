#!/usr/bin/env python3
"""Verify digest-bound local preview files; this does not authorize publication."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import stat
import sys


DEFAULT_RECORD = "docs/qualification/PREVIEW_2026_10_10.json"
SCOPE = "digest_bound_local_preview_integrity_only"
QUALIFICATION = "QUALIFIED_FOR_LOCAL_DEVELOPMENT_SIMULATION"
AWS_NOTICE = "AWS API/AWS integration is not yet ready"
BOUNDARY = {
    "evidence_boundary": "simulation",
    "source_status": "uncommitted_working_tree",
    "platform": "linux_amd64",
    "provider_address": "registry.example.com/platform/platformipam",
    "registry_publication": "NOT_PUBLISHED",
    "signing": "UNSIGNED",
    "live_aws": "NOT_READY",
    "aws_readiness_notice": AWS_NOTICE,
}


def mapping(value, label):
    if not isinstance(value, dict) or not value:
        raise ValueError(f"{label} must be a nonempty JSON object")
    return value


def path_within(base, relative, *, directory=False):
    if (not isinstance(relative, str) or not relative or "\\" in relative
            or "\0" in relative or relative.startswith("/")
            or re.match(r"^[A-Za-z]:", relative)
            or any(part in {"", ".", ".."} for part in relative.split("/"))):
        raise ValueError("referenced paths must be canonical relative paths without traversal")
    path = base
    for part in relative.split("/"):
        path = path / part
        if path.is_symlink():
            raise ValueError(f"referenced paths must not contain symlinks: {relative}")
    if not path.resolve(strict=True).is_relative_to(base):
        raise ValueError(f"referenced path escapes its base: {relative}")
    mode = path.stat().st_mode
    if not (stat.S_ISDIR(mode) if directory else stat.S_ISREG(mode)):
        raise ValueError(f"referenced object has the wrong file type: {relative}")
    return path


def digest_file(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def bound_file(base, relative, digest, label):
    if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise ValueError(f"{label} requires a lowercase SHA-256 digest")
    path = path_within(base, relative)
    if digest_file(path) != digest:
        raise ValueError(f"{label} SHA-256 mismatch: {relative}")
    return path


def reference(root, value, label):
    value = mapping(value, label)
    if set(value) != {"path", "sha256"}:
        raise ValueError(f"{label} must contain exactly path and sha256")
    return bound_file(root, value["path"], value["sha256"], label)


def load_object(path, label):
    def unique_keys(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"{label} contains duplicate JSON keys")
            result[key] = value
        return result

    def invalid_constant(_):
        raise ValueError(f"{label} contains a nonstandard JSON number")

    try:
        value = json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique_keys,
                           parse_constant=invalid_constant)
    except (UnicodeError, json.JSONDecodeError) as error:
        raise ValueError(f"{label} is not valid UTF-8 JSON") from error
    return mapping(value, label)


def verify_hash_map(base, value, label):
    value = mapping(value, label)
    for relative, digest in value.items():
        bound_file(base, relative, digest, label)
    return len(value)


def verify(root, record=DEFAULT_RECORD):
    """Return an integrity result; the record remains an unsigned local claim."""
    root = Path(root).resolve(strict=True)
    if not root.is_dir():
        raise ValueError("root must be an existing directory")
    report = load_object(path_within(root, record), "preview record")
    if type(report.get("schema_version")) is not int or report["schema_version"] != 1:
        raise ValueError("preview record requires schema_version 1")
    if report.get("qualification") != QUALIFICATION:
        raise ValueError("only local development simulation preview records are supported")
    if (report.get("maintainer_release_attestation") is not False
            or "immutable_source_commit" not in report or report["immutable_source_commit"] is not None):
        raise ValueError("preview must remain uncommitted and must not claim maintainer release attestation")
    if "source_review" in report:
        review = mapping(report["source_review"], "source_review")
        if review.get("maintainer_publication_approval") is not False:
            raise ValueError("preview source review must not claim maintainer publication approval")
    for field, expected in BOUNDARY.items():
        if report.get(field) != expected:
            raise ValueError(f"unsupported preview boundary: {field}")
    version = report.get("candidate")
    if not isinstance(version, str) or not re.fullmatch(
            r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-rc\.(0|[1-9][0-9]*)", version):
        raise ValueError("preview candidate must be a canonical rc.N version")
    base_commit = report.get("base_commit")
    if not isinstance(base_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", base_commit):
        raise ValueError("preview requires a full base commit SHA")

    manifest_path = reference(root, report.get("source_manifest"), "source manifest binding")
    manifest = load_object(manifest_path, "source manifest")
    source_count = verify_hash_map(root, manifest, "manifest source file")
    candidate_path = reference(root, report.get("candidate_metadata"), "candidate metadata binding")
    candidate = load_object(candidate_path, "candidate metadata")
    if candidate.get("version") != version or candidate.get("base_commit") != base_commit:
        raise ValueError("candidate version or base commit contradicts preview record")
    if candidate.get("qualification_status") != QUALIFICATION:
        raise ValueError("candidate qualification contradicts local simulation boundary")
    for field, expected in BOUNDARY.items():
        if candidate.get(field) != expected:
            raise ValueError(f"candidate metadata contradicts preview boundary: {field}")
    for value in (report, candidate):
        for field, expected in {"maintainer_release_attestation": False, "immutable_source_commit": None,
                                "maintainer_publication_approval": False, "publication_acceptance": False}.items():
            if field in value and value[field] is not expected:
                raise ValueError(f"preview metadata must not claim release acceptance: {field}")
        if "aws_mode" in value and value["aws_mode"] != "simulation":
            raise ValueError("preview metadata must not claim live AWS execution")
    tested_versions = candidate.get("tested_terraform_versions")
    if not isinstance(tested_versions, list) or not tested_versions or any(
            not isinstance(value, str) or not re.fullmatch(
                r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", value)
            for value in tested_versions):
        raise ValueError("candidate requires exact tested Terraform versions")

    candidate_relative = candidate_path.parent.relative_to(root).as_posix()
    candidate_root = root if candidate_relative == "." else path_within(root, candidate_relative, directory=True)
    source_root = path_within(candidate_root, "source", directory=True)
    license_root = path_within(candidate_root, "linked-licenses", directory=True)
    asset_root = path_within(candidate_root, "assets", directory=True)
    candidate_source = mapping(candidate.get("source_files_sha256"), "candidate source files")
    candidate_source_count = verify_hash_map(source_root, candidate_source, "candidate source file")
    for relative, digest in candidate_source.items():
        if relative == "README.md":  # Generated preview provenance, distinct from root README.
            continue
        original = relative if relative in {"LICENSE", "NOTICE"} else f"providers/terraform/{relative}"
        if manifest.get(original) != digest:
            raise ValueError(f"candidate source is not bound to the qualified source manifest: {relative}")
    license_count = verify_hash_map(license_root, candidate.get("linked_licenses_sha256"), "linked license file")

    assets = mapping(report.get("assets_sha256"), "preview assets")
    if candidate.get("assets_sha256") != assets:
        raise ValueError("candidate asset digests contradict preview record")
    prefix = f"terraform-provider-platformipam_{version}"
    if set(assets) != {f"{prefix}_linux_amd64.zip", f"{prefix}_manifest.json", f"{prefix}_SHA256SUMS"}:
        raise ValueError("preview assets must match candidate version and linux_amd64 target")
    asset_count = verify_hash_map(asset_root, assets, "preview asset")
    evidence = mapping(report.get("evidence"), "preview evidence")
    for name, value in evidence.items():
        if not isinstance(name, str) or not name.strip():
            raise ValueError("preview evidence names must be nonempty strings")
        reference(root, value, "preview evidence binding")
    checks = mapping(report.get("checks"), "preview checks")
    for value in checks.values():
        if (not isinstance(value, dict) or not isinstance(value.get("status"), str) or value["status"] not in {
                "PASSED", "PASSED_WITH_SKIPS", "PASSED_COMBINED_RUNS"}):
            raise ValueError("preview check has unsupported or contradictory status")

    # Informational URLs/report text and execution claims are not dereferenced or authenticated.
    return {
        "status": "PASSED", "scope": SCOPE, "candidate": version,
        "evidence_boundary": "simulation", "source_status": "uncommitted_working_tree",
        "signing": "UNSIGNED", "registry_publication": "NOT_PUBLISHED",
        "maintainer_release_attestation": False, "publication_acceptance": False,
        "live_aws": "NOT_READY", "aws_readiness_notice": AWS_NOTICE,
        "verified": {"manifest_source_files": source_count, "candidate_source_files": candidate_source_count,
                     "linked_license_files": license_count, "assets": asset_count, "evidence_files": len(evidence)},
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--record", default=DEFAULT_RECORD, help="preview JSON record path relative to root")
    args = parser.parse_args()
    try:
        result = verify(args.root, args.record)
    except (OSError, ValueError, RecursionError) as error:
        print(json.dumps({"status": "FAILED", "scope": SCOPE, "publication_acceptance": False,
                          "error": str(error)}, sort_keys=True))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
