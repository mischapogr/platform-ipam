#!/usr/bin/env python3
"""Create a fresh, explicitly synthetic pilot for the local development pool."""

import argparse
import csv
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import shutil
import subprocess


REGION = "eu-central-1"
TARGET_ACCOUNT = "000000000000"
OTHER_ACCOUNT = "111111111111"
MISSING_ACCOUNT = "222222222222"
OLD_CIDR = "10.20.0.0/22"
ROOT = Path(__file__).resolve().parents[2]


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def report(path, command, expected=(0, 3)):
    result = subprocess.run(command, cwd=ROOT, capture_output=True, text=True,
                            timeout=120, check=False)
    if result.returncode not in expected:
        raise RuntimeError(f"{path.name}: exit {result.returncode}: {result.stderr.strip()}")
    data = json.loads(result.stdout)
    path.write_text(result.stdout, encoding="utf-8")
    return data


def generate(out, key, binary, now):
    out = out.resolve()
    if out.exists() and any(out.iterdir()):
        raise ValueError("output directory must be empty")
    out.mkdir(parents=True, exist_ok=True)
    started = now - timedelta(minutes=3)
    observed = now - timedelta(minutes=2)
    finished = now - timedelta(minutes=1)
    stamp = lambda value: value.isoformat(timespec="seconds").replace("+00:00", "Z")
    accounts = ((TARGET_ACCOUNT, "synthetic-target", "vpc-00000000000000001", OLD_CIDR),
                (OTHER_ACCOUNT, "synthetic-peer", "vpc-00000000000000002", OLD_CIDR),
                (MISSING_ACCOUNT, "synthetic-third", "vpc-00000000000000003", "10.30.0.0/22"))
    write_json(out / "accounts.json", {"Accounts": [
        {"Id": account, "Name": name, "Status": "ACTIVE"} for account, name, _, _ in accounts]})
    rows = []
    for account, name, vpc, cidr in accounts:
        rows.append({"account_id": account, "account_name": name, "region": REGION,
            "type": "vpc", "resource_id": vpc, "cidr": cidr, "name": name,
            "state": "available", "primary": "true",
            "association_id": "vpc-cidr-assoc-" + account, "observed_at": stamp(observed)})
    columns = ("account_id", "account_name", "region", "type", "resource_id", "cidr",
        "parent_id", "az_id", "name", "state", "primary", "association_id", "observed_at")
    with (out / "networks.csv").open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=columns, lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)
    with (out / "failures.csv").open("w", encoding="utf-8", newline="") as handle:
        csv.writer(handle, lineterminator="\n").writerow(
            ("account_id", "account_name", "region", "stage", "error"))
    run = {"script_version": "synthetic-beta-v1",
        "started_at": stamp(started), "finished_at": stamp(finished),
        "configured_regions": [REGION], "accounts": [
            {"account_id": account, "account_name": name,
             "credential_source": "synthetic-fixture", "regions_attempted": "known",
             "region_source": "configured", "regions": [{"region": REGION,
                "outcome": "succeeded", "row_count": 1, "observed_at": stamp(observed)}]}
            for account, name, _, _ in accounts]}
    write_json(out / "run.json", run)
    write_json(out / "matrix.yaml", {"version": 1, "groups": [
        {"id": "target", "members": [TARGET_ACCOUNT]},
        {"id": "peer", "members": [OTHER_ACCOUNT]}],
        "must_communicate": [["target", "peer"]], "must_stay_isolated": [],
        "shared_services": []})
    write_json(out / "ownership.yaml", [
        {"account_id": account, "product": name, "environment": "development",
         "owner": "synthetic-operator"} for account, name, _, _ in accounts])
    write_json(out / "protected.json", [])
    write_json(out / "approved-plan.json", {"version": 1, "pools": [{
        "id": "pool_dev_euc1", "cidr": "10.64.0.0/16", "region": REGION,
        "authority": "platform-ipam", "allowed_prefix_lengths": [22],
        "eligible_accounts": [TARGET_ACCOUNT]}]})
    write_json(out / "pilot-scope.json", {"version": 1, "owner": "synthetic-operator",
        "accounts": [account for account, _, _, _ in accounts], "regions": [REGION]})
    common = ["--matrix", str(out / "matrix.yaml"), "--fixed", str(out / "protected.json"),
              "--ownership", str(out / "ownership.yaml"), "--format", "json"]
    assessment = report(out / "assessment.json", [str(binary), "onboard", "assess",
                        "--inventory", str(out), *common])
    conflicts = assessment["conflicts"]
    if len(conflicts) != 1 or conflicts[0]["impact"] != "confirmed":
        raise AssertionError("expected one confirmed synthetic overlap")
    write_json(out / "migration.yaml", {"version": 1,
        "plan_id": "synthetic-operator-beta", "waves": [{"id": "wave-1",
        "name": "Synthetic replacement", "owner": "synthetic-operator"}],
        "moves": [{"subject": {"account_id": TARGET_ACCOUNT, "region": REGION,
            "vpc_id": accounts[0][2]}, "disposition": "replace", "wave": "wave-1",
            "owner": "synthetic-operator", "approval": {"approved_by": "synthetic-operator",
            "approved_at": stamp(finished), "approves": "simulation only"},
            "resolves": [conflicts[0]["id"]], "target": {"tenant_id": "developer", "allocation_key": key,
            "scope": "vpc", "environment": "development", "region": REGION,
            "account_id": TARGET_ACCOUNT, "prefix_length": 22},
            "rollback": "Synthetic exercise; no workloads or routes exist."}]})
    unknown = out / "UNKNOWN_VARIANT"
    unknown.mkdir()
    shutil.copyfile(out / "accounts.json", unknown / "accounts.json")
    with (unknown / "networks.csv").open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=columns, lineterminator="\n")
        writer.writeheader()
        writer.writerows(row for row in rows if row["account_id"] != MISSING_ACCOUNT)
    run["accounts"][-1]["regions"][0] = {"region": REGION, "outcome": "failed",
        "row_count": 0, "observed_at": stamp(observed)}
    write_json(unknown / "run.json", run)
    with (unknown / "failures.csv").open("w", encoding="utf-8", newline="") as handle:
        writer = csv.writer(handle, lineterminator="\n")
        writer.writerow(("account_id", "account_name", "region", "stage", "error"))
        writer.writerow((MISSING_ACCOUNT, "synthetic-third", REGION, "describe-vpcs",
                         "AccessDenied (synthetic)"))
    report(out / "progress.json", [str(binary), "onboard", "progress", "--plan",
        str(out / "migration.yaml"), "--inventory", str(out), *common])
    address_command = ["python3", str(ROOT / "scripts/aws/address-plan.py"),
        "--matrix", str(out / "matrix.yaml"), "--protected", str(out / "protected.json"),
        "--approved-plan", str(out / "approved-plan.json"),
        "--pilot-scope", str(out / "pilot-scope.json"),
        "--migration-plan", str(out / "migration.yaml"), "--binary", str(binary)]
    address = report(out / "address.json", address_command + ["--inventory", str(out)])
    if not address["coverage_complete"] or address["proposals"][0]["candidate_cidr"] != "10.64.0.0/22":
        raise AssertionError("complete fixture did not yield the expected advisory CIDR")
    report(out / "pilot-unreviewed.json", ["python3", str(ROOT / "scripts/aws/pilot-evidence.py"),
        "--address", str(out / "address.json"), "--progress", str(out / "progress.json"),
        "--assessment", str(out / "assessment.json"), "--run", str(out / "run.json"),
        "--networks", str(out / "networks.csv"), "--review-template", str(out / "review-template.json")],
        expected=(0,))
    unknown_assessment = report(out / "unknown-assessment.json", [str(binary), "onboard",
        "assess", "--inventory", str(unknown), *common])
    unknown_address = report(out / "unknown-address.json", address_command +
                             ["--inventory", str(unknown)])
    if unknown_assessment["coverage"]["complete"] or unknown_address["coverage_complete"] or any(
            item["candidate_cidr"] is not None for item in unknown_address["proposals"]):
        raise AssertionError("missing synthetic cell was not withheld as UNKNOWN")
    print(json.dumps({"status": "GENERATED", "source": "synthetic-fixture",
        "confirmed_conflicts": 1, "proposed_cidr": "10.64.0.0/22",
        "incomplete_coverage_refused": True, "out": str(out)}, sort_keys=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--key", required=True, help="unique stable allocation key for this exercise")
    parser.add_argument("--binary", type=Path, required=True,
                        help="platform-ipam binary built from the source being qualified")
    args = parser.parse_args()
    if not args.key.strip():
        parser.error("--key must be nonempty")
    generate(args.out, args.key, args.binary.resolve(), datetime.now(timezone.utc))


if __name__ == "__main__":
    main()
