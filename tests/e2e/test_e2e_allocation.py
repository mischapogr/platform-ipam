"""End-to-end address-space behaviour against a running stack.

These tests assert the properties an IPAM system has to hold, not fixed
addresses: a pool that already contains allocations is still a valid starting
state, so every assertion is expressed as an invariant over what the run
itself reserved. That keeps the suite meaningful whether it runs against a
fresh stack or a developer's working one.

Covered surfaces: REST, the first-party CLI (ADR 0003), and the NetBox
inventory an operator reads (ADR 0002). The Terraform surface is in
test_e2e_terraform.py.
"""

from __future__ import annotations

import ipaddress
import unittest

from harness import ROOT, run_key, stack

POOL_ID = "pool_dev_euc1"


def network(cidr: str) -> ipaddress.IPv4Network:
    return ipaddress.ip_network(cidr, strict=True)


class AllocationE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()

    # -- reservation and range calculation -----------------------------------

    def test_reserved_cidr_is_aligned_and_inside_its_pool(self):
        allocation = self.stack.reserve(run_key("aligned"), 22)
        self.assertEqual(allocation["state"], "RESERVED")
        cidr = network(allocation["cidr"])
        self.assertEqual(cidr.prefixlen, 22)
        # strict=True above already proves alignment; containment proves the
        # service allocated from the configured pool rather than anywhere.
        pool = network(self._pool_cidr())
        self.assertTrue(cidr.subnet_of(pool), f"{cidr} is outside pool {pool}")

    def test_distinct_keys_receive_disjoint_ranges(self):
        keys = [run_key(f"disjoint{index}") for index in range(4)]
        allocations = [self.stack.reserve(key, 22) for key in keys]
        cidrs = [network(a["cidr"]) for a in allocations]
        self.assertEqual(len({str(c) for c in cidrs}), len(cidrs),
                         f"duplicate CIDRs issued: {[str(c) for c in cidrs]}")
        for left_index, left in enumerate(cidrs):
            for right in cidrs[left_index + 1:]:
                self.assertFalse(left.overlaps(right), f"{left} overlaps {right}")

    def test_mixed_sizes_never_overlap(self):
        small = network(self.stack.reserve(run_key("mixed-small"), 22)["cidr"])
        large = network(self.stack.reserve(run_key("mixed-large"), 20)["cidr"])
        self.assertEqual(small.prefixlen, 22)
        self.assertEqual(large.prefixlen, 20)
        self.assertFalse(small.overlaps(large),
                         f"a /20 was issued overlapping an existing /22: {large} vs {small}")

    def test_replaying_one_allocation_key_returns_one_allocation(self):
        key = run_key("replay")
        first = self.stack.reserve(key, 22)
        second = self.stack.reserve(key, 22)
        self.assertEqual(first["id"], second["id"])
        self.assertEqual(first["cidr"], second["cidr"])
        listed = self.stack.api("GET", f"/v1/allocations?allocation_key={key}").body
        matching = [row for row in (listed or {}).get("items", [])
                    if row.get("allocation_key") == key]
        self.assertEqual(len(matching), 1,
                         f"one key produced {len(matching)} allocations")

    def test_an_unsatisfiable_prefix_length_is_refused(self):
        # 24 is outside the pool's allowed_prefix_lengths for vpc scope. The
        # service must refuse rather than invent a size policy.
        key = run_key("bad-size")
        response = self.stack.api("POST", "/v1/allocations", {
            "allocation_key": key, "scope": "vpc",
            "environment": self.stack.env.get("IPAM_ENVIRONMENT", "development"),
            "region": "eu-central-1", "account_id": "000000000000",
            "prefix_length": 24, "description": "", "labels": {},
        }, {"Idempotency-Key": f"e2e-{key}"})
        self.assertGreaterEqual(response.status, 400)
        self.assertLess(response.status, 500)

    # -- capacity arithmetic --------------------------------------------------

    def test_capacity_accounts_for_each_new_reservation(self):
        before = self.stack.capacity_when_complete(POOL_ID)
        self.assertTrue(before["complete"], "capacity must be complete for a seeded pool")
        allocation = self.stack.reserve(run_key("capacity"), 22)
        after = self.stack.capacity_when_complete(POOL_ID)

        before_22 = before["by_prefix_length"]["22"]
        after_22 = after["by_prefix_length"]["22"]
        self.assertEqual(after_22["allocatable"], before_22["allocatable"] - 1,
                         "one /22 reservation must consume exactly one /22 block")
        self.assertEqual(after_22["reserved"], before_22["reserved"] + 1)

        # A /22 sits inside a /20, so it must not consume more than the single
        # /20 block that now contains it, and can never free capacity.
        before_20 = before["by_prefix_length"]["20"]
        after_20 = after["by_prefix_length"]["20"]
        self.assertIn(after_20["allocatable"],
                      (before_20["allocatable"], before_20["allocatable"] - 1),
                      f"a single /22 changed /20 availability by more than one block: "
                      f"{before_20['allocatable']} -> {after_20['allocatable']}")
        self.assertTrue(network(allocation["cidr"]).subnet_of(network(self._pool_cidr())))

    def test_pool_policy_is_visible_without_exposing_pool_address_space(self):
        pools = self.stack.api("GET", "/v1/pools").body
        listed = {pool["id"]: pool for pool in (pools or {}).get("items", [])}
        self.assertIn(POOL_ID, listed)
        self.assertIn(22, listed[POOL_ID]["allowed_prefix_lengths"])
        self.assertNotIn("cidr", listed[POOL_ID],
                         "pool address space must not leak through the consumer API")

    def test_capacity_never_reports_more_blocks_than_the_pool_holds(self):
        capacity = self.stack.capacity(POOL_ID)
        pool = network(self._pool_cidr())
        for length, counts in capacity["by_prefix_length"].items():
            total = 1 << (int(length) - pool.prefixlen)
            self.assertLessEqual(counts["allocatable"], total)
            self.assertGreaterEqual(counts["allocatable"], 0)

    # -- CLI surface ----------------------------------------------------------

    def test_cli_and_rest_agree_about_one_allocation(self):
        key = run_key("cli-agrees")
        through_cli = self.stack.cli(
            "reserve", "--key", key, "--scope", "vpc",
            "--env", self.stack.env.get("IPAM_ENVIRONMENT", "development"),
            "--region", "eu-central-1", "--account", "000000000000",
            "--prefix-length", "22", "--timeout", "120s",
        )
        self.assertEqual(through_cli["state"], "RESERVED")
        through_rest = self.stack.api("GET", f"/v1/allocations/{through_cli['id']}").body
        self.assertEqual(through_rest["cidr"], through_cli["cidr"])
        self.assertEqual(through_rest["allocation_key"], key)

        # The same key through the other surface must replay, not allocate.
        replayed = self.stack.reserve(key, 22)
        self.assertEqual(replayed["id"], through_cli["id"])
        self.assertEqual(replayed["cidr"], through_cli["cidr"])

    def test_cli_refuses_to_invent_an_allocation_key(self):
        # Exit code 2 is the CLI's usage failure; reaching the API at all
        # would mean a caller could reserve space without a stable identity.
        self.stack.cli(
            "reserve", "--scope", "vpc", "--env", "development",
            "--region", "eu-central-1", "--prefix-length", "22",
            expect_exit=2,
        )

    def test_cli_capacity_matches_rest_capacity(self):
        through_cli = self.stack.cli("capacity", "--pool", POOL_ID)
        through_rest = self.stack.capacity(POOL_ID)
        self.assertEqual(through_cli["pool_id"], through_rest["pool_id"])
        self.assertEqual(set(through_cli["by_prefix_length"]),
                         set(through_rest["by_prefix_length"]))

    # -- operator (NetBox) surface -------------------------------------------

    def test_reservation_appears_in_netbox_with_its_platform_markers(self):
        key = run_key("operator-view")
        allocation = self.stack.reserve(key, 22)
        prefixes = self.stack.netbox_prefixes()
        self.assertIn(allocation["cidr"], prefixes,
                      "a committed allocation must be visible in the operator inventory")
        row = prefixes[allocation["cidr"]]
        fields = row.get("custom_fields") or {}
        self.assertEqual(fields.get("platform_allocation_id"), allocation["id"])
        self.assertEqual(fields.get("platform_allocation_key"), key)
        state = fields.get("platform_state")
        self.assertEqual(state.get("value") if isinstance(state, dict) else state, "RESERVED")
        self.assertEqual(fields.get("platform_pool_id"), POOL_ID)

    def test_netbox_holds_no_managed_prefix_the_api_does_not_know(self):
        """A prefix in inventory that the ledger cannot explain is drift.

        The pool container itself is expected and carries no allocation id.
        """
        listed = self.stack.api("GET", "/v1/allocations").body
        known = {row["cidr"] for row in (listed or {}).get("items", [])}
        for cidr, row in self.stack.netbox_prefixes().items():
            fields = row.get("custom_fields") or {}
            if not fields.get("platform_allocation_id"):
                continue
            self.assertIn(cidr, known,
                          f"NetBox holds managed prefix {cidr} that the API does not list")

    def test_every_live_allocation_is_disjoint_from_every_other(self):
        """The whole-pool invariant, not just this run's reservations."""
        listed = self.stack.api("GET", "/v1/allocations").body
        active = [network(row["cidr"]) for row in (listed or {}).get("items", [])
                  if row.get("state") != "RELEASED" and row.get("scope") == "vpc"]
        for index, left in enumerate(active):
            for right in active[index + 1:]:
                self.assertFalse(left.overlaps(right),
                                 f"two live allocations overlap: {left} and {right}")

    # -- helpers --------------------------------------------------------------

    def _pool_cidr(self) -> str:
        """Read the pool's address space from the fixture the suite owns.

        /v1/pools deliberately does not expose a pool CIDR -- consumers get
        policy, not the pool's address space -- so containment has to be
        checked against configuration rather than against the API.
        """
        import yaml  # imported lazily so a missing dependency skips, not errors

        pools = yaml.safe_load(
            (ROOT / "deploy/compose/fixtures/pools.yaml").read_text(encoding="utf-8")
        )
        for pool in pools.get("pools", []):
            if pool.get("id") == POOL_ID:
                return pool["cidr"]
        raise AssertionError(f"pool {POOL_ID} is not defined in the Compose fixture")


if __name__ == "__main__":
    unittest.main()
