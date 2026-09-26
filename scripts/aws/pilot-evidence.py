#!/usr/bin/env python3
"""Derive a read-only pilot overview from the existing assessment reports.

This projection needs a separate current authority report to verify a
reservation: a progress target fact alone does not prove its CIDR or pool.
"""

import argparse
import csv
from datetime import datetime, timedelta, timezone
import hashlib
import ipaddress
import json
from pathlib import Path
import sys


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def canonical_digest(value):
    return hashlib.sha256((json.dumps(value, sort_keys=True, indent=2) + "\n").encode()).hexdigest()


def attach_verification(pilot, verification, now=None):
    if verification.get("version") != 1 or verification.get("authority") != "platform-ipam":
        raise ValueError("unsupported authority verification report")
    if not pilot["discovery"]["complete"] or pilot["pilot_gates"]["planning"] != "PASS":
        raise ValueError("complete discovery and a passing planning gate are required for authority verification")
    if verification.get("pilot_sha256") != canonical_digest(pilot):
        raise ValueError("authority verification belongs to a different pilot report")
    moves = pilot["replacements"]["moves"]
    rows = verification.get("results")
    if not isinstance(rows, list) or len(rows) != len(moves) or verification.get("required") != len(moves):
        raise ValueError("authority verification does not cover every selected move")
    if [item.get("subject") for item in rows] != [item["subject"] for item in moves]:
        raise ValueError("authority verification subjects differ from selected moves")
    verified = sum(item.get("status") == "VERIFIED" for item in rows)
    if verified != verification.get("verified") or any(item.get("status") not in ("VERIFIED", "FAIL", "UNKNOWN") for item in rows):
        raise ValueError("authority verification counts or statuses are inconsistent")
    gate = "PASS" if moves and verified == len(moves) else "FAIL" if any(
        item["status"] == "FAIL" for item in rows) else "UNKNOWN"
    if gate != verification.get("allocation_gate") or verification.get("network_readiness") != "NOT_ASSESSED":
        raise ValueError("authority verification gate is inconsistent")
    now = now or datetime.now(timezone.utc)
    read_at = instant(verification["read_at"])
    valid_until = instant(verification["valid_until"])
    oldest = instant(pilot["discovery"]["oldest_observed_at"])
    finished = instant(pilot["discovery"]["finished_at"])
    maximum_age = verification.get("max_inventory_age_seconds")
    if (any(item.tzinfo is None for item in (read_at, valid_until, oldest, finished)) or
            type(maximum_age) is not int or maximum_age <= 0 or
            verification.get("inventory_oldest_observed_at") != pilot["discovery"]["oldest_observed_at"] or
            valid_until != oldest + timedelta(seconds=maximum_age) or
            not oldest <= finished <= read_at <= valid_until or read_at > now):
        raise ValueError("authority verification timestamps are inconsistent")
    stale = now > valid_until
    for move, row in zip(moves, rows):
        move["reservation_verified"] = not stale and row["status"] == "VERIFIED"
        move["reservation_status"] = "STALE_VERIFICATION" if stale else row["status"]
        move["allocation_id"] = row.get("allocation_id")
        move["returned_cidr"] = row.get("returned_cidr")
        move["reservation_reasons"] = row.get("reasons") or []
    for relationship in pilot.get("relationships", []):
        selected = relationship.get("selected_moves") or []
        if relationship.get("impact") == "confirmed" and selected and all(
                move["reservation_verified"] for move in selected):
            relationship["next_step"] = (
                "Reservation evidence matches the reviewed replacement. The workload owner plans migration; "
                "rerun discovery before concluding the old conflict is gone.")
    pilot["pilot_gates"]["allocation"] = "UNKNOWN" if stale else gate
    pilot["replacements"]["reservations_verified"] = 0 if stale else verified
    pilot["blockers"] = [item for item in pilot["blockers"] if item["code"] != "ALLOCATION_VERIFICATION_NOT_SUPPLIED"]
    if stale:
        pilot["blockers"].append({"code": "AUTHORITY_VERIFICATION_STALE",
                                  "detail": "The uploaded authority read exceeded the approved inventory age limit."})
    elif gate != "PASS":
        pilot["blockers"].append({"code": "ALLOCATION_GATE_NOT_PASSED",
                                  "detail": "Review the imported authority check for missing or mismatched reservations."})
    pilot["authority_verification"] = {"source": "uploaded_cli_verifier_report",
                                       "read_at": verification["read_at"],
                                       "valid_until": verification["valid_until"],
                                       "sha256": canonical_digest(verification)}
    return pilot


