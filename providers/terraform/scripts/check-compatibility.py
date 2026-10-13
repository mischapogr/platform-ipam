#!/usr/bin/env python3
"""Validate provider compatibility metadata without authenticating execution."""

import argparse
import json
from pathlib import Path
import re
import sys


SEMVER = re.compile(
    r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)"
    r"(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
    r"(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?"
)


def version(value, label):
    match = SEMVER.fullmatch(value) if isinstance(value, str) else None
    if not match or (match[4] and any(
            part.isdigit() and len(part) > 1 and part.startswith("0") for part in match[4].split("."))):
        raise ValueError(f"{label} must be a canonical SemVer without v")
    return match


def version_list(value, label, *, empty=False, stable=False):
    if not isinstance(value, list) or (not empty and not value):
        raise ValueError(f"{label} must be a {'possibly empty ' if empty else 'nonempty '}version list")
    for item in value:
        parsed = version(item, label)
        if stable and (parsed[4] or parsed[5]):
            raise ValueError(f"{label} must contain exact stable X.Y.Z versions")
    if len(set(value)) != len(value):
        raise ValueError(f"{label} must not contain duplicate versions")
    return value


def object_fields(value, fields, label):
    if not isinstance(value, dict) or set(value) != set(fields):
        raise ValueError(f"{label} has missing or unsupported fields")
    return value


def enum(value, choices, label):
    if not isinstance(value, str) or value not in choices:
        raise ValueError(f"{label} is unsupported")
    return value


def read(path, label):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"{label} must be an existing regular file without symlinks")
    return path.read_text(encoding="utf-8")


def json_object(path, label):
    def unique_keys(pairs):
        value = {}
        for key, item in pairs:
            if key in value:
                raise ValueError(f"{label} contains duplicate JSON keys")
            value[key] = item
        return value

    def invalid_number(_):
        raise ValueError(f"{label} contains a nonstandard JSON number")

    value = json.loads(read(path, label), object_pairs_hook=unique_keys, parse_constant=invalid_number)
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be a JSON object")
    return value


def check(provider_directory, expected_version=None, *, release=False, qualification=None):
    directory = Path(provider_directory)
    raw_version = read(directory / "VERSION", "VERSION")
    provider_version = raw_version.removesuffix("\n")
    parsed = version(provider_version, "VERSION")
    is_rc = bool(parsed[4] and re.fullmatch(r"rc\.(0|[1-9][0-9]*)", parsed[4]))
    if parsed[5] or (parsed[4] and not is_rc):
        raise ValueError("VERSION must use a stable or rc.N release channel without build metadata")
    if expected_version is not None:
        version(expected_version, "expected version")
        if provider_version != expected_version:
            raise ValueError("VERSION differs from the expected release version")
    matrix = object_fields(json_object(directory / "COMPATIBILITY.json", "compatibility matrix"),
                           {"schema_version", "provider_version", "platform_api_major", "platform_release_versions",
                            "qualification", "aws_integration"}, "compatibility matrix")
    if type(matrix["schema_version"]) is not int or matrix["schema_version"] != 1:
        raise ValueError("compatibility schema_version must be integer 1")
    if type(matrix["platform_api_major"]) is not int or matrix["platform_api_major"] != 1:
        raise ValueError("only Platform-IPAM API major 1 is supported")
    version(matrix["provider_version"], "matrix provider_version")
    if matrix["provider_version"] != provider_version:
        raise ValueError("compatibility provider_version differs from VERSION")
    manifest = json_object(directory / "terraform-registry-manifest.json", "Registry manifest")
    if (type(manifest.get("version")) is not int or manifest != {
            "version": 1, "metadata": {"protocol_versions": ["6.0"]}}):
        raise ValueError("Registry manifest must declare format 1 and protocol 6.0")
    policy = object_fields(matrix["qualification"], {"status", "scope", "terraform_cli_versions", "platforms"},
                           "compatibility qualification")
    status = enum(policy["status"], {"pending", "qualified"}, "qualification status")
    scope = enum(policy["scope"], {"simulation", "authorized_aws_sandbox"}, "qualification scope")
    aws = enum(matrix["aws_integration"], {"not_ready", "ready"}, "AWS integration readiness")
    cli_versions = version_list(policy["terraform_cli_versions"], "Terraform CLI versions", stable=True)
    if policy["platforms"] != ["linux_amd64"]:
        raise ValueError("qualified platforms must be exactly linux_amd64")
    products = version_list(matrix["platform_release_versions"], "Platform-IPAM release versions", empty=True)
    if is_rc and (scope != "simulation" or aws != "not_ready"):
        raise ValueError("RC compatibility must retain simulation scope and unready AWS integration")
    if not is_rc and (status != "qualified" or scope != "authorized_aws_sandbox" or not products or aws != "ready"):
        raise ValueError("non-RC versions require qualified authorized AWS sandbox scope, product versions and ready AWS integration")
    if release and status != "qualified":
        raise ValueError("published releases require qualified compatibility; pending metadata is structural only")
    if release and qualification is None:
        raise ValueError("release validation requires a source-bound qualification record")
    if qualification is not None:
        attestation = json_object(Path(qualification), "release qualification")
        if attestation.get("provider_version") != provider_version:
            raise ValueError("qualification provider version differs from compatibility matrix")
        if attestation.get("evidence_boundary") != scope:
            raise ValueError("qualification scope differs from compatibility matrix")
        if sorted(version_list(attestation.get("tested_terraform_versions"), "attested Terraform versions", stable=True)) != sorted(cli_versions):
            raise ValueError("attested Terraform versions differ from compatibility matrix")
        if [attestation.get("platform")] != policy["platforms"]:
            raise ValueError("attested platform differs from compatibility matrix")
        attested_products = version_list(attestation.get("tested_platform_versions", []),
                                        "attested Platform-IPAM versions", empty=is_rc)
        if sorted(attested_products) != sorted(products):
            raise ValueError("attested Platform-IPAM versions differ from compatibility matrix")
    return {"status": "PASSED", "scope": "compatibility_metadata_validation", "provider_version": provider_version,
            "platform_api_major": 1, "qualification_status": status, "qualification_scope": scope,
            "platform_release_versions": products, "aws_integration": aws,
            "release_checks": release, "execution_authenticated": False, "publication_acceptance": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provider-directory", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--expected-version")
    parser.add_argument("--release", action="store_true", help="require qualified matrix and matching attestation")
    parser.add_argument("--qualification", type=Path, help="source-bound RELEASE_QUALIFICATION.json")
    args = parser.parse_args()
    try:
        result = check(args.provider_directory, args.expected_version, release=args.release, qualification=args.qualification)
    except (OSError, ValueError, RecursionError) as error:
        print(json.dumps({"status": "FAILED", "scope": "compatibility_metadata_validation", "error": str(error)}, sort_keys=True))
        return 1
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
