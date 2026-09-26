#!/usr/bin/env python3
"""Collect read-only route and TGW evidence for a prior organization inventory.

The output is independent of networks.csv/run.json coverage: an unread route
table never changes a known VPC CIDR into an unknown one, and a known CIDR
never makes the topology complete. AWS CLI v2 performs EC2 pagination unless
the caller explicitly disables it; this script passes no pagination limit.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from datetime import datetime, timezone


READS = (
    ("vpc_route_tables", "describe-route-tables", "RouteTables"),
    ("transit_gateways", "describe-transit-gateways", "TransitGateways"),
    ("tgw_attachments", "describe-transit-gateway-attachments", "TransitGatewayAttachments"),
    ("tgw_vpc_attachments", "describe-transit-gateway-vpc-attachments", "TransitGatewayVpcAttachments"),
    ("tgw_route_tables", "describe-transit-gateway-route-tables", "TransitGatewayRouteTables"),
)


def utc_now():
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def aws_json(env, *args):
    result = subprocess.run(
        ["aws", *args, "--output", "json", "--no-cli-pager"],
        env=env,
        capture_output=True,
        text=True,
        timeout=90,
        check=False,
    )
    if result.returncode:
        raise RuntimeError("AWS read failed")
    value = json.loads(result.stdout)
    if not isinstance(value, dict):
        raise ValueError("AWS response was not an object")
    if value.get("NextToken") or value.get("nextToken"):
        raise ValueError("AWS CLI returned an unconsumed pagination token")
    return value


def assumed_env(base_env, account_id, role_name):
    role = f"arn:aws:iam::{account_id}:role/{role_name}"
    value = aws_json(base_env, "sts", "assume-role", "--role-arn", role,
                     "--role-session-name", "ipam-topology", "--duration-seconds", "900")
    credentials = value["Credentials"]
    env = base_env.copy()
    env.update({
        "AWS_ACCESS_KEY_ID": credentials["AccessKeyId"],
        "AWS_SECRET_ACCESS_KEY": credentials["SecretAccessKey"],
        "AWS_SESSION_TOKEN": credentials["SessionToken"],
    })
    identity = aws_json(env, "sts", "get-caller-identity")
    if identity.get("Account") != account_id:
        raise ValueError("assumed role account identity mismatch")
    return env


def read_stage(env, region, cell, field, operation, output_key, *extra):
    try:
        value = aws_json(env, "ec2", operation, "--region", region, *extra)
        rows = value.get(output_key)
        if not isinstance(rows, list):
            raise ValueError("AWS response omitted its collection")
        cell[field] = rows
        return value
    except (OSError, RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired):
        if "gaps" in cell:
            cell["gaps"].append({"stage": operation, "reason": "read-unavailable"})
        return None


def scan_cell(env, account_id, region):
    cell = {
        "account_id": account_id,
        "region": region,
        "observed_at": utc_now(),
        "gaps": [],
    }
    for field, operation, output_key in READS:
        read_stage(env, region, cell, field, operation, output_key)

    for table in cell.get("tgw_route_tables", []):
        table_id = table.get("TransitGatewayRouteTableId")
        if not table_id:
            cell["gaps"].append({"stage": "tgw-route-table-id", "reason": "missing-id"})
            continue
        for field, operation, output_key in (
            ("associations", "get-transit-gateway-route-table-associations", "Associations"),
            ("propagations", "get-transit-gateway-route-table-propagations", "TransitGatewayRouteTablePropagations"),
        ):
            read_stage(env, region, table, field, operation, output_key,
                       "--transit-gateway-route-table-id", table_id)
            if field not in table:
                cell["gaps"].append({"stage": operation, "route_table_id": table_id,
                                     "reason": "read-unavailable"})

        route_result = read_stage(
            env, region, table, "routes", "search-transit-gateway-routes", "Routes",
            "--transit-gateway-route-table-id", table_id,
            "--filters", "Name=type,Values=static,propagated",
        )
        if route_result is None:
            cell["gaps"].append({"stage": "search-transit-gateway-routes",
                                 "route_table_id": table_id, "reason": "read-unavailable"})
        elif route_result.get("AdditionalRoutesAvailable"):
            cell["gaps"].append({"stage": "search-transit-gateway-routes",
                                 "route_table_id": table_id, "reason": "additional-routes-available"})

    cell["complete"] = not cell["gaps"]
    return cell


def collect(inventory, role_name, management_account_id):
    accounts_path = inventory / "accounts.json"
    run_path = inventory / "run.json"
    accounts_bytes = accounts_path.read_bytes()
    run_bytes = run_path.read_bytes()
    accounts = json.loads(accounts_bytes)["Accounts"]
    run = json.loads(run_bytes)
    attempts = {entry["account_id"]: entry for entry in run["accounts"]}
    base_env = os.environ.copy()
    report = {
        "version": 1,
        "started_at": utc_now(),
        "sources": [
            {"name": "accounts.json", "sha256": hashlib.sha256(accounts_bytes).hexdigest()},
            {"name": "run.json", "sha256": hashlib.sha256(run_bytes).hexdigest()},
        ],
        "cells": [],
        "gaps": [],
    }
    for account in accounts:
        if account.get("Status") != "ACTIVE":
            continue
        account_id = account["Id"]
        attempt = attempts.get(account_id)
        if not attempt:
            report["gaps"].append({"account_id": account_id, "reason": "account-not-in-run"})
            continue
        regions = [entry["region"] for entry in attempt.get("regions", []) if entry.get("region")]
        if not regions:
            report["gaps"].append({"account_id": account_id, "reason": "region-set-unknown"})
            continue
        try:
            if account_id == management_account_id:
                identity = aws_json(base_env, "sts", "get-caller-identity")
                if identity.get("Account") != account_id:
                    raise ValueError("management account identity mismatch")
                env = base_env
            else:
                env = assumed_env(base_env, account_id, role_name)
        except (OSError, RuntimeError, ValueError, KeyError, subprocess.TimeoutExpired):
            for region in regions:
                report["gaps"].append({"account_id": account_id, "region": region,
                                       "reason": "credentials-unavailable"})
            continue
        for region in regions:
            report["cells"].append(scan_cell(env, account_id, region))
    if not report["cells"] and not report["gaps"]:
        report["gaps"].append({"reason": "no-active-account-region-cells"})
    report["finished_at"] = utc_now()
    report["complete"] = not report["gaps"] and all(cell["complete"] for cell in report["cells"])
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True,
                        help="directory with accounts.json and run.json from org-inventory.sh")
    parser.add_argument("--out", type=Path, required=True, help="topology JSON output path")
    parser.add_argument("--role-name", default="PlatformIpamTopologyReadOnly")
    parser.add_argument("--management-account-id", default="")
    args = parser.parse_args()
    try:
        report = collect(args.inventory, args.role_name, args.management_account_id)
        args.out.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=args.out.parent,
                                         prefix=".topology-", delete=False) as handle:
            json.dump(report, handle, sort_keys=True, indent=2)
            handle.write("\n")
            temporary = Path(handle.name)
        temporary.replace(args.out)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
        print(f"topology collection has no report: {error}", file=sys.stderr)
        return 4
    print(f"topology: {len(report['cells'])} cell(s), complete={str(report['complete']).lower()}", file=sys.stderr)
    return 0 if report["complete"] else 3


if __name__ == "__main__":
    sys.exit(main())