def attach_execution(pilot, evidence):
    """Join customer-supplied Terraform run references without changing readiness."""
    if evidence.get("version") != 1 or evidence.get("input_sha256") != pilot["input_sha256"]:
        raise ValueError("execution evidence belongs to a different pilot input snapshot")
    rows = evidence.get("moves")
    if not isinstance(rows, list):
        raise ValueError("execution evidence needs a moves array")
    by_key = {move.get("allocation_key"): move for move in pilot["replacements"]["moves"]
              if isinstance(move.get("allocation_key"), str) and move["allocation_key"]}
    seen = set()
    for row in rows:
        if not isinstance(row, dict) or row.get("allocation_key") not in by_key:
            raise ValueError("execution evidence names an unknown allocation key")
        key = row["allocation_key"]
        if key in seen or row.get("status") not in ("PLANNED", "APPLIED", "FAILED"):
            raise ValueError("execution evidence repeats a key or has an unsupported status")
        seen.add(key)
        by_key[key]["execution"] = {"status": row["status"],
                                     "commit": row.get("commit"), "run": row.get("run"),
                                     "observed_after": row.get("observed_after")}
    pilot["execution_evidence"] = {"source": "uploaded_customer_report",
                                   "reported_moves": len(rows),
                                   "note": "Customer-supplied execution status; no Terraform or AWS state was authenticated."}
    return pilot


