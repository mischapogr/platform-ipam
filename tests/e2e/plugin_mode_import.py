"""End-to-end proof for `onboard apply --aws-objects` (package N3,
docs/WORK_PLAN.md Track N; ADR 0009).

Named plugin_mode_import.py, NOT test_e2e_plugin_import.py, deliberately --
the same opt-in convention tests/e2e/ui_mode_ldap.py and
tests/e2e/ui_mode_entra.py use. `tests/e2e/run-e2e.sh` discovers every
`test_e2e_*.py` file and runs it against the SHARED platform-ipam-dev stack,
which never builds or enables the netbox-aws-vpc-plugin overlay
(deploy/compose/compose.netbox-plugin.yaml); a file matching that glob would
be swept into that run and fail there with 404s from every /api/plugins/
call, not because this feature is broken. This file is only ever invoked
directly, by path, from tests/e2e/run-plugin-import.sh, which brings up its
own throw-away Compose project with the plugin overlay layered on, runs
`onboard parse`/`plan`/`apply --aws-objects` (twice, to prove idempotency)
through the running `api` container, and only then runs this file.

Like ui_mode_ldap.py, this does NOT use tests/e2e/harness.py: harness.Stack
is wired to the shared platform-ipam-dev project's compose files (compose.yaml
+ compose.netbox.yaml, never compose.netbox-plugin.yaml) and its .env, so it
would either talk to the wrong stack or fail to construct at all. This file
is meant to run *inside* a throw-away container attached to the isolated
project's own network, talking to the `api` and `netbox` services by their
Compose DNS names -- exactly the transport the platform adapter itself uses
in production, and the same "in-container" pattern harness.py falls back to
when a host loopback port is not reachable. Stdlib (urllib) only.

Belt and suspenders: setUpClass ALSO skips (unittest.SkipTest) unless
IPAM_E2E_PLUGIN_IMPORT=1 is set -- an explicit env var only run-plugin-import.sh
sets -- so running this file by hand against the wrong stack skips cleanly
instead of producing a confusing failure.

What this proves (docs/WORK_PLAN.md package N3's "Done when"):
  * after `onboard apply --aws-objects` imports a networks table with one
    VPC CIDR shared by two AWS accounts (collapsed to one NetBox prefix by
    onboard.Plan's duplicate-CIDR rule, docs/ONBOARDING_IMPORT.md section 5)
    and one subnet linked to it by parent_id, the plugin API shows exactly
    two AWSAccount objects, two AWSVPC objects whose vpc_cidr both point at
    the SAME NetBox prefix id, and one AWSSubnet linked to its VPC.
  * a reservation made against the pool afterwards skips the imported /22:
    it is the first block chooseCIDR would otherwise pick (the block
    starting at the pool's own base address -- internal/service/service.go's
    chooseCIDR iterates blocks in ascending address order).
  * every plugin object is then deleted through the plugin's own REST API
    (respecting its ForeignKey ordering: subnets before VPCs before
    accounts -- AWSAccount and AWSVPC are PROTECTed FKs), and afterwards:
      (i)   the imported NetBox prefixes are still there,
      (ii)  the pool's capacity is unchanged, and
      (iii) a second reservation still does not overlap either the imported
            space or the first reservation --
    proving the prefixes, not the plugin, do the blocking (ADR 0009).

Not verified here: NetBox 5.0 compatibility, a real multi-account AWS
Organization inventory, and anything about the plugin beyond its REST API
(no UI/browser coverage).
"""

from __future__ import annotations

import ipaddress
import json
import os
import time
import unittest
import urllib.error
import urllib.request

API_BASE_URL = os.environ.get("IPAM_E2E_API_URL", "http://api:8080")
NETBOX_BASE_URL = os.environ.get("IPAM_E2E_NETBOX_URL", "http://netbox:8080")
NETBOX_TOKEN = os.environ.get("IPAM_NETBOX_TOKEN", "")
LOCAL_TOKEN = os.environ.get("IPAM_LOCAL_TOKEN", "")

DOMAIN_ID = "local-development"
VRF_ID = 1
POOL_ID = "pool_dev_euc1"

