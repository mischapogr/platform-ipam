#!/usr/bin/env python3
"""Generate synthetic, collector-shaped 100- or 500-account workspace demos."""

import argparse
import csv
import ipaddress
import json
from pathlib import Path
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
OUT = Path(__file__).resolve().parent / "HUNDRED_ACCOUNT_DEMO"
REGIONS = ("eu-central-1", "eu-west-1", "us-east-1")
OBSERVED = "2026-09-24T10:00:00Z"
COLUMNS = ("account_id", "account_name", "region", "type", "resource_id", "cidr",
           "parent_id", "az_id", "name", "state", "primary", "association_id", "observed_at")
PAIR_CASES = (
    (0, 1, "eu-central-1", "10.20.0.0/22", "10.20.0.0/22", "must_communicate"),
    (2, 3, "eu-west-1", "10.32.0.0/22", "10.32.1.0/24", "must_communicate"),
    (4, 5, "us-east-1", "10.42.0.0/22", "10.42.0.0/22", "must_communicate"),
    (6, 7, "eu-central-1", "172.20.0.0/22", "172.20.1.0/24", "must_communicate"),
    (8, 9, "eu-west-1", "10.50.0.0/22", "10.50.0.0/22", "must_communicate"),
    (10, 11, "us-east-1", "10.60.0.0/22", "10.60.0.0/22", "must_stay_isolated"),
    (12, 13, "eu-central-1", "10.70.0.0/22", "10.70.0.0/22", "must_stay_isolated"),
)


def write_json(name, value):
    (OUT / name).write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def account(index):
    return f"{100000000000 + index:012d}"


def portable(path):
    try:
        return str(path.relative_to(ROOT))
    except ValueError:
        return str(path)


def vpc(index, region_index):
    return f"vpc-{index * len(REGIONS) + region_index + 1:017x}"


def write_csv(name, rows, columns):
    with (OUT / name).open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=columns)
        writer.writeheader()
        writer.writerows(rows)


def run(binary, *args):
    result = subprocess.run([str(binary), "onboard", *args], cwd=ROOT,
                            capture_output=True, text=True, check=False)
    if result.returncode not in (0, 3):
        raise RuntimeError(f"onboard {' '.join(args[:1])}: {result.stderr.strip()}")
    return json.loads(result.stdout)


