#!/usr/bin/env python3
"""Produce deterministic, advisory CIDR candidates from a reviewed pilot scope.

This command never contacts AWS, NetBox, the allocation API, or AWS IPAM. A
candidate is free only relative to the supplied inventory, protected ranges,
and other candidates in this report. It is not a reservation.
"""

import argparse
import csv
import hashlib
import ipaddress
import json
from pathlib import Path
import subprocess
import sys


def ipv4_prefix(value):
    network = ipaddress.ip_network(value, strict=True)
    if network.version != 4:
        raise ValueError(f"IPv6 is outside this pilot contract: {value}")
    return network


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_json(path):
    with path.open(encoding="utf-8") as handle:
        return json.load(handle)


def read_inventory(path):
    with path.open(newline="", encoding="utf-8-sig") as handle:
        reader = csv.DictReader(handle)
        required = {"account_id", "region", "type", "resource_id", "cidr", "primary"}
        if not required.issubset(reader.fieldnames or []):
            raise ValueError(f"{path}: missing inventory columns {sorted(required - set(reader.fieldnames or []))}")
        rows = list(reader)
    occupied = []
    primary = {}
    for number, row in enumerate(rows, 2):
        network = ipv4_prefix(row["cidr"])
        if row["type"] not in ("vpc", "subnet"):
            raise ValueError(f"{path}:{number}: unsupported resource type")
        occupied.append(network)
        if row["type"] == "vpc" and row["primary"].lower() == "true":
            key = (row["account_id"], row["region"], row["resource_id"])
            primary.setdefault(key, []).append(network.prefixlen)
    return occupied, primary


def read_protected(path):
    value = read_json(path)
    if not isinstance(value, list):
        raise ValueError("protected ranges must be a JSON array in the existing --fixed format")
    return [ipv4_prefix(row["cidr"]) for row in value]


def read_scope(path, accounts_path):
    value = read_json(path)
    if not isinstance(value, dict) or value.get("version") != 1 or not isinstance(value.get("owner"), str) or not value["owner"].strip():
        raise ValueError("pilot scope needs version 1 and a named owner")
    selected = value.get("accounts")
    if not isinstance(selected, list) or not selected or any(not isinstance(account, str) or len(account) != 12 or not account.isdigit() for account in selected):
        raise ValueError("pilot scope accounts must be a nonempty list of quoted 12-digit IDs")
    if len(set(selected)) != len(selected):
        raise ValueError("pilot scope repeats an account")
    regions = value.get("regions", [])
    if not isinstance(regions, list) or any(not isinstance(region, str) or not region for region in regions):
        raise ValueError("pilot scope regions must be a string list")
    known = {entry["Id"] for entry in read_json(accounts_path)["Accounts"]
             if entry.get("Status") == "ACTIVE"}
    if not set(selected).issubset(known):
        raise ValueError("pilot scope names an account missing or inactive in accounts.json")
    return {"owner": value["owner"], "accounts": sorted(selected), "regions": sorted(set(regions))}


def read_plan(path):
    value = read_json(path)
    if not isinstance(value, dict) or value.get("version") != 1 or not isinstance(value.get("pools"), list):
        raise ValueError("approved address plan must have version 1 and a pools array")
    pools = []
    seen = set()
    for row in value["pools"]:
        if not isinstance(row, dict) or not isinstance(row.get("id"), str) or not row["id"]:
            raise ValueError("every pool needs a nonempty id")
        if row["id"] in seen:
            raise ValueError(f"duplicate pool id {row['id']}")
        seen.add(row["id"])
        authority = row.get("authority")
        if authority not in ("platform-ipam", "aws-ipam"):
            raise ValueError(f"pool {row['id']}: authority must be platform-ipam or aws-ipam")
        network = ipv4_prefix(row["cidr"])
        lengths = row.get("allowed_prefix_lengths")
        if not isinstance(lengths, list) or not lengths or any(type(n) is not int or n < network.prefixlen or n > 32 for n in lengths):
            raise ValueError(f"pool {row['id']}: invalid allowed_prefix_lengths")
        accounts = row.get("eligible_accounts", [])
        if not isinstance(accounts, list) or any(not isinstance(a, str) for a in accounts):
            raise ValueError(f"pool {row['id']}: eligible_accounts must be a string list")
        if not isinstance(row.get("region"), str) or not row["region"]:
            raise ValueError(f"pool {row['id']}: region is required")
        excluded = row.get("excluded_cidrs", [])
        if not isinstance(excluded, list):
            raise ValueError(f"pool {row['id']}: excluded_cidrs must be a list")
        exclusions = [ipv4_prefix(raw) for raw in excluded]
        if any(not item.subnet_of(network) for item in exclusions):
            raise ValueError(f"pool {row['id']}: an excluded CIDR lies outside the pool")
        pools.append({"id": row["id"], "cidr": network, "authority": authority,
                      "region": row["region"], "allowed_prefix_lengths": lengths,
                      "eligible_accounts": accounts, "excluded_cidrs": exclusions})
    for index, left in enumerate(pools):
        for right in pools[index + 1:]:
            if left["cidr"].overlaps(right["cidr"]):
                raise ValueError(f"approved pools {left['id']} and {right['id']} overlap; one authority per range is required")
    return sorted(pools, key=lambda pool: pool["id"])


