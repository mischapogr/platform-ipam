#!/usr/bin/env python3
"""Check reviewed pilot moves against a current authenticated Platform-IPAM read.

This command is read-only. It never treats the migration-progress evidence
export as a reservation receipt because that export deliberately omits CIDR
and pool ID. AWS IPAM pools remain unverified by this command.
"""

import argparse
import csv
from datetime import datetime, timedelta, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.parse
import urllib.request


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def canonical_digest(value):
    return hashlib.sha256((json.dumps(value, sort_keys=True, indent=2) + "\n").encode()).hexdigest()


def instant(value):
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamps must carry a timezone")
    return parsed


def ipv4(value):
    network = ipaddress.ip_network(value, strict=True)
    if network.version != 4:
        raise ValueError("IPv6 is outside the pilot address contract")
    return network


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8"))


def check_discovery_snapshot(pilot, run, network_rows):
    scope = pilot["scope"]
    expected = {(account, region) for account in scope["accounts"] for region in scope["regions"]}
    if not expected or len(expected) != pilot["discovery"]["requested_cells"]:
        raise ValueError("pilot requested coverage differs from its reviewed scope")
    observed = {}
    for account in run["accounts"]:
        for cell in account.get("regions", []):
            key = (account["account_id"], cell["region"])
            if key in expected:
                if key in observed:
                    raise ValueError("run.json repeats a requested account-region cell")
                observed[key] = cell
    if set(observed) != expected or any(cell.get("outcome") != "succeeded" or not cell.get("observed_at") for cell in observed.values()):
        raise ValueError("run.json does not establish complete requested coverage")
    oldest = min(cell["observed_at"] for cell in observed.values())
    if (oldest != pilot["discovery"]["oldest_observed_at"] or
            run.get("finished_at") != pilot["discovery"]["finished_at"] or
            len(observed) != pilot["discovery"]["scanned_cells"]):
        raise ValueError("pilot discovery timing or counts differ from run.json")
    associations = [row for row in network_rows if row.get("type") == "vpc" and
                    (row.get("account_id"), row.get("region")) in expected]
    vpcs = {(row["account_id"], row["region"], row["resource_id"]) for row in associations}
    if (len(associations) != pilot["discovery"]["vpc_cidr_associations"] or
            len(vpcs) != pilot["discovery"]["vpcs"]):
        raise ValueError("pilot VPC counts differ from networks.csv")


def authority_allocations(origin, token):
    parsed = urllib.parse.urlparse(origin)
    if parsed.scheme != "https" and not (parsed.scheme == "http" and parsed.hostname in ("127.0.0.1", "localhost", "::1")):
        raise ValueError("authority URL must be HTTPS or loopback HTTP")
    if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path not in ("", "/"):
        raise ValueError("authority URL must be an origin without credentials, path or query")
    if not token:
        raise ValueError("an authority read token is required")
    rows = []
    class NoRedirect(urllib.request.HTTPRedirectHandler):
        def redirect_request(self, request, fp, code, msg, headers, newurl):
            return None

    opener = urllib.request.build_opener(NoRedirect)
    seen_ids = set()
    seen_cursors = set()
    cursor = None
    while True:
        query = {"limit": "200"}
        if cursor:
            query["cursor"] = cursor
        url = origin.rstrip("/") + "/v1/allocations?" + urllib.parse.urlencode(query)
        request = urllib.request.Request(url, headers={"Authorization": "Bearer " + token,
                                                       "Accept": "application/json"})
        try:
            with opener.open(request, timeout=20) as response:
                document = json.load(response)
        except urllib.error.HTTPError as error:
            raise ValueError(f"authority allocation read returned HTTP {error.code}") from error
        except urllib.error.URLError as error:
            raise ValueError(f"authority allocation read failed: {error.reason}") from error
        if not isinstance(document, dict) or not isinstance(document.get("items"), list) or "next_cursor" not in document:
            raise ValueError("authority returned an unreadable allocation page")
        for item in document["items"]:
            if not isinstance(item, dict) or not item.get("id") or item["id"] in seen_ids:
                raise ValueError("authority returned a duplicate or unreadable allocation")
            seen_ids.add(item["id"])
            rows.append(item)
        cursor = document["next_cursor"]
        if cursor is None:
            break
        if not isinstance(cursor, str) or not cursor or cursor in seen_cursors:
            raise ValueError("authority returned a repeated or unreadable cursor")
        seen_cursors.add(cursor)
    return rows