def generate(binary, account_count=100, output=None):
    global OUT
    if not 100 <= account_count <= 500:
        raise ValueError("account_count must be between 100 and 500")
    folder = ("HUNDRED_ACCOUNT_DEMO" if account_count == 100 else
              "FIVE_HUNDRED_ACCOUNT_DEMO" if account_count == 500 else
              f"{account_count}_ACCOUNT_DEMO")
    OUT = output.resolve() if output else Path(__file__).resolve().parent / folder
    OUT.mkdir(parents=True, exist_ok=True)
    extra_pairs = tuple((100 + 2 * i, 101 + 2 * i, REGIONS[i % len(REGIONS)],
                         f"172.21.{4 * i}.0/22",
                         f"172.21.{4 * i}.0/22" if i % 2 == 0 else f"172.21.{4 * i + 1}.0/24",
                         "must_communicate" if i < 20 else "must_stay_isolated")
                        for i in range((account_count - 100) * 25 // 400))
    pair_cases = PAIR_CASES + extra_pairs
    overrides = {}
    for left, right, region, left_cidr, right_cidr, _ in pair_cases:
        overrides[left, region] = left_cidr
        overrides[right, region] = right_cidr
    rows = []
    run_accounts = []
    accounts = []
    for index in range(account_count):
        aid = account(index)
        name = f"{'prod' if index % 2 == 0 else 'nonprod'}-application-{index + 1:03d}"
        accounts.append({"Id": aid, "Name": name, "Status": "ACTIVE"})
        cells = []
        for region_index, region in enumerate(REGIONS):
            number = index * len(REGIONS) + region_index
            cidr = overrides.get((index, region),
                                 str(ipaddress.ip_network((f"10.{100 + number // 64}.{number % 64 * 4}.0", 22))))
            vid = vpc(index, region_index)
            rows.append(dict(zip(COLUMNS, (aid, name, region, "vpc", vid, cidr, "", "",
                                           f"{name}-{region}", "available", "true",
                                           f"vpc-cidr-assoc-{number + 1:017x}", OBSERVED))))
            row_count = 1
            if region_index == 0 and index % 4 == 0:
                subnet = next(ipaddress.ip_network(cidr).subnets(new_prefix=ipaddress.ip_network(cidr).prefixlen + 2))
                rows.append(dict(zip(COLUMNS, (aid, name, region, "subnet", f"subnet-{number + 1:017x}",
                                               str(subnet), vid, region + "a", f"{name}-app-a",
                                               "available", "", "", OBSERVED))))
                row_count += 1
            if region_index == 0 and 20 <= index < 25:
                rows.append(dict(zip(COLUMNS, (aid, name, region, "vpc", vid,
                                               f"172.25.{index}.0/24", "", "", f"{name}-secondary",
                                               "available", "false", f"vpc-cidr-assoc-{400 + index:017x}", OBSERVED))))
                row_count += 1
            cells.append({"region": region, "outcome": "succeeded", "row_count": row_count,
                          "observed_at": OBSERVED})
        run_accounts.append({"account_id": aid, "account_name": name,
                             "credential_source": "assumed-role", "regions_attempted": "known",
                             "region_source": "configured", "regions": cells})
    write_csv("networks.csv", rows, COLUMNS)
    write_csv("failures.csv", [], ("account_id", "account_name", "region", "stage", "error"))
    write_json("accounts.json", {"Accounts": accounts})
    write_json("run.json", {"script_version": "2", "started_at": OBSERVED,
                            "finished_at": "2026-09-24T10:05:00Z",
                            "configured_regions": list(REGIONS), "accounts": run_accounts})
    groups = [{"id": f"account-{i:03d}", "members": [account(i)]}
              for i in sorted({index for pair in pair_cases for index in pair[:2]})]
    matrix = {"version": 1, "groups": groups,
              "must_communicate": [[f"account-{a:03d}", f"account-{b:03d}"]
                                   for a, b, _, _, _, relation in pair_cases if relation == "must_communicate"],
              "must_stay_isolated": [[f"account-{a:03d}", f"account-{b:03d}"]
                                     for a, b, _, _, _, relation in pair_cases if relation == "must_stay_isolated"],
              "shared_services": []}
    write_json("matrix.yaml", matrix)
    write_json("ownership.yaml", [{"account_id": account(i), "product": f"application-{i + 1:03d}",
                                   "environment": "prod" if i % 2 == 0 else "nonprod",
                                   "owner": f"application-{i + 1:03d}-team"} for i in range(account_count)])
    write_json("fixed.yaml", [{"cidr": f"10.{octet}.0.0/22", "description": "protected external route",
                                "owner": "corporate-network"} for octet in (64, 65, 66)])
    write_json("approved-plan.json", {"version": 1, "pools": [
        {"id": f"replacement-{region}", "cidr": f"10.{64 + i}.0.0/16",
         "region": region, "authority": "platform-ipam", "allowed_prefix_lengths": [22, 24]}
        for i, region in enumerate(REGIONS)]})
    write_json("pilot-scope.json", {"version": 1, "owner": "corporate-network-pilot",
                                    "accounts": [account(i) for i in range(account_count)], "regions": list(REGIONS)})
    assessment = run(binary, "assess", "--inventory", portable(OUT),
                     "--matrix", portable(OUT / "matrix.yaml"), "--fixed", portable(OUT / "fixed.yaml"),
                     "--ownership", portable(OUT / "ownership.yaml"), "--format", "json")
    conflicts = assessment["conflicts"]
    assert len(conflicts) == len(pair_cases), len(conflicts)
    assert sum(c["impact"] == "confirmed" for c in conflicts) == sum(p[5] == "must_communicate" for p in pair_cases)
    assert sum(c["impact"] == "potential" for c in conflicts) == sum(p[5] == "must_stay_isolated" for p in pair_cases)
    moves = []
    for pair_index, (left, right, region, _, right_cidr, relation) in enumerate(
            p for p in pair_cases if p[5] == "must_communicate"):
        conflict = next(c for c in conflicts if {side["account_id"] for side in c["sides"]}
                        == {account(left), account(right)})
        target = {"tenant_id": "developer", "allocation_key": f"demo-replacement-{pair_index + 1:02d}",
                  "scope": "vpc", "environment": "development", "region": region,
                  "account_id": account(right),
                  "prefix_length": ipaddress.ip_network(right_cidr).prefixlen}
        moves.append({"subject": {"account_id": account(right), "region": region,
                                   "vpc_id": vpc(right, REGIONS.index(region))},
                      "disposition": "replace", "wave": "wave-1", "owner": f"application-{right + 1:03d}-team",
                      "approval": {"approved_by": "synthetic-demo-owner", "approved_at": OBSERVED,
                                   "approves": "replace for synthetic demo only"},
                      "resolves": [conflict["id"]], "target": target,
                      "rollback": "Customer workload owner must design and validate rollback."})
    write_json("migration.yaml", {"version": 1, "plan_id": f"synthetic-{account_count}-account-demo",
                                  "waves": [{"id": "wave-1", "name": "Example replacement wave",
                                             "owner": "corporate-network-pilot"}], "moves": moves})
    # A second input combination intentionally loses one requested account/region observation.
    variant = OUT / "UNKNOWN_VARIANT"
    variant.mkdir(exist_ok=True)
    unknown_matrix = json.loads((OUT / "matrix.yaml").read_text())
    unknown_pair = next(p for p in reversed(pair_cases) if p[5] == "must_stay_isolated")
    unknown_matrix["must_stay_isolated"].remove([f"account-{unknown_pair[0]:03d}", f"account-{unknown_pair[1]:03d}"])
    unknown_matrix["groups"] = [group for group in unknown_matrix["groups"]
                                if group["id"] not in (f"account-{unknown_pair[0]:03d}", f"account-{unknown_pair[1]:03d}")]
    (variant / "matrix.yaml").write_text(json.dumps(unknown_matrix, indent=2) + "\n")
    missing_account, missing_region = account(account_count - 1), "us-east-1"
    write_variant_rows = [row for row in rows if (row["account_id"], row["region"]) != (missing_account, missing_region)]
    with (variant / "networks.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=COLUMNS); writer.writeheader(); writer.writerows(write_variant_rows)
    variant_run = json.loads((OUT / "run.json").read_text())
    variant_run["accounts"][-1]["regions"][-1] = {"region": missing_region, "outcome": "failed",
                                                     "row_count": 0, "observed_at": OBSERVED}
    (variant / "run.json").write_text(json.dumps(variant_run, indent=2) + "\n")
    with (variant / "failures.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle); writer.writerow(("account_id", "account_name", "region", "stage", "error"))
        writer.writerow((missing_account, accounts[-1]["Name"], missing_region, "describe-vpcs", "AccessDenied (synthetic)"))
    variant_assessment = run(binary, "assess", "--networks", portable(variant / "networks.csv"),
                             "--accounts", portable(OUT / "accounts.json"),
                             "--failures", portable(variant / "failures.csv"),
                             "--run", portable(variant / "run.json"),
                             "--matrix", portable(variant / "matrix.yaml"),
                             "--fixed", portable(OUT / "fixed.yaml"),
                             "--ownership", portable(OUT / "ownership.yaml"), "--format", "json")
    assert not variant_assessment["coverage"]["complete"]
    assert [sum(c["impact"] == impact for c in variant_assessment["conflicts"])
            for impact in ("confirmed", "potential", "unknown")] == [
                sum(p[5] == "must_communicate" for p in pair_cases),
                sum(p[5] == "must_stay_isolated" for p in pair_cases) - 1, 1]
    with tempfile.TemporaryDirectory() as temporary:
        temp = Path(temporary)
        address = subprocess.run([
            sys.executable, str(ROOT / "scripts/aws/address-plan.py"),
            "--inventory", portable(OUT), "--matrix", portable(OUT / "matrix.yaml"),
            "--protected", portable(OUT / "fixed.yaml"), "--approved-plan", portable(OUT / "approved-plan.json"),
            "--pilot-scope", portable(OUT / "pilot-scope.json"),
            "--migration-plan", portable(OUT / "migration.yaml"), "--binary", str(binary),
        ], cwd=ROOT, capture_output=True, text=True, check=False)
        if address.returncode not in (0, 3):
            raise RuntimeError(address.stderr)
        (temp / "address.json").write_text(address.stdout)
        common = ("--inventory", portable(OUT), "--matrix", portable(OUT / "matrix.yaml"),
                  "--fixed", portable(OUT / "fixed.yaml"), "--ownership", portable(OUT / "ownership.yaml"),
                  "--format", "json")
        (temp / "progress.json").write_text(json.dumps(run(binary, "progress", "--plan",
            portable(OUT / "migration.yaml"), *common), sort_keys=True, indent=2) + "\n")
        (temp / "assessment.json").write_text(json.dumps(run(binary, "assess", *common),
            sort_keys=True, indent=2) + "\n")
        overview_command = [
            sys.executable, str(ROOT / "scripts/aws/pilot-evidence.py"),
            "--address", str(temp / "address.json"), "--progress", str(temp / "progress.json"),
            "--assessment", str(temp / "assessment.json"), "--run", portable(OUT / "run.json"),
            "--networks", portable(OUT / "networks.csv"),
        ]
        overview = subprocess.run(overview_command, cwd=ROOT, capture_output=True, text=True, check=False)
        if overview.returncode:
            raise RuntimeError(overview.stderr)
        base_report = json.loads(overview.stdout)
        write_json("execution.json", {"version": 1, "input_sha256": base_report["input_sha256"],
                                      "moves": [{"allocation_key": move["target"]["allocation_key"],
                                                 "status": "PLANNED", "commit": "synthetic-demo",
                                                 "run": f"synthetic-plan-{i + 1:03d}"}
                                                for i, move in enumerate(moves)]})
        with_execution = subprocess.run(overview_command + ["--execution", portable(OUT / "execution.json")],
                                        cwd=ROOT, capture_output=True, text=True, check=False)
        if with_execution.returncode:
            raise RuntimeError(with_execution.stderr)
        (OUT / "pilot-overview.json").write_text(with_execution.stdout)
    print(f"Wrote {len(accounts)} accounts, {account_count * len(REGIONS)} VPCs, {(account_count + 3) // 4} subnets, 5 secondary VPC CIDRs, {len(conflicts)} overlaps and {len(moves)} reviewed moves to {OUT}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", nargs="?", type=Path, default=ROOT / "bin/platform-ipam")
    parser.add_argument("--accounts", type=int, default=100, help="100 through 500")
    parser.add_argument("--output", type=Path, help="output directory (defaults to a folder beside this script)")
    args = parser.parse_args()
    generate(args.binary.resolve(), args.accounts, args.output)
