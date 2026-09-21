#!/usr/bin/env python3
"""Small, repeatable development NetBox bootstrap fallback.

The platform's `seed` mode is preferred. This script only creates the narrow
custom-field/VRF/pool contract used by the local fixture and refuses any
non-development endpoint or environment.
"""

from __future__ import annotations

import argparse
import json
import os
from urllib.error import HTTPError, URLError
from urllib.parse import urljoin
from urllib.request import Request, urlopen


# NetBox requires every selection custom field to reference a choice set.
# The values are taken from the implementation, not invented here:
# lifecycle states are internal/domain/types.go, and the drift summary mirrors
# the finding codes the worker and service actually emit. The NetBox adapter
# does not populate platform_drift_status yet; the field exists so operator
# filters have a stable vocabulary once it does.
CHOICE_SETS = (
    ("platform-allocation-state", ["RESERVED", "ACTIVE", "QUARANTINED", "RELEASED"]),
    (
        "platform-drift-status",
        ["none", "coverage_incomplete", "unmanaged_occupancy", "resource_missing", "binding_mismatch"],
    ),
)

SELECT_CHOICE_SETS = {
    "platform_state": "platform-allocation-state",
    "platform_drift_status": "platform-drift-status",
}

FIELDS = (
    ("platform_allocation_id", "text"),
    ("platform_allocation_key", "text"),
    ("platform_operation_id", "text"),
    ("platform_parent_allocation_id", "text"),
    ("platform_tenant_id", "text"),
    ("platform_environment", "text"),
    ("platform_pool_id", "text"),
    ("platform_policy_version", "text"),
    ("platform_state", "select"),
    ("platform_aws_account_id", "text"),
    ("platform_aws_region", "text"),
    ("platform_aws_resource_id", "text"),
    ("platform_aws_az_id", "text"),
    ("platform_quarantine_until", "datetime"),
    ("platform_last_observed_at", "datetime"),
    ("platform_drift_status", "select"),
    ("platform_import_batch", "text"),
    ("platform_import_source", "text"),
    ("platform_import_contributors", "json"),
    ("platform_import_contributors_reconstructed", "boolean"),
)

# Onboarding import writes occupancy, never allocations (ADR 0007): these two
# fields name the batch a row came from so a whole import can be listed or
# removed later. They are the only custom fields the import writes on an IP
# range, which is why they are defined for ranges as well as prefixes; the
# allocation markers above stay prefix-only.
#
# platform_import_contributors (ADR 0016) is prefix-only and so is absent from
# this map, taking the ["ipam.prefix"] default below. It holds the JSON array
# of cloud resources that occupy a prefix, which is what the import's
# collapse-by-CIDR used to discard. The pinned NetBox v4.6.7-5.0.2 offers a
# `json` custom-field type on ipam.prefix and this script's own ensure()
# creates it unchanged -- measured 2026-09-22, ADR 0016's dated paragraph.
# The canonical ranges table has no account, region or resource column, so a
# range has no contributors to record and internal/netbox refuses them there
# rather than dropping them silently.
#
# platform_import_contributors_reconstructed (ADR 0016, package M9b3) is the
# top-level marker beside the list, prefix-only for the same reason: package
# M9b3's `apply --refresh` sets it once, the first time it populates a
# contributor list on a prefix that carried none at all, so a later removal
# can refuse a reconstructed list without reading a single entry. Measured
# against the pinned image 2026-09-22 (ADR 0016's M9b3 dated paragraph): a
# `boolean` custom field on ipam.prefix round-trips as JSON true/false, and an
# unset field reads back as the key present with JSON null -- the same shape
# every other custom field in this list already has, which internal/netbox's
# boolCF was written to survive.
#
# The order here does not matter: NetBox returns object types in its own
# order, and matches() compares lists of plain values as sets.
IMPORT_OBJECT_TYPES = sorted(("ipam.prefix", "ipam.iprange"))
FIELD_OBJECT_TYPES = {
    "platform_import_batch": IMPORT_OBJECT_TYPES,
    "platform_import_source": IMPORT_OBJECT_TYPES,
}

# Tag assignment on create resolves an existing tag by slug and fails
# otherwise, so the import adapter cannot create this itself.
TAGS = (
    (
        "platform-ipam-imported",
        "platform-ipam-imported",
        "occupancy imported by platform-ipam onboarding; not a platform allocation",
    ),
)