def verify(pilot, approved, protected, observed_cidrs, allocations, read_at, now, maximum_age_seconds):
    if pilot.get("version") != 1 or approved.get("version") != 1:
        raise ValueError("unsupported pilot or approved plan version")
    if not pilot["discovery"]["complete"] or pilot["pilot_gates"]["planning"] != "PASS":
        raise ValueError("complete discovery and a passing planning gate are required")
    finished = instant(pilot["discovery"]["finished_at"])
    oldest = instant(pilot["discovery"]["oldest_observed_at"])
    read_time = instant(read_at)
    if oldest > finished or finished > read_time or read_time > now:
        raise ValueError("inventory finish and authority read times are inconsistent")
    age = (read_time - oldest).total_seconds()
    if age > maximum_age_seconds:
        raise ValueError(f"inventory is stale for allocation verification ({int(age)} seconds)")

    pools = {row["id"]: row for row in approved["pools"]}
    if len(pools) != len(approved["pools"]):
        raise ValueError("approved plan repeats a pool ID")
    protected_ranges = [ipv4(row["cidr"]) for row in protected]
    occupied = [ipv4(value) for value in observed_cidrs]
    selected = pilot["replacements"]["moves"]
    indexed = {}
    for row in allocations:
        if not isinstance(row, dict) or not row.get("tenant_id") or not row.get("allocation_key"):
            raise ValueError("authority read is not an operator-scoped allocation list")
        key = (row["tenant_id"], row["allocation_key"])
        if key in indexed:
            raise ValueError("authority returned duplicate tenant/allocation keys")
        indexed[key] = row
    results = []
    for move in selected:
        target = move.get("target") or {}
        key = (target.get("tenant_id"), target.get("allocation_key"))
        pool = pools.get(move.get("pool_id"))
        row = indexed.get(key)
        reasons = []
        returned = None
        if not move.get("approved"):
            reasons.append("move approval is missing")
        if move.get("authority") != "platform-ipam" or not pool or pool.get("authority") != "platform-ipam":
            reasons.append("the selected pool is not Platform-IPAM-owned")
        if not row:
            reasons.append("no committed allocation was returned by the authority")
        else:
            for field in ("allocation_key", "scope", "environment", "region", "account_id", "prefix_length"):
                if row.get(field) != target.get(field):
                    reasons.append(f"allocation {field} differs from the reviewed target")
            if row.get("tenant_id") != target.get("tenant_id"):
                reasons.append("allocation tenant differs from the reviewed target")
            if row.get("state") not in ("RESERVED", "ACTIVE"):
                reasons.append("allocation is not committed and usable")
            if row.get("pool_id") != move.get("pool_id"):
                reasons.append("allocation pool differs from the proposal")
            try:
                returned = ipv4(row["cidr"])
            except (KeyError, ValueError, TypeError):
                reasons.append("allocation CIDR is absent or invalid")
            if returned:
                if returned.prefixlen != target.get("prefix_length"):
                    reasons.append("allocation CIDR size differs from the reviewed target")
                if move.get("proposed_cidr") != str(returned):
                    reasons.append("allocator returned a different CIDR; owner review is required")
                if pool:
                    pool_range = ipv4(pool["cidr"])
                    if not returned.subnet_of(pool_range):
                        reasons.append("allocation CIDR lies outside the approved pool")
                    if target.get("prefix_length") not in pool.get("allowed_prefix_lengths", []):
                        reasons.append("prefix length is not approved for the pool")
                    if pool.get("region") != target.get("region"):
                        reasons.append("pool region differs from the reviewed target")
                    eligible = pool.get("eligible_accounts") or []
                    if eligible and target.get("account_id") not in eligible:
                        reasons.append("account is not eligible for the approved pool")
                    if any(returned.overlaps(ipv4(cidr)) for cidr in pool.get("excluded_cidrs", [])):
                        reasons.append("allocation CIDR overlaps a pool exclusion")
                if any(returned.overlaps(item) for item in protected_ranges):
                    reasons.append("allocation CIDR overlaps a protected range")
                if any(returned.overlaps(item) for item in occupied):
                    reasons.append("allocation CIDR overlaps observed inventory")
        status = ("VERIFIED" if not reasons else "FAIL" if row and
                  move.get("authority") == "platform-ipam" and pool and
                  pool.get("authority") == "platform-ipam" else "UNKNOWN")
        results.append({"subject": move["subject"], "allocation_key": target.get("allocation_key"),
                        "authority": move.get("authority"), "pool_id": move.get("pool_id"),
                        "proposal_cidr": move.get("proposed_cidr"),
                        "allocation_id": row.get("id") if row else None,
                        "returned_cidr": str(returned) if returned else None,
                        "status": status, "reasons": reasons})
    for index, left in enumerate(results):
        if not left["returned_cidr"]:
            continue
        for right in results[index + 1:]:
            if right["returned_cidr"] and ipv4(left["returned_cidr"]).overlaps(ipv4(right["returned_cidr"])):
                for item in (left, right):
                    item["status"] = "FAIL"
                    item["reasons"].append("returned CIDR overlaps another selected replacement")
    verified = sum(item["status"] == "VERIFIED" for item in results)
    gate = ("PASS" if selected and verified == len(selected) else
            "FAIL" if any(item["status"] == "FAIL" for item in results) else "UNKNOWN")
    return {"version": 1, "authority": "platform-ipam", "read_at": read_at,
            "inventory_finished_at": pilot["discovery"]["finished_at"],
            "inventory_oldest_observed_at": pilot["discovery"]["oldest_observed_at"],
            "inventory_age_seconds": int(age), "max_inventory_age_seconds": maximum_age_seconds,
            "required": len(selected), "verified": verified,
            "valid_until": (oldest + timedelta(seconds=maximum_age_seconds)).isoformat().replace("+00:00", "Z"),
            "allocation_gate": gate,
            "results": results, "network_readiness": "NOT_ASSESSED"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pilot", type=Path, required=True)
    parser.add_argument("--approved-plan", type=Path, required=True)
    parser.add_argument("--protected", type=Path, required=True)
    parser.add_argument("--networks", type=Path, required=True)
    parser.add_argument("--run", type=Path, required=True)
    parser.add_argument("--url", required=True)
    parser.add_argument("--max-inventory-age-seconds", type=int, required=True)
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    try:
        if args.max_inventory_age_seconds <= 0:
            raise ValueError("max inventory age must be positive")
        pilot = read_json(args.pilot)
        hashes = pilot["input_sha256"]
        for name, path in (("approved_plan", args.approved_plan), ("protected", args.protected),
                           ("networks", args.networks), ("run", args.run)):
            if hashes[name] != digest(path):
                raise ValueError(f"{path}: bytes differ from the pilot evidence")
        approved = read_json(args.approved_plan)
        protected = read_json(args.protected)
        if not isinstance(protected, list):
            raise ValueError("protected ranges must be an array")
        with args.networks.open(newline="", encoding="utf-8-sig") as handle:
            network_rows = list(csv.DictReader(handle))
        check_discovery_snapshot(pilot, read_json(args.run), network_rows)
        observed = [row["cidr"] for row in network_rows]
        token = os.environ.get("PLATFORM_IPAM_TOKEN", "")
        rows = authority_allocations(args.url, token)
        read_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
        result = verify(pilot, approved, protected, observed, rows, read_at,
                        datetime.now(timezone.utc), args.max_inventory_age_seconds)
        result["pilot_sha256"] = canonical_digest(pilot)
        body = json.dumps(result, sort_keys=True, indent=2) + "\n"
        if args.out:
            args.out.write_text(body, encoding="utf-8")
        sys.stdout.write(body)
    except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        print(f"reservation verification: {error}", file=sys.stderr)
        return 4
    return 0 if result["allocation_gate"] == "PASS" else 3


if __name__ == "__main__":
    sys.exit(main())
