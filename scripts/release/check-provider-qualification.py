#!/usr/bin/env python3
"""Validate a maintainer's digest-bound sandbox qualification attestation."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


CHECKS = {
    "plan", "apply", "read", "import", "destroy", "create_timeout_recovery",
    "ambiguous_read_retains_identity", "replacement_uses_new_key", "durable_quarantine_acceptance",
    "normal_install_and_checksum_refusal",
    "vulnerability_scan",
}
AWS_NOTICE = "AWS API/AWS integration is not yet ready"


def check(mirror, source_ref, version):
    record_path = mirror / "RELEASE_QUALIFICATION.json"
    if record_path.is_symlink():
        raise ValueError("qualification record must not be a symlink")
    record = json.loads(record_path.read_text(encoding="utf-8"))
    if (not isinstance(record, dict) or type(record.get("schema_version")) is not int
            or record.get("schema_version") != 1):
        raise ValueError("qualification requires schema_version 1")
    if record.get("source_commit") != source_ref or not re.fullmatch(r"[0-9a-f]{40}", source_ref):
        raise ValueError("qualification source commit differs from reviewed source")
    if record.get("provider_version") != version or record.get("platform") != "linux_amd64":
        raise ValueError("qualification provider version or target differs from release")
    is_rc = bool(re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-rc\.(0|[1-9][0-9]*)", version))
    is_stable = bool(re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version))
    if not is_rc and not is_stable:
        raise ValueError("qualification supports stable or rc.N semantic versions")
    for field in ("reviewer", "authorization_reference"):
        if not isinstance(record.get(field), str) or not record[field].strip():
            raise ValueError(f"qualification requires a named {field}")
    if is_rc:
        if (record.get("release_channel") != "rc" or record.get("evidence_boundary") != "simulation"
                or record.get("live_aws") != "NOT_READY" or record.get("aws_readiness_notice") != AWS_NOTICE):
            raise ValueError("RC qualification must disclose simulation and unready AWS integration")
    elif (record.get("release_channel") != "stable" or record.get("evidence_boundary") != "authorized_aws_sandbox"
          or record.get("aws_mode") != "live"):
        raise ValueError("stable qualification requires authorized live AWS acceptance")
    versions = record.get("tested_terraform_versions")
    if not isinstance(versions, list) or not versions or any(
            not isinstance(value, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", value)
            for value in versions):
        raise ValueError("qualification requires exact tested Terraform versions")
    checks = record.get("checks")
    if not isinstance(checks, dict) or set(checks) != CHECKS:
        raise ValueError("qualification must include every required sandbox acceptance check")
    for name, result in checks.items():
        if not isinstance(result, dict) or result.get("status") != "passed":
            raise ValueError(f"sandbox qualification did not pass: {name}")
        relative = result.get("evidence_file")
        if not isinstance(relative, str):
            raise ValueError(f"qualification lacks an evidence file: {name}")
        path = Path(relative)
        if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != "release-evidence":
            raise ValueError(f"qualification evidence must be within release-evidence/: {name}")
        current = mirror
        for part in path.parts:
            current = current / part
            if current.is_symlink():
                raise ValueError(f"qualification evidence must not contain symlinks: {name}")
        expected = result.get("sha256")
        if not isinstance(expected, str) or not re.fullmatch(r"[0-9a-f]{64}", expected):
            raise ValueError(f"qualification lacks a SHA-256 evidence digest: {name}")
        with current.open("rb") as stream:
            actual = hashlib.file_digest(stream, "sha256").hexdigest()
        if expected != actual:
            raise ValueError(f"qualification evidence checksum differs: {name}")
    return len(checks)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mirror", required=True, type=Path)
    parser.add_argument("--source-ref", required=True)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    try:
        count = check(args.mirror, args.source_ref, args.version)
    except (OSError, UnicodeError, ValueError) as error:
        print(f"provider qualification: {error}", file=sys.stderr)
        return 1
    print(f"maintainer attestation: {count} checks and evidence digests passed; runtime or AWS execution is not authenticated by this check")
    return 0


if __name__ == "__main__":
    sys.exit(main())
