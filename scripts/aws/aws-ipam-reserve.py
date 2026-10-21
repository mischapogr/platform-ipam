#!/usr/bin/env python3
"""Hand reviewed pilot moves in AWS-IPAM-owned pools to AWS IPAM for allocation.

Without --execute this command only prints the requests it would send. With
--execute it allocates one custom CIDR per reviewed move, the only AWS write
this repository makes. Each request carries a stable identity in its
description and a stable client token, so a rerun adopts what a previous run
allocated instead of allocating again. Nothing is ever released: a rejected
response is reported with the release command for an operator to review.

The output is an operator journal, not evidence. Verify the allocations with
verify-reservations.py --aws-ipam, which reads AWS IPAM again.
"""

import argparse
import csv
from datetime import datetime, timezone
import importlib.util
import ipaddress
import json
from pathlib import Path
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import aws_ipam  # noqa: E402

_spec = importlib.util.spec_from_file_location("verify_reservations", HERE / "verify-reservations.py")
verifier = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(verifier)


def plan_requests(pilot, approved, protected, observed_cidrs, now, maximum_age_seconds):
    """Validate the reviewed pilot and derive one request per AWS IPAM move."""
    if pilot.get("version") != 1 or approved.get("version") != 1:
        raise ValueError("unsupported pilot or approved plan version")
    if not pilot["discovery"]["complete"] or pilot["pilot_gates"]["planning"] != "PASS":
        raise ValueError("complete discovery and a passing planning gate are required")
    age = (now - verifier.instant(pilot["discovery"]["oldest_observed_at"])).total_seconds()
    if age > maximum_age_seconds:
        raise ValueError(f"inventory is stale for allocation ({int(age)} seconds); rerun discovery")
    pools = {row["id"]: row for row in approved["pools"]}
    moves = pilot["replacements"]["moves"]
    held = [cidr for cidr in (move.get("proposed_cidr") for move in moves) if cidr]
    known = ([row["cidr"] for row in protected] + list(observed_cidrs) + held)
    requests = []
    seen = set()
    for move in moves:
        if move.get("authority") != "aws-ipam":
            continue
        subject = move["subject"]
        pool = pools.get(move.get("pool_id"))
        target = move.get("target") or {}
        if not move.get("approved"):
            raise ValueError(f"{subject}: move approval is missing")
        if move.get("proposal_status") != "REQUEST_FROM_AWS_IPAM" or not move.get("proposal_target_match"):
            raise ValueError(f"{subject}: the reviewed proposal is not an AWS IPAM request for this target")
        if not pool or pool.get("authority") != "aws-ipam":
            raise ValueError(f"{subject}: the approved pool is not AWS-IPAM-owned")
        aws_pool_id = pool.get("aws_ipam_pool_id")
        if not isinstance(aws_pool_id, str) or not aws_ipam.POOL_ID.match(aws_pool_id):
            raise ValueError(f"pool {pool['id']}: aws_ipam_pool_id is required")
        length = target.get("prefix_length")
        if length not in pool.get("allowed_prefix_lengths", []):
            raise ValueError(f"{subject}: prefix length is not approved for the pool")
        if pool.get("region") != target.get("region"):
            raise ValueError(f"{subject}: pool region differs from the reviewed target")
        eligible = pool.get("eligible_accounts") or []
        if eligible and target.get("account_id") not in eligible:
            raise ValueError(f"{subject}: account is not eligible for the approved pool")
        key = (target.get("tenant_id"), target.get("allocation_key"))
        if key in seen:
            raise ValueError(f"{subject}: request identity repeats another move")
        seen.add(key)
        requests.append({
            "subject": subject, "tenant_id": key[0], "allocation_key": key[1],
            "pool_id": pool["id"], "pool_cidr": pool["cidr"], "aws_ipam_pool_id": aws_pool_id,
            "region": pool["region"], "prefix_length": length,
            "identity": aws_ipam.identity(*key),
            "client_token": aws_ipam.client_token(aws_pool_id, key[0], key[1], length),
            "disallowed_cidrs": aws_ipam.relevant(pool["cidr"], known + pool.get("excluded_cidrs", [])),
        })
    if not requests:
        raise ValueError("no reviewed move uses an AWS-IPAM-owned pool")
    return requests


def check_response(request, allocation, taken):
    reasons = []
    try:
        returned = verifier.ipv4(allocation.get("Cidr"))
    except (ValueError, TypeError):
        return None, ["AWS IPAM returned no valid IPv4 CIDR"]
    if allocation.get("Description") != request["identity"]:
        reasons.append("allocation description differs from the request identity")
    if returned.prefixlen != request["prefix_length"]:
        reasons.append("returned CIDR size differs from the reviewed target")
    if not returned.subnet_of(ipaddress.ip_network(request["pool_cidr"])):
        reasons.append("returned CIDR lies outside the approved pool")
    if any(returned.overlaps(ipaddress.ip_network(item)) for item in request["disallowed_cidrs"]):
        reasons.append("returned CIDR overlaps protected, observed, excluded or proposed space")
    if any(returned.overlaps(item) for item in taken):
        reasons.append("returned CIDR overlaps another request in this run")
    return returned, reasons