def request(base: str, token: str, path: str, method: str = "GET", payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    req = Request(urljoin(base.rstrip("/") + "/", path.lstrip("/")), data=body, method=method)
    req.add_header("Authorization", f"Token {token}")
    req.add_header("Accept", "application/json")
    req.add_header("Content-Type", "application/json")
    with urlopen(req, timeout=10) as response:
        return json.loads(response.read().decode())


def matches(existing, wanted) -> bool:
    """Compare a sent payload value against NetBox's rendered representation.

    NetBox echoes choices as {"value", "label"} and related objects as nested
    briefs, so a naive equality check reports a conflict for a record this
    script itself just created and breaks re-runs.
    """
    if isinstance(existing, dict):
        if isinstance(wanted, dict):
            return all(matches(existing.get(key), value) for key, value in wanted.items())
        if "value" in existing:
            return existing["value"] == wanted
        if "id" in existing:
            return existing["id"] == wanted
        return False
    if isinstance(existing, list) and isinstance(wanted, list):
        if len(existing) != len(wanted):
            return False
        # Lists of plain values (object_types, tags) are sets as far as NetBox
        # is concerned, and it returns them in its own order -- not the order
        # they were sent in, and not alphabetical. Comparing position by
        # position made the second run of this script report a conflict with a
        # record the first run had just created.
        if all(isinstance(item, (str, int, float, bool)) for item in existing + wanted):
            return sorted(map(str, existing)) == sorted(map(str, wanted))
        return all(matches(item, other) for item, other in zip(existing, wanted))
    return existing == wanted


def ensure(base: str, token: str, endpoint: str, unique_key: str, payload: dict, dry_run: bool):
    result = request(base, token, endpoint + f"?{unique_key}")
    rows = result.get("results", [])
    if len(rows) > 1:
        raise RuntimeError(f"multiple existing definitions matched {endpoint} {unique_key}")
    if rows:
        if any(not matches(rows[0].get(key), value) for key, value in payload.items() if key in rows[0]):
            raise RuntimeError(f"conflicting existing definition at {endpoint}: {rows[0].get('name')}")
        return rows[0]
    if dry_run:
        print(f"would create {endpoint}: {payload.get('name', payload)}")
        return payload
    return request(base, token, endpoint, "POST", payload)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default=os.environ.get("NETBOX_URL", "http://127.0.0.1:8000"))
    parser.add_argument("--token", default=os.environ.get("NETBOX_TOKEN", ""))
    parser.add_argument("--environment", default=os.environ.get("IPAM_ENVIRONMENT", "development"))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.environment != "development":
        raise SystemExit("refusing to seed a non-development environment")
    if not args.url.startswith(("http://127.0.0.1", "http://localhost", "http://netbox:")):
        raise SystemExit("refusing a non-local NetBox URL")
    if not args.token and not args.dry_run:
        raise SystemExit("NETBOX_TOKEN is required")

    choice_set_ids = {}
    for set_name, choices in CHOICE_SETS:
        choice_set = ensure(args.url, args.token, "/api/extras/custom-field-choice-sets/",
                            f"name={set_name}", {
                                "name": set_name,
                                "extra_choices": [[value, value] for value in choices],
                            }, args.dry_run)
        choice_set_ids[set_name] = choice_set.get("id")
    for name, field_type in FIELDS:
        payload = {"name": name, "type": field_type,
                   "object_types": FIELD_OBJECT_TYPES.get(name, ["ipam.prefix"])}
        if field_type == "select":
            set_name = SELECT_CHOICE_SETS[name]
            payload["choice_set"] = choice_set_ids[set_name]
        ensure(args.url, args.token, "/api/extras/custom-fields/", f"name={name}", payload, args.dry_run)
    for tag_name, tag_slug, tag_description in TAGS:
        ensure(args.url, args.token, "/api/extras/tags/", f"slug={tag_slug}", {
            "name": tag_name, "slug": tag_slug, "description": tag_description,
        }, args.dry_run)
    vrf = ensure(args.url, args.token, "/api/ipam/vrfs/", "name=platform-development", {
        "name": "platform-development", "rd": "65000:100", "enforce_unique": True,
    }, args.dry_run)
    vrf_id = vrf.get("id")
    if vrf_id is None and not args.dry_run:
        raise RuntimeError("NetBox did not return the stable development VRF id")
    if vrf_id not in (None, 1):
        raise RuntimeError(f"development pool expects VRF id 1, found {vrf_id}; refusing to rewrite the app fixture")
    prefix_payload = {
        "prefix": "10.64.0.0/16", "status": "container", "vrf": vrf_id,
        "description": "platform-ipam development VPC pool",
        "custom_fields": {"platform_pool_id": "pool_dev_euc1", "platform_environment": "development"},
    }
    prefix = ensure(args.url, args.token, "/api/ipam/prefixes/", "prefix=10.64.0.0/16", prefix_payload, args.dry_run)
    print(json.dumps({"environment": "development", "vrf_id": vrf_id, "pool_id": "pool_dev_euc1", "prefix_id": prefix.get("id")}))
    print("development NetBox bootstrap complete")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except HTTPError as error:
        # NetBox reports the offending field in the response body; a bare
        # status line is not enough to act on.
        try:
            detail = error.read().decode(errors="replace").strip()
        except OSError:
            detail = ""
        raise SystemExit(f"NetBox request failed: {error}{': ' + detail if detail else ''}")
    except (URLError, TimeoutError) as error:
        raise SystemExit(f"NetBox request failed: {error}")