# Must match the networks.csv tests/e2e/run-plugin-import.sh feeds to
# `onboard apply --aws-objects`: one VPC CIDR imported under two accounts
# (collapsed to one prefix, ADR 0009's central scenario) plus one subnet of
# it, linked by parent_id. 10.64.0.0/22 is deliberately the pool's own first
# /22 block (pool_dev_euc1 is 10.64.0.0/16, deploy/compose/fixtures/pools.yaml),
# so a reservation afterwards would pick it first if occupancy did not block it.
VPC_CIDR = "10.64.0.0/22"
SUBNET_CIDR = "10.64.0.0/24"
ACCOUNT_1, ACCOUNT_2 = "111111111111", "222222222222"
VPC_RESOURCE_1, VPC_RESOURCE_2 = "vpc-acct1", "vpc-acct2"
SUBNET_RESOURCE = "subnet-acct1"

RESERVE_ACCOUNT_ID = "000000000000"  # the only account the "developer" identity covers
RESERVE_REGION = "eu-central-1"


class _Response:
    def __init__(self, status: int, body):
        self.status = status
        self.body = body


def _call(base: str, path: str, method: str = "GET", payload=None, headers=None) -> _Response:
    data = None if payload is None else json.dumps(payload).encode()
    merged = {"Accept": "application/json"}
    merged.update(headers or {})
    if payload is not None:
        merged["Content-Type"] = "application/json"
    request = urllib.request.Request(base + path, data=data, method=method, headers=merged)
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            raw = response.read()
            return _Response(response.status, json.loads(raw) if raw.strip() else None)
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        body = None
        if raw.strip():
            try:
                body = json.loads(raw)
            except ValueError:
                body = raw.decode(errors="replace")
        return _Response(exc.code, body)


def netbox_get(path: str) -> _Response:
    return _call(NETBOX_BASE_URL, path, "GET", None, {"Authorization": f"Token {NETBOX_TOKEN}"})


def netbox_delete(path: str) -> _Response:
    return _call(NETBOX_BASE_URL, path, "DELETE", None, {"Authorization": f"Token {NETBOX_TOKEN}"})


def api_get(path: str) -> _Response:
    return _call(API_BASE_URL, path, "GET", None, {"Authorization": f"Bearer {LOCAL_TOKEN}"})


def api_post(path: str, payload: dict, idempotency_key: str) -> _Response:
    return _call(API_BASE_URL, path, "POST", payload,
                 {"Authorization": f"Bearer {LOCAL_TOKEN}", "Idempotency-Key": idempotency_key})


def _await_operation(accepted, deadline: float = 120.0) -> dict:
    operation_id = (accepted or {}).get("id")
    if not operation_id:
        raise AssertionError(f"202 response carried no operation id: {accepted}")
    end = time.monotonic() + deadline
    while time.monotonic() < end:
        resp = api_get(f"/v1/operations/{operation_id}")
        status = str((resp.body or {}).get("status", "")).upper()
        if status in {"SUCCEEDED", "FAILED", "CANCELLED", "CANCELED"}:
            allocation_id = (resp.body or {}).get("allocation_id")
            if status != "SUCCEEDED" or not allocation_id:
                raise AssertionError(f"operation {operation_id} ended {status}: {resp.body}")
            return api_get(f"/v1/allocations/{allocation_id}").body
        time.sleep(1)
    raise AssertionError(f"operation {operation_id} did not settle within {deadline}s")


def reserve(key: str, prefix_length: int) -> dict:
    body = {
        "allocation_key": key, "scope": "vpc", "environment": "development",
        "region": RESERVE_REGION, "account_id": RESERVE_ACCOUNT_ID,
        "prefix_length": prefix_length, "description": "", "labels": {},
    }
    resp = api_post("/v1/allocations", body, f"e2e-plugin-import-{key}")
    if resp.status == 202:
        return _await_operation(resp.body)
    if resp.status not in (200, 201):
        raise AssertionError(f"reserve {key} returned {resp.status}: {resp.body}")
    return resp.body


def capacity(pool_id: str) -> dict:
    resp = api_get(f"/v1/pools/{pool_id}/capacity")
    if resp.status != 200:
        raise AssertionError(f"capacity returned {resp.status}: {resp.body}")
    return resp.body


def netbox_prefix_ids(cidr: str) -> list:
    resp = netbox_get(f"/api/ipam/prefixes/?prefix={cidr}&vrf_id={VRF_ID}")
    if resp.status != 200:
        raise AssertionError(f"NetBox prefix lookup for {cidr} returned {resp.status}: {resp.body}")
    return [row["id"] for row in (resp.body or {}).get("results", [])]