def first_fit(pool, prefix_length, occupied):
    size = 1 << (32 - prefix_length)
    start = int(pool.network_address)
    end = int(pool.broadcast_address)
    blocked = sorted((int(item.network_address), int(item.broadcast_address))
                     for item in occupied if item.overlaps(pool))
    while start + size - 1 <= end:
        candidate_end = start + size - 1
        overlap = next(((low, high) for low, high in blocked
                        if low <= candidate_end and high >= start), None)
        if overlap is None:
            return ipaddress.ip_network((start, prefix_length))
        start = ((overlap[1] + 1 + size - 1) // size) * size
    return None


def assess(binary, inventory, matrix, protected):
    command = [str(binary), "onboard", "assess", "--inventory", str(inventory),
               "--matrix", str(matrix), "--fixed", str(protected), "--format", "json"]
    result = subprocess.run(command, capture_output=True, text=True, timeout=120, check=False)
    if result.returncode not in (0, 3):
        raise ValueError(result.stderr.strip() or "overlap assessment produced no report")
    report = json.loads(result.stdout)
    if report.get("report_version") != 1 or not isinstance(report.get("conflicts"), list):
        raise ValueError("unsupported overlap assessment report")
    return report


def reviewed_targets(binary, migration_plan, inventory, matrix, protected):
    """Use the existing strict migration-plan decoder, not a second YAML contract."""
    command = [str(binary), "onboard", "progress", "--plan", str(migration_plan),
               "--inventory", str(inventory), "--matrix", str(matrix),
               "--fixed", str(protected), "--format", "json"]
    result = subprocess.run(command, capture_output=True, text=True, timeout=120, check=False)
    if result.returncode not in (0, 3):
        raise ValueError(result.stderr.strip() or "migration progress produced no report")
    report = json.loads(result.stdout)
    if report.get("report_version") != 1 or not isinstance(report.get("moves"), list):
        raise ValueError("unsupported migration progress report")
    return {(move["account_id"], move["region"], move["vpc_id"]): move.get("target") or {}
            for move in report["moves"] if move.get("disposition") == "replace"}


def plan(assessment, inventory_path, matrix_path, protected_path, plan_path, scope_path,
         targets=None, migration_plan_path=None):
    known_inputs = {item["sha256"] for item in assessment["inputs"]}
    for path in (inventory_path, matrix_path, protected_path):
        if digest(path) not in known_inputs:
            raise ValueError(f"{path}: bytes do not match the assessment input")
    occupied, primary = read_inventory(inventory_path)
    occupied.extend(read_protected(protected_path))
    pools = read_plan(plan_path)
    scope = read_scope(scope_path, inventory_path.parent / "accounts.json")
    in_scope = lambda side: (not side.get("fixed") and
                             side.get("account_id") in scope["accounts"] and
                             (not scope["regions"] or side.get("region") in scope["regions"]))
    for pool in pools:
        occupied.extend(pool["excluded_cidrs"])
    conflicts = assessment["conflicts"]
    touching = [item for item in conflicts if any(in_scope(side) for side in item["sides"])]
    confirmed = [item for item in touching if item.get("impact") == "confirmed"]
    unknown = [item for item in touching if item.get("impact") == "unknown" or
               item.get("impact") == "potential" and item.get("matrix_relation") != "must_stay_isolated"]
    coverage = assessment["coverage"]
    input_limits = assessment.get("input_limits", [])
    if confirmed:
        address_state = "CIDR_BLOCKED"
    elif not coverage.get("complete") or unknown or input_limits:
        address_state = "UNKNOWN"
    else:
        address_state = "CIDR_READY"

    subjects = {}
    for conflict in confirmed:
        for side in conflict["sides"]:
            if not in_scope(side):
                continue
            key = (side["account_id"], side["region"], side["vpc_id"])
            subjects.setdefault(key, set()).add(conflict["id"])

    targets = targets or {}
    proposals = []
    # A selected move claims capacity before an unselected alternative does.
    for key in sorted(subjects, key=lambda subject: (subject not in targets, subject)):
        ids = subjects[key]
        account, region, vpc = key
        item = {"account_id": account, "region": region, "vpc_id": vpc,
                "conflict_ids": sorted(ids), "candidate_cidr": None,
                "pool_id": None, "authority": None, "status": "NEEDS_REVIEW"}
        if key in targets:
            target = targets[key]
            target_account = target.get("account_id")
            target_region = target.get("region")
            length = target.get("prefix_length")
            if (not isinstance(target_account, str) or len(target_account) != 12 or
                    not target_account.isdigit() or not isinstance(target_region, str) or
                    not target_region or type(length) is not int or not 0 <= length <= 32):
                item["reason"] = "reviewed replacement target needs account, region and prefix length"
                proposals.append(item)
                continue
            item["reviewed_target"] = True
        else:
            lengths = primary.get(key, [])
            if len(lengths) != 1:
                item["reason"] = "exactly one observed primary VPC prefix length is required"
                proposals.append(item)
                continue
            target_account, target_region, length = account, region, lengths[0]
            item["reviewed_target"] = False
        item["target_account_id"] = target_account
        item["target_region"] = target_region
        item["prefix_length"] = length
        eligible = [pool for pool in pools if pool["region"] == target_region and
                    length in pool["allowed_prefix_lengths"] and
                    (not pool["eligible_accounts"] or target_account in pool["eligible_accounts"])]
        if len(eligible) != 1:
            item["reason"] = f"expected one eligible approved pool, found {len(eligible)}"
            proposals.append(item)
            continue
        pool = eligible[0]
        item["pool_id"] = pool["id"]
        item["authority"] = pool["authority"]
        if not coverage.get("complete") or input_limits:
            item["status"] = "INSUFFICIENT_COVERAGE"
            item["reason"] = "complete inventory coverage and input evidence are required before suggesting a candidate"
        elif pool["authority"] == "aws-ipam":
            item["status"] = "REQUEST_FROM_AWS_IPAM"
            item["reason"] = "AWS IPAM is the sole allocation authority; no local CIDR is selected"
        else:
            candidate = first_fit(pool["cidr"], length, occupied)
            if candidate is None:
                item["reason"] = "no candidate fits the approved pool and observed exclusions"
            else:
                item["candidate_cidr"] = str(candidate)
                item["status"] = "ADVISORY_CANDIDATE"
                item["reason"] = "free only against this report's inventory, protected ranges and other candidates"
                occupied.append(candidate)
        proposals.append(item)

    input_hashes = {"networks": digest(inventory_path), "matrix": digest(matrix_path),
                    "protected": digest(protected_path), "approved_plan": digest(plan_path),
                    "pilot_scope": digest(scope_path)}
    if migration_plan_path:
        input_hashes["migration_plan"] = digest(migration_plan_path)
    return {
        "version": 1,
        "address_readiness": address_state,
        "network_readiness": "NOT_ASSESSED",
        "pilot_scope": scope,
        "scope_inputs": {
            "versions": {"pilot_scope": read_json(scope_path)["version"],
                         "approved_plan": read_json(plan_path)["version"]},
            "protected_ranges": read_json(protected_path),
            "approved_pools": read_json(plan_path)["pools"],
        },
        "coverage_complete": bool(coverage.get("complete")),
        "confirmed_conflicts": len(confirmed),
        "unresolved_intent_conflicts": len(unknown),
        "input_limits": input_limits,
        "conflicts": confirmed,
        "proposals": proposals,
        "input_sha256": input_hashes,
        "notes": ["All proposals are alternatives for owner review, not a decision to move every VPC.",
                  "Candidates are not reservations; current ledger, NetBox and AWS IPAM authority must be checked at execution.",
                  "No TGW route, security, DNS or traffic test contributes to network readiness."],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--matrix", type=Path, required=True)
    parser.add_argument("--protected", type=Path, required=True,
                        help="JSON array in the existing --fixed format")
    parser.add_argument("--approved-plan", type=Path, required=True,
                        help="JSON version 1 pools with exactly one authority per pool")
    parser.add_argument("--pilot-scope", type=Path, required=True,
                        help="JSON version 1 named owner and account/region scope")
    parser.add_argument("--migration-plan", type=Path,
                        help="optional reviewed migration.yaml; selected replacements use target account, region and size")
    parser.add_argument("--binary", default="platform-ipam")
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    try:
        report = assess(args.binary, args.inventory, args.matrix, args.protected)
        targets = (reviewed_targets(args.binary, args.migration_plan, args.inventory,
                                    args.matrix, args.protected) if args.migration_plan else None)
        result = plan(report, args.inventory / "networks.csv", args.matrix,
                      args.protected, args.approved_plan, args.pilot_scope,
                      targets, args.migration_plan)
        body = json.dumps(result, sort_keys=True, indent=2) + "\n"
        if args.out:
            args.out.write_text(body, encoding="utf-8")
        sys.stdout.write(body)
    except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
        print(f"address plan: {error}", file=sys.stderr)
        return 4
    return 0 if result["address_readiness"] == "CIDR_READY" else 3


if __name__ == "__main__":
    sys.exit(main())