def execute(requests, runner=subprocess.run, attempts=3, pause=2.0):
    results = []
    taken = []
    for request in requests:
        result = {key: request[key] for key in ("subject", "tenant_id", "allocation_key", "pool_id",
                                                "aws_ipam_pool_id", "region", "prefix_length",
                                                "identity", "client_token")}
        result.update({"allocation_id": None, "cidr": None, "reasons": []})
        allocation = None
        try:
            existing = aws_ipam.owned_rows(
                aws_ipam.pool_allocations(request["aws_ipam_pool_id"], request["region"], runner),
                request["aws_ipam_pool_id"]).get((request["tenant_id"], request["allocation_key"]), [])
            if len(existing) > 1:
                result["status"] = "REJECTED"
                result["reasons"].append("AWS IPAM already holds more than one allocation for this identity")
                result["release_candidates"] = [row["id"] for row in existing]
                results.append(result)
                continue
            if existing:
                result["status"] = "ADOPTED"
                allocation = {"IpamPoolAllocationId": existing[0]["id"], "Cidr": existing[0]["cidr"],
                              "Description": existing[0]["description"]}
            else:
                for attempt in range(1, attempts + 1):
                    try:
                        allocation = aws_ipam.allocate(
                            request["aws_ipam_pool_id"], request["region"], request["prefix_length"],
                            request["client_token"], request["identity"], request["disallowed_cidrs"], runner)
                        break
                    except aws_ipam.AwsCallError as error:
                        if not error.transient or attempt == attempts:
                            raise
                        time.sleep(pause * attempt)
                result["status"] = "ALLOCATED"
        except aws_ipam.AwsCallError as error:
            # The write may have reached AWS. The same identity and token make
            # a rerun safe; do not guess a result here.
            result["status"] = "UNKNOWN_OUTCOME"
            result["reasons"].append(str(error))
            if "IdempotentParameterMismatch" in str(error):
                result["reasons"].append("this identity was sent before with different parameters; wait for "
                                         "the allocation to appear in AWS IPAM and rerun to adopt it")
            results.append(result)
            continue
        result["allocation_id"] = allocation.get("IpamPoolAllocationId")
        returned, reasons = check_response(request, allocation, taken)
        result["cidr"] = str(returned) if returned else allocation.get("Cidr")
        if reasons:
            result["status"] = "REJECTED"
            result["reasons"] = reasons
            result["release_command"] = (
                f"aws ec2 release-ipam-pool-allocation --region {request['region']} "
                f"--ipam-pool-id {request['aws_ipam_pool_id']} "
                f"--ipam-pool-allocation-id {result['allocation_id']} --cidr {result['cidr']}")
        else:
            taken.append(returned)
        results.append(result)
    return results


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--pilot", type=Path, required=True)
    parser.add_argument("--approved-plan", type=Path, required=True)
    parser.add_argument("--protected", type=Path, required=True)
    parser.add_argument("--networks", type=Path, required=True)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--max-inventory-age-seconds", type=int, required=True)
    parser.add_argument("--execute", action="store_true", help="allocate in AWS IPAM; without it nothing is sent")
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    try:
        if args.max_inventory_age_seconds <= 0:
            raise ValueError("max inventory age must be positive")
        pilot = verifier.read_json(args.pilot)
        hashes = pilot["input_sha256"]
        for name, path in (("approved_plan", args.approved_plan), ("protected", args.protected),
                           ("networks", args.networks), ("run", args.run)):
            if hashes[name] != verifier.digest(path):
                raise ValueError(f"{path}: bytes differ from the pilot evidence")
        approved = verifier.read_json(args.approved_plan)
        protected = verifier.read_json(args.protected)
        if not isinstance(protected, list):
            raise ValueError("protected ranges must be an array")
        with args.networks.open(newline="", encoding="utf-8-sig") as handle:
            network_rows = list(csv.DictReader(handle))
        verifier.check_discovery_snapshot(pilot, verifier.read_json(args.run), network_rows)
        requests = plan_requests(pilot, approved, protected, [row["cidr"] for row in network_rows],
                                 datetime.now(timezone.utc), args.max_inventory_age_seconds)
        if args.execute:
            results = execute(requests)
        else:
            results = [dict(request, status="PLANNED") for request in requests]
        journal = {"version": 1, "authority": "aws-ipam", "executed": args.execute,
                   "pilot_sha256": verifier.canonical_digest(pilot),
                   "written_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
                   "note": "Operator journal, not evidence. Verify with verify-reservations.py --aws-ipam.",
                   "results": results}
        body = json.dumps(journal, sort_keys=True, indent=2) + "\n"
        if args.out:
            args.out.write_text(body, encoding="utf-8")
        sys.stdout.write(body)
    except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        print(f"aws ipam handoff: {error}", file=sys.stderr)
        return 4
    if not args.execute:
        return 0
    return 0 if all(item["status"] in ("ALLOCATED", "ADOPTED") for item in results) else 3


if __name__ == "__main__":
    sys.exit(main())