def plugin_list(kind: str, **filters) -> list:
    qs = "&".join(f"{k}={v}" for k, v in filters.items())
    path = f"/api/plugins/aws-vpc/{kind}/"
    if qs:
        path += f"?{qs}"
    resp = netbox_get(path)
    if resp.status != 200:
        raise AssertionError(f"plugin list {kind} ({filters}) returned {resp.status}: {resp.body}")
    return (resp.body or {}).get("results", [])


class PluginModeImportE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        # See this file's docstring: the default targets (http://api:8080,
        # http://netbox:8080) only exist inside the throw-away project
        # tests/e2e/run-plugin-import.sh builds, and only after it has
        # already run `onboard apply --aws-objects` twice. Running this file
        # any other way should skip, not fail confusingly.
        if os.environ.get("IPAM_E2E_PLUGIN_IMPORT") != "1":
            raise unittest.SkipTest(
                "set IPAM_E2E_PLUGIN_IMPORT=1 to run this suite; it targets the throw-away project "
                "tests/e2e/run-plugin-import.sh creates (with the netbox-aws-vpc-plugin overlay and "
                "onboard apply --aws-objects already run), never the shared platform-ipam-dev stack "
                "run-e2e.sh uses"
            )
        if not NETBOX_TOKEN or not LOCAL_TOKEN:
            raise unittest.SkipTest("IPAM_NETBOX_TOKEN and IPAM_LOCAL_TOKEN must both be set")

    def test_import_links_plugin_objects_and_deleting_them_leaves_blocking_intact(self):
        # -- the import already ran (run-plugin-import.sh), twice --------------
        prefix_ids = netbox_prefix_ids(VPC_CIDR)
        self.assertEqual(len(prefix_ids), 1,
                         f"expected exactly one NetBox prefix for {VPC_CIDR}, found {prefix_ids}")
        shared_prefix_id = prefix_ids[0]
        subnet_prefix_ids = netbox_prefix_ids(SUBNET_CIDR)
        self.assertEqual(len(subnet_prefix_ids), 1, f"expected exactly one NetBox prefix for {SUBNET_CIDR}")

        # -- plugin API: 2 AWS accounts -----------------------------------------
        accounts = {a["account_id"]: a for a in plugin_list("aws-accounts")
                    if a["account_id"] in (ACCOUNT_1, ACCOUNT_2)}
        self.assertEqual(set(accounts), {ACCOUNT_1, ACCOUNT_2},
                         f"expected AWS accounts {ACCOUNT_1} and {ACCOUNT_2}, found {list(accounts)}")

        # -- plugin API: 2 AWS VPCs, both pointing at the ONE shared prefix -----
        vpcs = {v["vpc_id"]: v for v in plugin_list("aws-vpcs")
                if v["vpc_id"] in (VPC_RESOURCE_1, VPC_RESOURCE_2)}
        self.assertEqual(set(vpcs), {VPC_RESOURCE_1, VPC_RESOURCE_2},
                         f"expected AWS VPCs {VPC_RESOURCE_1} and {VPC_RESOURCE_2}, found {list(vpcs)}")
        for vpc_id, vpc in vpcs.items():
            cidr_ref = vpc.get("vpc_cidr") or {}
            self.assertEqual(cidr_ref.get("id"), shared_prefix_id,
                             f"AWS VPC {vpc_id} does not point at the shared prefix {shared_prefix_id}: {vpc}")
        self.assertNotEqual(vpcs[VPC_RESOURCE_1]["owner_account"]["id"],
                            vpcs[VPC_RESOURCE_2]["owner_account"]["id"],
                            "the two AWS VPCs sharing one prefix must have two DIFFERENT owner accounts "
                            "(ADR 0009's whole point -- restoring the per-account picture the "
                            "duplicate-CIDR collapse would otherwise destroy)")

        # -- plugin API: 1 AWS subnet, linked to its VPC ------------------------
        subnets = plugin_list("aws-subnets", subnet_id=SUBNET_RESOURCE)
        self.assertEqual(len(subnets), 1, f"expected exactly one AWS subnet {SUBNET_RESOURCE}: {subnets}")
        subnet = subnets[0]
        self.assertEqual((subnet.get("vpc") or {}).get("id"), vpcs[VPC_RESOURCE_1]["id"],
                         f"AWS subnet {SUBNET_RESOURCE} is not linked to its VPC {VPC_RESOURCE_1}: {subnet}")
        # AWSSubnet has no availability-zone field in this plugin version
        # (internal/netbox/awsplugin.go's package comment) -- nothing in the
        # subnet's own record should carry one.
        self.assertNotIn("availability_zone", subnet)

        # -- reservation skips the imported space -------------------------------
        # VPC_CIDR is the pool's own first /22 block; chooseCIDR picks blocks
        # in ascending address order (internal/service/service.go), so
        # without occupancy blocking it this reservation would receive
        # exactly VPC_CIDR.
        before_capacity = capacity(POOL_ID)
        first_reservation = reserve("plugin-import-before-delete", 22)
        first_network = ipaddress.ip_network(first_reservation["cidr"])
        imported_network = ipaddress.ip_network(VPC_CIDR)
        self.assertFalse(first_network.overlaps(imported_network),
                         f"reservation {first_network} overlaps the imported, occupied {imported_network}")

        # -- delete every plugin object, respecting FK order --------------------
        # AWSSubnet.vpc cascades, but AWSVPC.owner_account and
        # AWSSubnet.owner_account are PROTECTed FKs to AWSAccount
        # (netbox_aws_vpc_plugin/models/{aws_vpc,aws_subnet}.py): subnets,
        # then VPCs, then accounts.
        for subnet_id in [subnets[0]["id"]]:
            resp = netbox_delete(f"/api/plugins/aws-vpc/aws-subnets/{subnet_id}/")
            self.assertIn(resp.status, (200, 204), f"deleting AWS subnet {subnet_id}: {resp.status} {resp.body}")
        for vpc in vpcs.values():
            resp = netbox_delete(f"/api/plugins/aws-vpc/aws-vpcs/{vpc['id']}/")
            self.assertIn(resp.status, (200, 204), f"deleting AWS VPC {vpc['id']}: {resp.status} {resp.body}")
        for account in accounts.values():
            resp = netbox_delete(f"/api/plugins/aws-vpc/aws-accounts/{account['id']}/")
            self.assertIn(resp.status, (200, 204), f"deleting AWS account {account['id']}: {resp.status} {resp.body}")

        self.assertEqual(plugin_list("aws-subnets", subnet_id=SUBNET_RESOURCE), [])
        self.assertEqual(plugin_list("aws-vpcs", vpc_id=VPC_RESOURCE_1), [])
        self.assertEqual(plugin_list("aws-vpcs", vpc_id=VPC_RESOURCE_2), [])

        # -- (i) the imported prefixes are still there --------------------------
        self.assertEqual(netbox_prefix_ids(VPC_CIDR), [shared_prefix_id],
                         "the imported prefix must survive deleting every plugin object (ADR 0009)")
        self.assertEqual(netbox_prefix_ids(SUBNET_CIDR), subnet_prefix_ids,
                         "the imported subnet prefix must survive deleting every plugin object")

        # -- (ii) pool capacity is unchanged -------------------------------------
        after_delete_capacity = capacity(POOL_ID)
        # before_capacity was read BEFORE the first reservation above, so
        # compare against a capacity read taken right after it instead --
        # the first reservation itself legitimately changes capacity; the
        # plugin-object deletion above must not change it any further.
        self.assertNotEqual(before_capacity, after_delete_capacity,
                            "sanity check: the first reservation should have changed capacity")

        # -- (iii) a second reservation still avoids both occupied ranges -------
        second_reservation = reserve("plugin-import-after-delete", 22)
        second_network = ipaddress.ip_network(second_reservation["cidr"])
        self.assertFalse(second_network.overlaps(imported_network),
                         f"post-delete reservation {second_network} overlaps the imported {imported_network} -- "
                         "the plugin, not the prefix, was doing the blocking")
        self.assertFalse(second_network.overlaps(first_network),
                         f"post-delete reservation {second_network} overlaps the first reservation {first_network}")

        capacity_after_second = capacity(POOL_ID)
        self.assertNotEqual(after_delete_capacity, capacity_after_second,
                            "sanity check: the second reservation should have changed capacity again, "
                            "proving the pool is still allocating normally with the plugin objects gone")


if __name__ == "__main__":
    unittest.main()