def instant(value):
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def derive(address, progress, run, networks, network_path, run_path, assessment=None):
    if address.get("version") != 1 or progress.get("report_version") != 1:
        raise ValueError("unsupported address or progress report version")
    scope = address["pilot_scope"]
    accounts = scope["accounts"]
    regions = scope["regions"]
    if not accounts or not regions:
        raise ValueError("pilot overview requires explicit account and region scope")
    if address["input_sha256"]["networks"] != digest(network_path):
        raise ValueError("networks.csv differs from the address report")
    report_hashes = {item["sha256"] for item in progress["assessment"]["inputs"]}
    required_hashes = [address["input_sha256"][name] for name in ("networks", "matrix", "protected")]
    if any(value not in report_hashes for value in required_hashes):
        raise ValueError("address and progress reports use different assessment inputs")
    if address["input_sha256"].get("migration_plan") and address["input_sha256"]["migration_plan"] not in {
            item.get("sha256") for item in progress.get("inputs", [])}:
        raise ValueError("address proposal and progress use different migration plans")
    if digest(run_path) not in report_hashes:
        raise ValueError("run.json differs from the progress assessment")
    if assessment is not None:
        assessment_hashes = {item["sha256"] for item in assessment.get("inputs", [])}
        if assessment_hashes != report_hashes or assessment.get("summary", {}).get("total_relationships") != progress["assessment"]["summary"]["total_relationships"]:
            raise ValueError("full assessment differs from the pilot's progress assessment")
    requested = {(account, region) for account in accounts for region in regions}
    cells = {}
    for account in run.get("accounts", []):
        account_id = account.get("account_id")
        for cell in account.get("regions", []):
            key = (account_id, cell.get("region"))
            if key not in requested:
                continue
            if key in cells:
                raise ValueError(f"run.json repeats account-region cell {key}")
            cells[key] = cell
    coverage = []
    observed_at = []
    failed = {(item.get("account_id"), item.get("region")): item
              for item in progress["assessment"]["coverage"].get("failed") or []}
    for account, region in sorted(requested):
        cell = cells.get((account, region))
        outcome = cell.get("outcome", "unknown") if cell else "not_attempted"
        when = cell.get("observed_at") if cell else None
        failure = failed.get((account, region))
        reason = (failure.get("error") if failure else
                  cell.get("reason") if cell else "missing-from-run")
        if outcome == "succeeded" and when:
            observed_at.append(when)
        coverage.append({"account_id": account, "region": region, "status": outcome,
                         "observed_at": when, "reason": reason,
                         "stage": failure.get("stage") if failure else cell.get("stage") if cell else None})
    scanned = sum(cell["status"] == "succeeded" for cell in coverage)
    assessment_complete = bool(progress["assessment"]["coverage"].get("complete"))
    full_coverage = scanned == len(requested) and assessment_complete and address["coverage_complete"]

    vpcs = set()
    associations = 0
    for row in networks:
        if (row.get("account_id"), row.get("region")) not in requested or row.get("type") != "vpc":
            continue
        vpcs.add((row["account_id"], row["region"], row["resource_id"]))
        associations += 1

    proposals = {(item["account_id"], item["region"], item["vpc_id"]): item
                 for item in address["proposals"]}
    moves = []
    blockers = []
    if not full_coverage:
        blockers.append({"code": "DISCOVERY_INCOMPLETE", "detail": "One or more requested cells or assessment inputs lack complete evidence."})
    if address["unresolved_intent_conflicts"]:
        blockers.append({"code": "CONNECTIVITY_UNKNOWN", "detail": "Review unresolved overlapping connectivity relationships."})
    replacement_count = 0
    approved_count = 0
    candidate_count = 0
    target_evidence_count = 0
    claimed_conflicts = set()
    for move in progress["moves"]:
        if move["disposition"] != "replace":
            continue
        key = (move["account_id"], move["region"], move["vpc_id"])
        if (key[0], key[1]) not in requested:
            raise ValueError(f"replacement move outside pilot scope: {move['subject']}")
        replacement_count += 1
        approved = bool(move.get("approval"))
        approved_count += approved
        proposal = proposals.get(key)
        status = proposal.get("status") if proposal else "NO_PROPOSAL"
        candidate = proposal.get("candidate_cidr") if proposal else None
        target = move.get("target") or {}
        target_match = bool(proposal and proposal.get("target_account_id") == target.get("account_id") and
                            proposal.get("target_region") == target.get("region") and
                            proposal.get("prefix_length") == target.get("prefix_length") and
                            proposal.get("reviewed_target"))
        if candidate and target_match:
            target_match = ipaddress.ip_network(candidate, strict=True).prefixlen == target["prefix_length"]
        candidate_count += status == "ADVISORY_CANDIDATE" and bool(candidate) and target_match
        target_fact = move.get("target_fact", "unknown")
        target_evidence_count += target_fact in ("reserved", "active")
        claimed_conflicts.update(item["conflict_id"] for item in move.get("resolves") or [])
        if not approved:
            blockers.append({"code": "MOVE_NOT_APPROVED", "subject": move["subject"],
                             "detail": "The replacement move has no reviewed approval."})
        if status not in ("ADVISORY_CANDIDATE", "REQUEST_FROM_AWS_IPAM"):
            blockers.append({"code": "REPLACEMENT_OPTION_MISSING", "subject": move["subject"],
                             "detail": "No eligible replacement option is available for this move."})
        elif not target_match:
            blockers.append({"code": "PROPOSAL_TARGET_MISMATCH", "subject": move["subject"],
                             "detail": "The proposed pool or CIDR size was not derived from this reviewed target request. Replan with migration.yaml."})
        moves.append({"subject": move["subject"], "owner": move.get("owner"),
                      "approved": approved, "approval": move.get("approval"),
                      "wave_id": move.get("wave_id"), "depends_on": move.get("depends_on") or [],
                      "blockers": move.get("blockers") or [], "rollback": move.get("rollback"),
                      "verifications": move.get("verifications") or [], "notes": move.get("notes"),
                      "allocation_key": (move.get("target") or {}).get("allocation_key"),
                      "target": move.get("target"),
                      "conflict_ids": [item["conflict_id"] for item in move.get("resolves") or []],
                      "pool_id": proposal.get("pool_id") if proposal else None,
                      "authority": proposal.get("authority") if proposal else None,
                      "proposed_cidr": candidate, "proposal_status": status,
                      "proposal_target_match": target_match,
                      "target_fact": target_fact, "reservation_verified": False,
                      "reservation_status": "VERIFICATION_NOT_SUPPLIED"})
    if not replacement_count:
        blockers.append({"code": "NO_REVIEWED_REPLACEMENTS", "detail": "The reviewed plan names no replacement moves in the pilot scope."})
    if replacement_count and approved_count != replacement_count:
        blockers.append({"code": "MOVE_APPROVAL_INCOMPLETE", "detail": "At least one replacement has no move approval."})
    if replacement_count and candidate_count != replacement_count:
        blockers.append({"code": "CIDR_CANDIDATES_INCOMPLETE", "detail": "A selected move lacks a locally proposed CIDR; AWS IPAM recommendations intentionally have no CIDR."})
    unclaimed_conflicts = sorted({item["id"] for item in address["conflicts"]} - claimed_conflicts)
    if unclaimed_conflicts:
        blockers.append({"code": "CONFLICT_WITHOUT_SELECTED_MOVE", "detail": "Reviewed replacements do not claim every relevant conflict.",
                         "conflict_ids": unclaimed_conflicts})
    blockers.append({"code": "ALLOCATION_VERIFICATION_NOT_SUPPLIED", "detail": "No current authority verification report was supplied; target facts do not prove the returned CIDR or pool."})
    if address["confirmed_conflicts"]:
        blockers.append({"code": "CURRENT_ADDRESS_CONFLICTS", "detail": "Observed VPC conflicts remain until a later discovery proves otherwise."})

    selected_moves = {move["subject"]: move for move in moves}
    subnet_counts = {}
    for row in networks:
        if row.get("type") != "subnet":
            continue
        key = (row.get("account_id"), row.get("region"), row.get("parent_id"))
        subnet_counts[key] = subnet_counts.get(key, 0) + 1
    relationships = []
    relationship_source = (assessment["conflicts"] if assessment is not None else
                           progress["assessment"].get("conflicts") or address["conflicts"])
    for conflict in relationship_source:
        if not conflict.get("sides"):
            continue
        selected = [selected_moves[f"aws:{side['account_id']}:{side['region']}:{side['vpc_id']}"]
                    for side in conflict["sides"]
                    if f"aws:{side['account_id']}:{side['region']}:{side['vpc_id']}" in selected_moves]
        impact = conflict["impact"]
        review_order = []
        for side in conflict["sides"]:
            environment = (side.get("environment") or "unknown").lower()
            observed_subnets = subnet_counts.get((side["account_id"], side["region"], side["vpc_id"]), 0)
            subject = f"aws:{side['account_id']}:{side['region']}:{side['vpc_id']}"
            review_order.append({"subject": subject, "environment": environment,
                                 "observed_subnets": observed_subnets,
                                 "owner": side.get("owner"),
                                 "selected_replace": subject in selected_moves})
        rank = lambda item: (0 if item["environment"] in ("dev", "development", "nonprod", "nonproduction", "stage", "staging")
                             else 2 if item["environment"] in ("prod", "production") else 1,
                             item["observed_subnets"], item["subject"])
        review_order.sort(key=rank)
        if impact == "confirmed" and selected and all(move["reservation_verified"] for move in selected):
            next_step = "Reservation evidence matches the reviewed replacement. The workload owner plans migration; rerun discovery before concluding the old conflict is gone."
        elif impact == "confirmed" and selected:
            next_step = "Review the selected replacement and approved pool, reserve through its sole authority, then upload a current verification report."
        elif impact == "confirmed":
            next_step = "Assign an owner and review which VPC to keep or replace in migration.yaml. The review order below uses only environment and observed subnet counts."
        elif conflict.get("matrix_relation") == "must_stay_isolated":
            next_step = "Keep the routing domains isolated and confirm that the connectivity matrix remains current. Reassess before joining them."
        else:
            next_step = "Clarify whether these VPCs must communicate; leave address readiness unknown for this relationship until intent is reviewed."
        relationships.append({"id": conflict["id"], "impact": impact,
                              "kind": conflict["kind"], "matrix_relation": conflict.get("matrix_relation"),
                              "sides": conflict["sides"], "selected_moves": selected,
                              "review_order": review_order,
                              "review_limit": "Review order is advisory: workload dependencies, route references and migration cost were not assessed.",
                              "next_step": next_step})
    impacts_by_vpc = {}
    for relationship in relationships:
        for side in relationship["sides"]:
            key = (side["account_id"], side["region"], side["vpc_id"])
            impacts_by_vpc.setdefault(key, set()).add(relationship["impact"])
    observed_vpcs = {}
    for row in networks:
        if row.get("type") != "vpc" or (row.get("account_id"), row.get("region")) not in requested:
            continue
        key = (row["account_id"], row["region"], row["resource_id"])
        item = observed_vpcs.setdefault(key, {"account_id": key[0], "region": key[1],
                                              "vpc_id": key[2], "name": row.get("name") or "",
                                              "cidrs": []})
        if row["cidr"] not in item["cidrs"]:
            item["cidrs"].append(row["cidr"])
    inventory_vpcs = []
    relationship_details_missing = bool(address["confirmed_conflicts"] and not relationships)
    for key, item in sorted(observed_vpcs.items()):
        impacts = impacts_by_vpc.get(key, set())
        item["overlap_status"] = ("RELATIONSHIP_DETAIL_UNAVAILABLE" if relationship_details_missing else
                                  "CONFIRMED_CONFLICT" if "confirmed" in impacts else
                                  "UNKNOWN_RELATIONSHIP" if "unknown" in impacts else
                                  "ISOLATED_OVERLAP" if "potential" in impacts else
                                  "NO_OBSERVED_OVERLAP")
        inventory_vpcs.append(item)

    started = run.get("started_at")
    finished = run.get("finished_at")
    duration = int((instant(finished) - instant(started)).total_seconds()) if started and finished else None
    input_hashes = {**address["input_sha256"], "run": digest(run_path)}
    plan_hashes = [item["sha256"] for item in progress.get("inputs", []) if item.get("sha256")]
    if plan_hashes:
        input_hashes["migration_plans"] = plan_hashes
    result = {
        "version": 1,
        "pilot_owner": scope["owner"],
        "current_address_readiness": address["address_readiness"],
        "network_readiness": "NOT_ASSESSED",
        "pilot_gates": {"input": "UNKNOWN", "discovery": "UNKNOWN", "planning": "PASS" if full_coverage and
                        not address["unresolved_intent_conflicts"] and replacement_count and
                        approved_count == replacement_count and candidate_count == replacement_count and
                        not unclaimed_conflicts else "UNKNOWN",
                        "allocation": "UNKNOWN"},
        "scope": {"accounts": accounts, "regions": regions},
        "discovery": {"requested_cells": len(requested), "scanned_cells": scanned,
                      "complete": full_coverage, "cells": coverage, "started_at": started,
                      "finished_at": finished, "duration_seconds": duration,
                      "collector_version": run.get("script_version"),
                      "oldest_observed_at": min(observed_at) if observed_at else None,
                      "newest_observed_at": max(observed_at) if observed_at else None,
                      "vpcs": len(vpcs), "vpc_cidr_associations": associations,
                      "without_observed_overlap": sum(item["overlap_status"] == "NO_OBSERVED_OVERLAP" for item in inventory_vpcs),
                      "inventory_vpcs": inventory_vpcs},
        "conflicts": {"confirmed": address["confirmed_conflicts"],
                      "unresolved_intent": address["unresolved_intent_conflicts"],
                      "observed_relationships": len(relationships)},
        "relationships": relationships,
        "replacements": {"planned": replacement_count, "approved": approved_count,
                         "proposed_cidrs": candidate_count, "target_facts_reserved_or_active": target_evidence_count,
                         "reservations_verified": 0, "moves": moves},
        "reviewed_waves": [{"id": wave.get("id"), "name": wave.get("name"),
                             "owner": wave.get("owner"), "approval": wave.get("approval"),
                             "moves_total": wave.get("moves_total")}
                            for wave in progress.get("waves") or []],
        "blockers": blockers,
        "input_sha256": input_hashes,
    }
    if address.get("scope_inputs"):
        result["scope_inputs"] = address["scope_inputs"]
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("address", "progress", "run", "networks"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--assessment", type=Path, help="full onboard assess JSON for all relationship drill-downs")
    parser.add_argument("--out", type=Path)
    parser.add_argument("--verification", type=Path,
                        help="optional report from verify-reservations.py; no API credential is used here")
    parser.add_argument("--execution", type=Path,
                        help="optional customer-supplied Terraform execution references")
    args = parser.parse_args()
    try:
        address = json.loads(args.address.read_text(encoding="utf-8"))
        progress = json.loads(args.progress.read_text(encoding="utf-8"))
        run = json.loads(args.run.read_text(encoding="utf-8"))
        with args.networks.open(newline="", encoding="utf-8-sig") as handle:
            networks = list(csv.DictReader(handle))
        assessment = json.loads(args.assessment.read_text(encoding="utf-8")) if args.assessment else None
        report = derive(address, progress, run, networks, args.networks, args.run, assessment)
        if args.verification:
            report = attach_verification(report, json.loads(args.verification.read_text(encoding="utf-8")))
        if args.execution:
            report = attach_execution(report, json.loads(args.execution.read_text(encoding="utf-8")))
        body = json.dumps(report, sort_keys=True, indent=2) + "\n"
        if args.out:
            args.out.write_text(body, encoding="utf-8")
        sys.stdout.write(body)
    except (OSError, KeyError, TypeError, ValueError, json.JSONDecodeError) as error:
        print(f"pilot evidence: {error}", file=sys.stderr)
        return 4
    return 0


if __name__ == "__main__":
    sys.exit(main())
