#!/usr/bin/env python3
"""Check saved Terraform plan JSON for unsafe Platform-IPAM replacements.

Run terraform show -json SAVED_PLAN > PLAN.json, then pass PLAN.json here
before applying that same saved plan. This check is read-only and needs only
Python 3; it does not validate AWS binding, authorization or network readiness.
"""

import argparse
import json
from pathlib import Path
import re
import sys


def replacement_errors(plan):
    if not isinstance(plan, dict) or not isinstance(plan.get("format_version"), str):
        raise ValueError("Expected terraform show -json plan with format_version")
    if not re.fullmatch(r"1\.\d+", plan["format_version"]):
        raise ValueError("Only Terraform plan JSON format major version 1 is supported")
    if "planned_values" not in plan:
        raise ValueError("Plan must include planned_values; state JSON is not a plan")
    resource_changes = plan.get("resource_changes", [])
    if not isinstance(plan["planned_values"], dict) or not isinstance(resource_changes, list):
        raise ValueError("Invalid planned_values or resource_changes")
    if "resource_changes" not in plan and (
            not isinstance(plan.get("configuration"), dict)
            or not isinstance(plan.get("timestamp"), str)):
        raise ValueError("A plan without resource_changes must include configuration and timestamp")
    if plan.get("errored"):
        raise ValueError("Cannot approve an errored plan")
    if plan.get("complete") is False or plan.get("deferred_changes"):
        raise ValueError("Cannot approve an incomplete or deferred plan")
    errors = []
    for resource in resource_changes:
        if not isinstance(resource, dict) or "type" not in resource or "mode" not in resource:
            raise ValueError("Malformed resource change")
        if resource["mode"] not in ("managed", "data"):
            raise ValueError("Unknown resource change mode")
        if resource["mode"] != "managed" or resource["type"] != "platformipam_allocation":
            continue
        change = resource.get("change")
        if not isinstance(change, dict):
            raise ValueError("Malformed allocation change")
        actions = change.get("actions")
        if not isinstance(actions, list) or not actions or any(
                action not in ("no-op", "create", "read", "update", "delete", "forget")
                for action in actions):
            raise ValueError("Missing or unknown allocation change actions")
        if "create" not in actions or "delete" not in actions:
            continue
        before, after = change.get("before"), change.get("after")
        if (before is not None and not isinstance(before, dict)
                or after is not None and not isinstance(after, dict)):
            raise ValueError("Malformed allocation replacement values")
        before, after = before or {}, after or {}
        unknown = change.get("after_unknown", {})
        if not isinstance(unknown, (dict, bool)):
            raise ValueError("Malformed allocation unknown values")
        key_unknown = unknown is True or (isinstance(unknown, dict) and unknown.get("allocation_key"))
        old, new = before.get("allocation_key"), after.get("allocation_key")
        if key_unknown or not isinstance(old, str) or not isinstance(new, str) or not new or old == new:
            errors.append(resource.get("address", "unknown allocation"))
    return errors


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan-json", required=True, type=Path,
                        help="JSON from terraform show -json for the saved plan to apply")
    args = parser.parse_args(argv)
    try:
        errors = replacement_errors(json.loads(args.plan_json.read_text(encoding="utf-8")))
    except (OSError, ValueError) as error:
        print(json.dumps({"check": "replacement-keys", "status": "FAILED", "detail": str(error)}))
        return 1
    print(json.dumps({"check": "replacement-keys", "status": "FAILED" if errors else "PASSED",
                      "unsafe_allocations": errors}))
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
