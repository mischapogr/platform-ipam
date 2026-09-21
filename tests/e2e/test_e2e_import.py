"""End-to-end tests for the onboarding import (package C7).

docs/ONBOARDING_IMPORT.md and ADR 0007: `platform-ipam onboard parse|plan|
apply` turns a table people already have into NetBox occupancy -- prefixes
and IP ranges in the domain's VRF that the allocator's Snapshot treats as
already occupied, but that carry no allocation marker and are never chosen
by `chooseCIDR`. These tests exercise that pipeline against the REAL running
stack (the `api` container's own `platform-ipam onboard` binary, docs/
ONBOARDING_IMPORT.md section 2), the same way test_e2e_allocation.py
exercises the ordinary reservation path.

Address space: allocation keys and imported prefixes are both permanent
(harness.run_key's docstring), and the ordinary allocator (exercised by
test_e2e_allocation.py, and by this suite's own reservations) always hands
out the lowest available block first. Every CIDR this suite imports is
therefore drawn from the pool's top quarter, 10.64.192.0/18 -- never touched
by that low-address-first policy -- and is additionally rotated per run
(`_shift`, keyed off IPAM_E2E_RUN_ID) so two runs against a stack that was
never reset (run-e2e.sh's normal case) do not collide either.
"""

from __future__ import annotations

import hashlib
import ipaddress
import json
import os
import shlex
import time
import unittest

from harness import ROOT, compose_argv, run, run_key, stack
# ADR 0016's removal (package M9b4) needs one more slot than this module's
# own quadrant has left (all sixteen of 10.64.192.0/18 are claimed by tests
# 1-21 above); test_e2e_adopt.py's own registry names the one slot still
# unclaimed anywhere in the pool and lends it here (see its own comment on
# SLOT_REMOVAL), the same way it already lends two slots to
# test_e2e_reservation_stuck.py.
from test_e2e_adopt import SLOT_REMOVAL, slot22 as _adopt_slot22

POOL_ID = "pool_dev_euc1"
DOMAIN_ID = "local-development"

# The pool's top quarter (deploy/compose/fixtures/pools.yaml: pool_dev_euc1
# is 10.64.0.0/16). Sixteen non-overlapping /22 "slots" live inside it,
# indexed 0..15 by their offset from the region's first address.
_TOP_REGION_BASE_OCTET = 192  # 10.64.192.0/18
_SLOT22_COUNT = 16


def network(cidr: str) -> ipaddress.IPv4Network:
    return ipaddress.ip_network(cidr, strict=True)


def _shift(modulus: int) -> int:
    """A deterministic-per-run, well-spread offset into a slot range.

    Keyed off IPAM_E2E_RUN_ID (run-e2e.sh sets a fresh timestamp per
    invocation; a manual run falls back to a wall-clock default, matching
    harness.run_key), so two different runs almost never pick the same
    slot for the same logical test, while every test within one run stays
    internally consistent (same shift, so distinct raw indices below never
    collide with each other, run or no run).
    """
    run_id = os.environ.get("IPAM_E2E_RUN_ID") or f"local{int(time.time())}"
    digest = hashlib.sha256(run_id.encode()).hexdigest()
    return int(digest, 16) % modulus


def slot22(index: int) -> str:
    """The CIDR of top-of-pool /22 slot `index` (0..15), rotated per run."""
    i = (index + _shift(_SLOT22_COUNT)) % _SLOT22_COUNT
    third_octet = _TOP_REGION_BASE_OCTET + i * 4
    return f"10.64.{third_octet}.0/22"


def outside_pool_cidr() -> str:
    """A /24 entirely outside the pool's 10.64.0.0/16, rotated per run."""
    octet = 10 + _shift(200)
    return f"10.90.{octet}.0/24"


def _networks_csv(rows: list) -> str:
    headers = ["cidr", "account_id", "region", "name", "type"]
    lines = [",".join(headers)]
    for row in rows:
        lines.append(",".join(str(row.get(h, "")) for h in headers))
    return "\n".join(lines) + "\n"


def _collector_networks_csv(rows: list, *, columns: list | None = None) -> str:
    """A networks table in the shape `scripts/aws/org-inventory.sh` writes
    since package M1b1: the two columns ADR 0014 added are present, so a
    contributor entry can carry a real association id and observation time.

    `columns` lets a test write a pre-M1b1 file -- a header genuinely without
    those columns, which is what makes ADR 0016's null provenance reachable
    end to end (internal/onboard.NetworkRow's column-presence flags tell that
    apart from a blank cell, and only a real header can exercise them).
    """
    headers = columns or ["cidr", "account_id", "region", "name", "type",
                          "resource_id", "association_id", "observed_at"]
    lines = [",".join(headers)]
    for row in rows:
        lines.append(",".join(str(row.get(h, "")) for h in headers))
    return "\n".join(lines) + "\n"


def _ranges_csv(rows: list) -> str:
    headers = ["start_address", "end_address", "description"]
    lines = [",".join(headers)]
    for row in rows:
        lines.append(",".join(str(row.get(h, "")) for h in headers))
    return "\n".join(lines) + "\n"


class ImportE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()

    # -- shared pipeline helper ------------------------------------------

    def _import(self, csv_text: str, name: str, *, plan_exit: int = 0, apply_exit=None):
        """Run parse -> plan -> apply for one table (docs/ONBOARDING_IMPORT.md
        section 2) and return (plan_report, apply_lines, batch, apply_stderr).

        `apply_exit` defaults to `plan_exit`: apply re-runs plan first and
        refuses on the same errors (design section 6), so a table plan
        refuses is expected to make apply refuse identically, not to reach
        NetBox at all.
        """
        if apply_exit is None:
            apply_exit = plan_exit
        path = f"/tmp/{run_key(name)}.json"
        batch = run_key(name)
        self.stack.onboard("parse", "-", "--out", path, stdin=csv_text)
        plan_stdout, _ = self.stack.onboard(
            "plan", path, "--domain", DOMAIN_ID, expect_exit=plan_exit)
        plan_report = json.loads(plan_stdout)
        apply_stdout, apply_stderr = self.stack.onboard(
            "apply", path, "--domain", DOMAIN_ID, "--batch", batch, expect_exit=apply_exit)
        apply_lines = [json.loads(line) for line in apply_stdout.splitlines() if line.strip()]
        return plan_report, apply_lines, batch, apply_stderr

    def _netbox_prefix_count(self) -> int:
        return (self.stack.netbox("/api/ipam/prefixes/?limit=1").body or {}).get("count", 0)

    def _pool_cidr(self) -> str:
        """Read the pool's address space from the fixture the suite owns.

        Mirrors test_e2e_allocation.py's _pool_cidr: /v1/pools deliberately
        does not expose a pool CIDR, so this has to come from configuration.
        """
        import yaml  # imported lazily so a missing dependency skips, not errors

        pools = yaml.safe_load(
            (ROOT / "deploy/compose/fixtures/pools.yaml").read_text(encoding="utf-8")
        )
        for pool in pools.get("pools", []):
            if pool.get("id") == POOL_ID:
                return pool["cidr"]
        raise AssertionError(f"pool {POOL_ID} is not defined in the Compose fixture")

    def _pool_network(self) -> ipaddress.IPv4Network:
        return network(self._pool_cidr())

    # -- 1. imported space is never allocated ----------------------------

    def test_01_imported_network_is_never_allocated(self):
        """An imported network occupies its capacity block, and the
        allocator's later reservations never overlap it (ADR 0007's whole
        premise -- Snapshot turns occupancy into an allocation guard)."""
        cidr = slot22(0)
        before = self.stack.capacity_when_complete(POOL_ID)
        self.assertTrue(before["complete"], "capacity must be complete before importing")

        csv_text = _networks_csv([{"cidr": cidr, "account_id": "000000000001",
                                    "region": "eu-central-1", "name": "t01-import"}])
        _, apply_lines, _, _ = self._import(csv_text, "t01")
        self.assertEqual(len(apply_lines), 1)
        self.assertEqual(apply_lines[0]["action"], "created")

        after = self.stack.capacity_when_complete(POOL_ID)
        self.assertTrue(after["complete"], "capacity must stay complete after importing")
        before22 = before["by_prefix_length"]["22"]
        after22 = after["by_prefix_length"]["22"]
        self.assertEqual(after22["allocatable"], before22["allocatable"] - 1,
                         "one imported /22 must remove exactly one /22 block from capacity")

        imported = network(cidr)
        for i in range(3):
            allocation = self.stack.reserve(run_key(f"t01-post22-{i}"), 22)
            self.assertFalse(network(allocation["cidr"]).overlaps(imported),
                             f"reserved {allocation['cidr']} overlaps imported {imported}")
        for i in range(2):
            allocation = self.stack.reserve(run_key(f"t01-post20-{i}"), 20)
            self.assertFalse(network(allocation["cidr"]).overlaps(imported),
                             f"reserved {allocation['cidr']} overlaps imported {imported}")

    # -- 2. imported space is occupancy, not an allocation ----------------

    def test_02_imported_prefix_is_occupancy_not_allocation(self):
        """Imported space is occupied, not owned (ADR 0007): NetBox carries
        no allocation markers on it, and /v1/allocations never lists it."""
        cidr = slot22(1)
        csv_text = _networks_csv([{"cidr": cidr, "account_id": "000000000002",
                                    "region": "eu-central-1", "name": "t02-import"}])
        _, apply_lines, batch, _ = self._import(csv_text, "t02")
        self.assertEqual(apply_lines[0]["action"], "created")

        prefixes = self.stack.netbox_prefixes()
        self.assertIn(cidr, prefixes, "imported prefix must appear in NetBox")
        row = prefixes[cidr]
        self.assertEqual(row.get("vrf", {}).get("id"), 1)
        self.assertIn("platform-ipam-imported", [t["slug"] for t in row.get("tags", [])])
        fields = row.get("custom_fields") or {}
        self.assertEqual(fields.get("platform_import_batch"), batch)
        for marker in ("platform_allocation_id", "platform_allocation_key",
                       "platform_operation_id", "platform_state"):
            self.assertFalse(fields.get(marker),
                             f"imported prefix must not carry {marker}: {fields.get(marker)!r}")

        listed = self.stack.api("GET", "/v1/allocations").body
        known_cidrs = {row["cidr"] for row in (listed or {}).get("items", [])}
        self.assertNotIn(cidr, known_cidrs,
                         "an imported network must not appear as an allocation")

    # -- 3/4. duplicate-CIDR collapse -------------------------------------

    def test_03_same_cidr_in_two_accounts_collapses_to_one_prefix(self):
        """Two accounts reporting the same VPC CIDR (design section 5)
        collapse to one prefix, or Snapshot's duplicate-(vrf,cidr) guard
        would fail the entire domain's inventory."""
        cidr = slot22(2)
        csv_text = _networks_csv([
            {"cidr": cidr, "account_id": "000000000003", "region": "eu-central-1", "name": "acct-a"},
            {"cidr": cidr, "account_id": "000000000004", "region": "eu-central-1", "name": "acct-b"},
        ])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t03")
        warnings = [f for f in plan_report["Findings"] if f["Rule"] == "duplicate-cidr"]
        self.assertEqual(len(warnings), 1)
        self.assertEqual(warnings[0]["Level"], "warning")
        self.assertEqual(len(apply_lines), 1, "two duplicate rows must collapse to one write")

        matches = [row for c, row in self.stack.netbox_prefixes().items() if c == cidr]
        self.assertEqual(len(matches), 1, f"expected exactly one prefix at {cidr}")
        description = matches[0]["description"]
        self.assertIn("000000000003", description)
        self.assertIn("000000000004", description)

    def test_04_subnet_equal_to_its_vpc_cidr_collapses_to_one_prefix(self):
        """A subnet row whose CIDR equals its own VPC's CIDR (design
        section 5) is the same duplicate-CIDR case, not a special one."""
        cidr = slot22(3)
        csv_text = _networks_csv([
            {"cidr": cidr, "account_id": "000000000005", "region": "eu-central-1",
             "name": "t04-vpc", "type": "vpc"},
            {"cidr": cidr, "account_id": "000000000005", "region": "eu-central-1",
             "name": "t04-subnet", "type": "subnet"},
        ])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t04")
        warnings = [f for f in plan_report["Findings"] if f["Rule"] == "duplicate-cidr"]
        self.assertEqual(len(warnings), 1)
        self.assertEqual(len(apply_lines), 1)

        matches = [row for c, row in self.stack.netbox_prefixes().items() if c == cidr]
        self.assertEqual(len(matches), 1, f"expected exactly one prefix at {cidr}")

    # -- 5/6. pool geometry refusals ---------------------------------------

    def test_05_row_equal_to_pool_cidr_is_refused(self):
        """The pool must match exactly one prefix, its own container
        (design section 5); a second prefix at that CIDR would fail
        Snapshot's exact-match check for the whole domain."""
        before = self._netbox_prefix_count()
        csv_text = _networks_csv([{"cidr": self._pool_cidr(), "account_id": "000000000006",
                                    "region": "eu-central-1", "name": "t05-pool-exact"}])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t05", plan_exit=3)
        errors = [f for f in plan_report["Findings"] if f["Rule"] == "pool-exact-match"]
        self.assertEqual(len(errors), 1)
        self.assertEqual(errors[0]["Level"], "error")
        self.assertEqual(apply_lines, [], "a refused apply must write nothing")
        self.assertEqual(self._netbox_prefix_count(), before,
                         "NetBox prefix count must be unchanged after a refused apply")

    def test_06_row_containing_the_pool_is_refused(self):
        """A row that contains the pool as a subnet (design section 5) is
        silently dropped by Snapshot's ancestor rule and would block
        nothing, while the pool's address space is in fact still fully
        live -- pool_dev_euc1 is 10.64.0.0/16, and 10.64.0.0/14 contains it."""
        before = self._netbox_prefix_count()
        pool = self._pool_network()
        self.assertLess(14, pool.prefixlen, "fixture assumption: the pool is narrower than /14")
        ancestor_cidr = f"{pool.network_address}/14"
        csv_text = _networks_csv([{"cidr": ancestor_cidr, "account_id": "000000000007",
                                    "region": "eu-central-1", "name": "t06-ancestor"}])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t06", plan_exit=3)
        errors = [f for f in plan_report["Findings"] if f["Rule"] == "pool-ancestor"]
        self.assertEqual(len(errors), 1)
        self.assertEqual(apply_lines, [])
        self.assertEqual(self._netbox_prefix_count(), before)

    # -- 7. overlapping a live allocation -----------------------------------

    def test_07_row_overlapping_a_live_allocation_is_refused(self):
        """Space the platform already allocated needs a human, not an import
        (design section 5): a row equal to a live allocation is refused.

        A row strictly *inside* one is the exception the 2026-09-20 amendment
        to ADR 0010 introduced (package F7). A vpc-scoped allocation's own
        subnets have to stay importable after it is adopted, or a subnet
        created later could never be brought under management at all, so such
        a row plans clean with an informational finding naming the parent
        allocation. What still refuses -- a row inside a managed *subnet* --
        is shown live by test_e2e_adopt.py's
        AdoptVPCWithSubnetsE2ETest.test_08 and exhaustively by
        internal/onboard's TestPlanStillRefusesEveryOtherOverlapWithManagedSpace.
        """
        allocation = self.stack.reserve(run_key("t07-managed"), 22)
        managed = network(allocation["cidr"])
        subnet = next(managed.subnets(new_prefix=24))

        csv_text = _networks_csv([{"cidr": str(managed), "account_id": "000000000008",
                                    "region": "eu-central-1", "name": "t07-exact"}])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t07-exact", plan_exit=3)
        errors = [f for f in plan_report["Findings"] if f["Rule"] == "overlaps-managed"]
        self.assertEqual(len(errors), 1, f"row {managed} must be refused: {plan_report}")
        self.assertEqual(apply_lines, [])

        # Plan only: this row is admissible now, and importing it would leave
        # occupancy inside another test's live allocation for the rest of the
        # run. The write set is the assertion that apply would have written it.
        nested_path = f"/tmp/{run_key('t07-subnet')}.json"
        csv_text = _networks_csv([{"cidr": str(subnet), "account_id": "000000000008",
                                    "region": "eu-central-1", "name": "t07-subnet"}])
        self.stack.onboard("parse", "-", "--out", nested_path, stdin=csv_text)
        nested_stdout, _ = self.stack.onboard("plan", nested_path, "--domain", DOMAIN_ID)
        nested_report = json.loads(nested_stdout)
        self.assertEqual([f for f in nested_report["Findings"] if f["Rule"] == "overlaps-managed"],
                         [], nested_report)
        inside = [f for f in nested_report["Findings"] if f["Rule"] == "inside-managed-vpc"]
        self.assertEqual(len(inside), 1, nested_report)
        self.assertEqual(inside[0]["Level"], "info", nested_report)
        self.assertIn(allocation["id"], inside[0]["Message"], nested_report)
        self.assertEqual([w["CIDR"] for w in nested_report["Writes"]], [str(subnet)], nested_report)

    # -- 8. re-applying is a no-op --------------------------------------

    def test_08_reapplying_the_same_table_writes_nothing(self):
        """apply re-runs plan and drops every row already present as
        unmanaged occupancy from the write set (design section 6): a
        repeated apply must create nothing and print no line for it."""
        cidr = slot22(6)
        csv_text = _networks_csv([{"cidr": cidr, "account_id": "000000000009",
                                    "region": "eu-central-1", "name": "t08-reapply"}])
        path = f"/tmp/{run_key('t08')}.json"
        batch = run_key("t08")
        self.stack.onboard("parse", "-", "--out", path, stdin=csv_text)
        self.stack.onboard("plan", path, "--domain", DOMAIN_ID)
        first_out, _ = self.stack.onboard("apply", path, "--domain", DOMAIN_ID, "--batch", batch)
        first_lines = [json.loads(l) for l in first_out.splitlines() if l.strip()]
        self.assertEqual(len(first_lines), 1)
        self.assertEqual(first_lines[0]["action"], "created")

        before = self._netbox_prefix_count()
        second_plan_out, _ = self.stack.onboard("plan", path, "--domain", DOMAIN_ID)
        second_plan = json.loads(second_plan_out)
        self.assertFalse(second_plan["Writes"],
                         "a row already present as unmanaged occupancy must drop out of the write set")
        second_apply_out, second_apply_err = self.stack.onboard(
            "apply", path, "--domain", DOMAIN_ID, "--batch", batch)
        second_lines = [json.loads(l) for l in second_apply_out.splitlines() if l.strip()]
        self.assertEqual(second_lines, [], "a repeated apply must print no line for an unchanged row")
        self.assertIn("0 created", second_apply_err)
        self.assertEqual(self._netbox_prefix_count(), before, "a repeated apply must write nothing")

    # -- 9. account id repair --------------------------------------------

    def test_09_account_id_without_leading_zeros_is_padded_and_reaches_netbox(self):
        """Excel-dropped leading zeros (design section 4) are repaired
        with a warning, and the padded 12-digit id -- not the short one --
        reaches NetBox's platform_aws_account_id."""
        cidr = slot22(5)
        stripped = "45678901"
        padded = "0" * (12 - len(stripped)) + stripped
        csv_text = _networks_csv([{"cidr": cidr, "account_id": stripped,
                                    "region": "eu-central-1", "name": "t09-padded"}])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t09")
        normalize_findings = [f for f in plan_report["Findings"] if f["Rule"] == "normalize"]
        self.assertTrue(
            any("left-padded" in f["Message"] and padded in f["Message"] for f in normalize_findings),
            f"no normalize finding reports the padded account id: {normalize_findings}")
        self.assertEqual(apply_lines[0]["action"], "created")

        row = self.stack.netbox_prefixes()[cidr]
        fields = row.get("custom_fields") or {}
        self.assertEqual(fields.get("platform_aws_account_id"), padded)

    # -- 10. outside every pool -------------------------------------------

    def test_10_row_outside_every_pool_is_imported_without_touching_capacity(self):
        """A network outside every configured pool is harmless and still
        imported (design section 5), but must not move the pool's own
        capacity numbers."""
        cidr = outside_pool_cidr()
        before = self.stack.capacity_when_complete(POOL_ID)
        csv_text = _networks_csv([{"cidr": cidr, "account_id": "000000000010",
                                    "region": "eu-central-1", "name": "t10-outside"}])
        plan_report, apply_lines, _, _ = self._import(csv_text, "t10")
        findings = [f for f in plan_report["Findings"] if f["Rule"] == "outside-pool"]
        self.assertEqual(len(findings), 1)
        self.assertEqual(findings[0]["Level"], "info")
        self.assertEqual(len(apply_lines), 1)
        self.assertEqual(apply_lines[0]["action"], "created")

        after = self.stack.capacity_when_complete(POOL_ID)
        self.assertEqual(after["by_prefix_length"], before["by_prefix_length"],
                         "a network outside the pool must not change pool capacity")

    # -- 11. ranges ---------------------------------------------------------

    def test_11_range_inside_pool_is_avoided_a_range_spanning_the_pool_is_refused(self):
        """A start/end range inside the pool becomes a NetBox IP range
        (with the import tag) that later reservations avoid; a range
        spanning the whole pool would mark it entirely occupied (design
        section 5, mirroring internal/netbox/occupancy.go's
        refusePoolSpan) and is refused instead."""
        base = network(slot22(8))
        start = str(base.network_address)
        end = str(base.network_address + 63)
        csv_text = _ranges_csv([{"start_address": start, "end_address": end,
                                  "description": "t11-range"}])
        _, apply_lines, _, _ = self._import(csv_text, "t11")
        self.assertEqual(len(apply_lines), 1)
        self.assertEqual(apply_lines[0]["kind"], "range")
        self.assertEqual(apply_lines[0]["action"], "created")

        ranges = (self.stack.netbox("/api/ipam/ip-ranges/?limit=200").body or {}).get("results", [])
        matches = [r for r in ranges
                   if r.get("start_address", "").split("/")[0] == start
                   and r.get("end_address", "").split("/")[0] == end]
        self.assertEqual(len(matches), 1, f"expected exactly one IP range {start}-{end}")
        self.assertIn("platform-ipam-imported", [t["slug"] for t in matches[0].get("tags", [])])

        imported_networks = list(ipaddress.summarize_address_range(
            ipaddress.ip_address(start), ipaddress.ip_address(end)))
        for i in range(3):
            allocation = self.stack.reserve(run_key(f"t11-post-{i}"), 22)
            reserved = network(allocation["cidr"])
            for imported_net in imported_networks:
                self.assertFalse(reserved.overlaps(imported_net),
                                 f"reserved {reserved} overlaps imported range {start}-{end}")

        pool = self._pool_network()
        span_csv = _ranges_csv([{"start_address": str(pool.network_address),
                                  "end_address": str(pool.broadcast_address),
                                  "description": "t11-span"}])
        span_report, span_apply_lines, _, _ = self._import(span_csv, "t11-span", plan_exit=3)
        span_errors = [f for f in span_report["Findings"] if f["Rule"] == "range-spans-pool"]
        self.assertEqual(len(span_errors), 1)
        self.assertEqual(span_apply_lines, [])

    # -- 12. whole-system invariants ---------------------------------------

    def test_12_whole_system_invariants_hold_after_import(self):
        """After everything above, the invariants test_e2e_allocation.py
        protects for ordinary reservations must still hold: every managed
        prefix is known to the API, every live allocation is pairwise
        disjoint, and the pool's capacity snapshot is still complete."""
        listed = self.stack.api("GET", "/v1/allocations").body
        known = {row["cidr"] for row in (listed or {}).get("items", [])}
        for cidr, row in self.stack.netbox_prefixes().items():
            fields = row.get("custom_fields") or {}
            if not fields.get("platform_allocation_id"):
                continue
            self.assertIn(cidr, known,
                          f"NetBox holds managed prefix {cidr} that the API does not list")

        active = [network(row["cidr"]) for row in (listed or {}).get("items", [])
                  if row.get("state") != "RELEASED" and row.get("scope") == "vpc"]
        for i, left in enumerate(active):
            for right in active[i + 1:]:
                self.assertFalse(left.overlaps(right),
                                 f"two live allocations overlap: {left} and {right}")

        capacity = self.stack.capacity(POOL_ID)
        self.assertTrue(capacity["complete"], "pool capacity must remain complete after import")

    # -- 13/14/15. contributors on a shared prefix (ADR 0016, M9b1) -------

    def _contributors(self, cidr: str) -> list:
        """The contributor list NetBox holds for one imported prefix.

        Asserts the field arrives as a structured JSON array rather than a
        string: package M9b1 measured that the pinned NetBox's `json`
        custom-field type round-trips as an array through the adapter's own
        request path, and this is that measurement repeated through the real
        binary rather than through a probe.
        """
        row = self.stack.netbox_prefixes()[cidr]
        value = (row.get("custom_fields") or {}).get("platform_import_contributors")
        self.assertIsInstance(
            value, list,
            f"platform_import_contributors must read back as a JSON array, got {type(value).__name__}: {value!r}")
        return value

    def test_13_two_vpcs_of_two_accounts_at_one_cidr_name_both_contributors(self):
        """The gap M9 exists for: several VPCs share one imported prefix and
        the collapse used to forget all but one of them (ADR 0016). The
        prefix now carries one entry per source row, each with ADR 0014's
        resource identity -- the same string `onboard assess` puts on a
        conflict, so a reviewer can join the two without a mapping table.
        """
        cidr = slot22(11)
        csv_text = _collector_networks_csv([
            {"cidr": cidr, "account_id": "000000000021", "region": "eu-central-1",
             "name": "t13-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e13a",
             "association_id": "vpc-cidr-assoc-0e2e13a", "observed_at": "2026-09-22T10:00:00Z"},
            {"cidr": cidr, "account_id": "000000000022", "region": "eu-central-1",
             "name": "t13-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e13b",
             "association_id": "vpc-cidr-assoc-0e2e13b", "observed_at": "2026-09-22T10:05:00Z"},
        ])
        plan_report, apply_lines, batch, _ = self._import(csv_text, "t13")
        self.assertEqual(len(apply_lines), 1, "two rows at one CIDR must still collapse to one write")
        self.assertEqual(apply_lines[0]["action"], "created")
        self.assertEqual(
            len([f for f in plan_report["Findings"] if f["Rule"] == "duplicate-cidr"]), 1)

        contributors = self._contributors(cidr)
        self.assertEqual(len(contributors), 2,
                         f"one prefix must name both VPCs: {contributors}")
        by_resource = {c["resource_id"]: c for c in contributors}
        self.assertEqual(sorted(by_resource), ["vpc-0e2e13a", "vpc-0e2e13b"])

        first = by_resource["vpc-0e2e13a"]
        self.assertEqual(first["identity"],
                         f"aws:000000000021:eu-central-1:vpc-0e2e13a:{cidr}:vpc-cidr-assoc-0e2e13a")
        self.assertEqual(first["account_id"], "000000000021")
        self.assertEqual(first["region"], "eu-central-1")
        self.assertEqual(first["type"], "vpc")
        self.assertEqual(first["association_id"], "vpc-cidr-assoc-0e2e13a")
        self.assertEqual(first["observed_at"], "2026-09-22T10:00:00Z")
        self.assertEqual(first["first_seen_batch"], batch)
        self.assertEqual(first["last_seen_batch"], batch)

        # Each entry names its own input record -- ADR 0014's traceability
        # property, applied to the inventory. The row numbers are the ones an
        # operator sees in the file, so the header counts and the first data
        # row is 2 (internal/onboard.shiftRows, package C2's review fix).
        self.assertEqual(
            sorted((c["source_row"], c["resource_id"]) for c in contributors),
            [(2, "vpc-0e2e13a"), (3, "vpc-0e2e13b")])
        self.assertEqual(by_resource["vpc-0e2e13b"]["observed_at"], "2026-09-22T10:05:00Z")

        # The structured record is what the AWS custom fields could not hold:
        # two rows disagreeing about the account blank them deliberately.
        fields = self.stack.netbox_prefixes()[cidr].get("custom_fields") or {}
        self.assertFalse(fields.get("platform_aws_account_id"),
                         "two accounts at one CIDR must still leave the single-valued AWS field unset")
        self.assertEqual(fields.get("platform_import_batch"), batch)
        self.assertIn("platform-ipam-imported",
                      [t["slug"] for t in self.stack.netbox_prefixes()[cidr].get("tags", [])])

    def test_14_a_reimport_naming_a_new_vpc_writes_nothing(self):
        """M9b1 has no update path. ADR 0016 gives the refresh to package
        M9b3, so a second import that names a VPC the prefix does not carry
        must leave the prefix -- and its contributor list -- exactly as the
        first import wrote it, and must make no NetBox write at all."""
        cidr = slot22(12)
        rows = [{"cidr": cidr, "account_id": "000000000023", "region": "eu-central-1",
                 "name": "t14-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e14a",
                 "association_id": "vpc-cidr-assoc-0e2e14a", "observed_at": "2026-09-22T10:00:00Z"}]
        _, apply_lines, first_batch, _ = self._import(_collector_networks_csv(rows), "t14")
        self.assertEqual(apply_lines[0]["action"], "created")

        before_row = self.stack.netbox_prefixes()[cidr]
        before = self._contributors(cidr)
        self.assertEqual(len(before), 1)

        rows.append({"cidr": cidr, "account_id": "000000000024", "region": "eu-central-1",
                     "name": "t14-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e14b",
                     "association_id": "vpc-cidr-assoc-0e2e14b", "observed_at": "2026-09-22T11:00:00Z"})
        second_path = f"/tmp/{run_key('t14-second')}.json"
        second_batch = run_key("t14-second")
        self.stack.onboard("parse", "-", "--out", second_path, stdin=_collector_networks_csv(rows))
        plan_out, _ = self.stack.onboard("plan", second_path, "--domain", DOMAIN_ID)
        plan_report = json.loads(plan_out)
        self.assertFalse(plan_report["Writes"],
                         "the CIDR is already unmanaged occupancy, so it must drop out of the write set")

        prefix_count = self._netbox_prefix_count()
        apply_out, apply_err = self.stack.onboard(
            "apply", second_path, "--domain", DOMAIN_ID, "--batch", second_batch)
        self.assertEqual([l for l in apply_out.splitlines() if l.strip()], [])
        self.assertIn("0 created", apply_err)
        self.assertEqual(self._netbox_prefix_count(), prefix_count)

        after_row = self.stack.netbox_prefixes()[cidr]
        self.assertEqual(self._contributors(cidr), before,
                         "a re-import must not add, remove or reorder a contributor: there is no update path yet")
        self.assertEqual((after_row.get("custom_fields") or {}).get("platform_import_batch"),
                         first_batch, "the batch must still be the first import's")
        self.assertEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                         "NetBox recorded a write for a prefix nothing should have touched")

    def test_15_a_pre_m1b1_file_records_null_provenance(self):
        """ADR 0016: a contributor from a file written before the collector
        gained `association_id` and `observed_at` has unknown provenance, it
        carries null there, and it can never take part in a removal. Nothing
        may be invented to fill the gap."""
        cidr = slot22(13)
        csv_text = _collector_networks_csv(
            [{"cidr": cidr, "account_id": "000000000025", "region": "eu-central-1",
              "name": "t15-legacy", "type": "vpc", "resource_id": "vpc-0e2e15a"}],
            columns=["cidr", "account_id", "region", "name", "type", "resource_id"])
        _, apply_lines, _, _ = self._import(csv_text, "t15")
        self.assertEqual(apply_lines[0]["action"], "created")

        contributors = self._contributors(cidr)
        self.assertEqual(len(contributors), 1)
        entry = contributors[0]
        self.assertIsNone(entry["observed_at"],
                          "a file with no observed_at column must record null, never a guess")
        self.assertIsNone(entry["association_id"],
                          "a file with no association_id column must record null")
        # Without an association the identity is ADR 0014's four-part form.
        self.assertEqual(entry["identity"],
                         f"aws:000000000025:eu-central-1:vpc-0e2e15a:{cidr}")

    # -- 16/17. Plan's contributor comparison (ADR 0016, M9b2) ------------

    def test_16_replanning_with_one_vpc_missing_warns_contributor_absent(self):
        """ADR 0016, package M9b2: a table is a scope, not a census. Re-planning
        the two-VPC prefix from a table naming only one of them must warn that
        the other is a recorded contributor this table does not name -- and
        must write nothing: absence from one table is never evidence a VPC is
        gone."""
        cidr = slot22(14)
        rows = [
            {"cidr": cidr, "account_id": "000000000026", "region": "eu-central-1",
             "name": "t16-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e16a",
             "association_id": "vpc-cidr-assoc-0e2e16a", "observed_at": "2026-09-22T10:00:00Z"},
            {"cidr": cidr, "account_id": "000000000027", "region": "eu-central-1",
             "name": "t16-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e16b",
             "association_id": "vpc-cidr-assoc-0e2e16b", "observed_at": "2026-09-22T10:05:00Z"},
        ]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t16")
        self.assertEqual(apply_lines[0]["action"], "created")
        before_row = self.stack.netbox_prefixes()[cidr]
        before = self._contributors(cidr)
        self.assertEqual(len(before), 2)

        # A second table naming vpc-a only -- e.g. an operator filtered the
        # file, or a --networks input covering one account only.
        second_path = f"/tmp/{run_key('t16-second')}.json"
        self.stack.onboard("parse", "-", "--out", second_path,
                            stdin=_collector_networks_csv([rows[0]]))
        plan_out, _ = self.stack.onboard("plan", second_path, "--domain", DOMAIN_ID)
        plan_report = json.loads(plan_out)
        self.assertFalse(plan_report["Writes"],
                          "the CIDR is already unmanaged occupancy, so it must drop out of the write set")

        absent = [f for f in plan_report["Findings"] if f["Rule"] == "contributor-absent"]
        self.assertEqual(len(absent), 1, plan_report["Findings"])
        self.assertEqual(absent[0]["Level"], "warning")
        vpc_b_identity = f"aws:000000000027:eu-central-1:vpc-0e2e16b:{cidr}:vpc-cidr-assoc-0e2e16b"
        self.assertIn(vpc_b_identity, absent[0]["Message"])
        self.assertFalse(
            [f for f in plan_report["Findings"] if f["Rule"] == "contributor-new"],
            "vpc-a is already a recorded contributor; it must not be reported new")

        # Applying this table must still write nothing -- M9b2 never removes,
        # never refreshes: that is package M9b3's --refresh.
        second_batch = run_key("t16-second-apply")
        apply_out, apply_err = self.stack.onboard(
            "apply", second_path, "--domain", DOMAIN_ID, "--batch", second_batch)
        self.assertEqual([l for l in apply_out.splitlines() if l.strip()], [])
        self.assertIn("0 created", apply_err)
        self.assertEqual(self._contributors(cidr), before,
                          "a table missing a contributor must never remove or reorder it")
        after_row = self.stack.netbox_prefixes()[cidr]
        self.assertEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                          "NetBox recorded a write for a prefix nothing should have touched")

    def test_17_replanning_with_a_third_vpc_raises_contributor_new_and_still_writes_nothing(self):
        """ADR 0016, package M9b2: naming a VPC the prefix does not carry yet
        raises contributor-new -- the only finding a later refresh (M9b3) acts
        on -- but M9b2 itself has no update path, so apply must still write
        nothing at all."""
        cidr = slot22(15)
        rows = [
            {"cidr": cidr, "account_id": "000000000028", "region": "eu-central-1",
             "name": "t17-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e17a",
             "association_id": "vpc-cidr-assoc-0e2e17a", "observed_at": "2026-09-22T10:00:00Z"},
            {"cidr": cidr, "account_id": "000000000029", "region": "eu-central-1",
             "name": "t17-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e17b",
             "association_id": "vpc-cidr-assoc-0e2e17b", "observed_at": "2026-09-22T10:05:00Z"},
        ]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t17")
        self.assertEqual(apply_lines[0]["action"], "created")
        before_row = self.stack.netbox_prefixes()[cidr]
        before = self._contributors(cidr)
        self.assertEqual(len(before), 2)

        rows.append({"cidr": cidr, "account_id": "000000000030", "region": "eu-central-1",
                     "name": "t17-vpc-c", "type": "vpc", "resource_id": "vpc-0e2e17c",
                     "association_id": "vpc-cidr-assoc-0e2e17c", "observed_at": "2026-09-22T11:00:00Z"})
        third_path = f"/tmp/{run_key('t17-third')}.json"
        self.stack.onboard("parse", "-", "--out", third_path, stdin=_collector_networks_csv(rows))
        plan_out, _ = self.stack.onboard("plan", third_path, "--domain", DOMAIN_ID)
        plan_report = json.loads(plan_out)
        self.assertFalse(plan_report["Writes"],
                          "the CIDR is already unmanaged occupancy, so it must drop out of the write set")

        new = [f for f in plan_report["Findings"] if f["Rule"] == "contributor-new"]
        self.assertEqual(len(new), 1, plan_report["Findings"])
        self.assertEqual(new[0]["Level"], "info")
        vpc_c_identity = f"aws:000000000030:eu-central-1:vpc-0e2e17c:{cidr}:vpc-cidr-assoc-0e2e17c"
        self.assertIn(vpc_c_identity, new[0]["Message"])
        self.assertFalse(
            [f for f in plan_report["Findings"] if f["Rule"] == "contributor-absent"],
            "every one of the first two VPCs is still named in this table")

        third_batch = run_key("t17-third-apply")
        apply_out, apply_err = self.stack.onboard(
            "apply", third_path, "--domain", DOMAIN_ID, "--batch", third_batch)
        self.assertEqual([l for l in apply_out.splitlines() if l.strip()], [],
                          "no refresh path exists yet (package M9b3): apply must still write nothing")
        self.assertIn("0 created", apply_err)
        self.assertEqual(self._contributors(cidr), before,
                          "vpc-c must not have been added: M9b2 never writes")
        after_row = self.stack.netbox_prefixes()[cidr]
        self.assertEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                          "NetBox recorded a write for a prefix nothing should have touched")


    # -- 18/19/20/21. the additive refresh (ADR 0016, M9b3) ---------------

    def _refresh(self, path: str, batch: str, *, expect_exit: int = 0):
        return self.stack.onboard(
            "apply", path, "--domain", DOMAIN_ID, "--batch", batch,
            "--source", "networks.csv", "--refresh", expect_exit=expect_exit)

    def test_18_refresh_adds_a_third_contributor_with_exactly_one_patch(self):
        """ADR 0016, package M9b3: `apply --refresh` is occupancy's first
        update path. Naming a third VPC on the two-VPC prefix this suite's
        own test_13 shape produces must add exactly that one contributor,
        touch nothing else, and move NetBox's own last_updated exactly
        once."""
        cidr = slot22(4)
        rows = [
            {"cidr": cidr, "account_id": "000000000031", "region": "eu-central-1",
             "name": "t18-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e18a",
             "association_id": "vpc-cidr-assoc-0e2e18a", "observed_at": "2026-09-22T10:00:00Z"},
            {"cidr": cidr, "account_id": "000000000032", "region": "eu-central-1",
             "name": "t18-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e18b",
             "association_id": "vpc-cidr-assoc-0e2e18b", "observed_at": "2026-09-22T10:05:00Z"},
        ]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t18")
        self.assertEqual(apply_lines[0]["action"], "created")
        before_row = self.stack.netbox_prefixes()[cidr]
        before_contributors = self._contributors(cidr)
        self.assertEqual(len(before_contributors), 2)
        before_description = before_row["description"]

        rows.append({"cidr": cidr, "account_id": "000000000033", "region": "eu-central-1",
                     "name": "t18-vpc-c", "type": "vpc", "resource_id": "vpc-0e2e18c",
                     "association_id": "vpc-cidr-assoc-0e2e18c", "observed_at": "2026-09-22T11:00:00Z"})
        path = f"/tmp/{run_key('t18-third')}.json"
        batch = run_key("t18-third-refresh")
        self.stack.onboard("parse", "-", "--out", path, stdin=_collector_networks_csv(rows))
        _, refresh_err = self._refresh(path, batch)
        self.assertIn("--refresh: 1 written (0 reconstructed), 0 unchanged, 1 total", refresh_err, refresh_err)

        after_row = self.stack.netbox_prefixes()[cidr]
        self.assertNotEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                            "a refresh that added a contributor must move NetBox's own last_updated")
        self.assertEqual(after_row["description"], before_description,
                         "a refresh must never touch the description")

        after_contributors = self._contributors(cidr)
        self.assertEqual(len(after_contributors), 3, after_contributors)
        by_resource = {c["resource_id"]: c for c in after_contributors}
        self.assertEqual(sorted(by_resource), ["vpc-0e2e18a", "vpc-0e2e18b", "vpc-0e2e18c"])
        # The write happened (the set grew by vpc-c), so ADR 0016's merge
        # rule applies to every identity this table names, not only the new
        # one: vpc-a and vpc-b were named again, so their last_seen_batch
        # moves to this call's batch and their observed_at is taken from the
        # table (unchanged in value here, since the table repeated the same
        # timestamp) -- but first_seen_batch, and every other field, stays
        # exactly what the ORIGINAL import wrote (internal/netbox/refresh.go's
        # mergeContributors: "identity present in both ... keep everything
        # else from existing").
        before_by_resource = {c["resource_id"]: c for c in before_contributors}
        for resource in ("vpc-0e2e18a", "vpc-0e2e18b"):
            before = dict(before_by_resource[resource])
            got = dict(by_resource[resource])
            self.assertEqual(got.pop("last_seen_batch"), batch,
                             f"{resource} was named again by a table that caused a write; its last_seen_batch must move")
            before.pop("last_seen_batch")
            self.assertEqual(got, before,
                             f"{resource}: every field but last_seen_batch (and observed_at, unchanged here) must be untouched")
        self.assertEqual(by_resource["vpc-0e2e18a"]["first_seen_batch"], before_by_resource["vpc-0e2e18a"]["first_seen_batch"],
                         "first_seen_batch must never move once written")
        self.assertEqual(by_resource["vpc-0e2e18c"]["first_seen_batch"], batch)
        self.assertEqual(by_resource["vpc-0e2e18c"]["last_seen_batch"], batch)

        after_fields = after_row.get("custom_fields") or {}
        self.assertFalse(after_fields.get("platform_import_contributors_reconstructed"),
                         "a prefix that already carried a list must never be flagged reconstructed")

    def test_19_a_second_refresh_over_the_same_table_makes_no_patch(self):
        """The HEAD property ADR 0016 names: a write happens only when the
        contributor SET changes, never because an observation time moved.
        Refreshing the SAME table twice must PATCH once and then never
        again, even though every row still names the same VPCs."""
        cidr = slot22(7)
        rows = [
            {"cidr": cidr, "account_id": "000000000034", "region": "eu-central-1",
             "name": "t19-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e19a",
             "association_id": "vpc-cidr-assoc-0e2e19a", "observed_at": "2026-09-22T10:00:00Z"},
        ]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t19")
        self.assertEqual(apply_lines[0]["action"], "created")

        rows.append({"cidr": cidr, "account_id": "000000000035", "region": "eu-central-1",
                     "name": "t19-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e19b",
                     "association_id": "vpc-cidr-assoc-0e2e19b", "observed_at": "2026-09-22T10:05:00Z"})
        path = f"/tmp/{run_key('t19')}.json"
        self.stack.onboard("parse", "-", "--out", path, stdin=_collector_networks_csv(rows))
        _, first_refresh_err = self._refresh(path, run_key("t19-refresh-1"))
        self.assertIn("--refresh: 1 written", first_refresh_err, first_refresh_err)
        row_after_first = self.stack.netbox_prefixes()[cidr]
        contributors_after_first = self._contributors(cidr)
        self.assertEqual(len(contributors_after_first), 2)

        # Same table again, a later batch name, and -- were observed_at
        # alone enough to trigger a write -- a strictly newer timestamp for
        # both rows. The identity SET is unchanged, so this must write
        # nothing at all.
        rows[0]["observed_at"] = "2026-09-23T00:00:00Z"
        rows[1]["observed_at"] = "2026-09-23T00:00:00Z"
        second_path = f"/tmp/{run_key('t19-second')}.json"
        self.stack.onboard("parse", "-", "--out", second_path, stdin=_collector_networks_csv(rows))
        _, second_refresh_err = self._refresh(second_path, run_key("t19-refresh-2"))
        self.assertIn("--refresh: 0 written (0 reconstructed), 1 unchanged, 1 total", second_refresh_err,
                      second_refresh_err)

        row_after_second = self.stack.netbox_prefixes()[cidr]
        self.assertEqual(row_after_second.get("last_updated"), row_after_first.get("last_updated"),
                         "an unchanged contributor set must never move last_updated")
        self.assertEqual(self._contributors(cidr), contributors_after_first,
                         "an unchanged contributor set must leave the stored list byte-identical")

    def test_20_a_prefix_imported_before_the_field_gains_a_reconstructed_list(self):
        """ADR 0016: a prefix imported before this record carries no
        contributor list at all. `--refresh` populates one from the table
        and flags it reconstructed, permanently -- the reconstructed list
        and the one an import would have written are indistinguishable
        afterwards, which is the whole reason the flag exists.

        Seeded directly through the NetBox REST API rather than through
        `onboard apply`, exactly as test_e2e_reservation_stuck.py's own
        out-of-band obstacle is: this is what a real prefix imported before
        package M9b1 ever existed looks like -- tagged, unowned, and
        carrying no platform_import_contributors key at all.
        """
        cidr = slot22(9)
        # VRF 1 is the development domain's configured VRF
        # (deploy/compose/fixtures/pools.yaml), the same constant
        # test_e2e_reservation_stuck.py's own out-of-band obstacle uses.
        seeded = self.stack.netbox_as(
            self.stack.netbox_token, "POST", "/api/ipam/prefixes/",
            {"prefix": cidr, "vrf": 1, "status": "active",
             "tags": [{"slug": "platform-ipam-imported"}],
             "custom_fields": {"platform_import_batch": "pre-adr-0016",
                               "platform_import_source": "legacy-export.csv"}})
        self.assertEqual(seeded.status, 201, seeded.body)

        rows = [{"cidr": cidr, "account_id": "000000000036", "region": "eu-central-1",
                 "name": "t20-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e20a",
                 "association_id": "vpc-cidr-assoc-0e2e20a", "observed_at": "2026-09-22T10:00:00Z"}]
        path = f"/tmp/{run_key('t20')}.json"
        self.stack.onboard("parse", "-", "--out", path, stdin=_collector_networks_csv(rows))
        _, refresh_err = self._refresh(path, run_key("t20-refresh"))
        self.assertIn("--refresh: 1 written (1 reconstructed), 0 unchanged, 1 total", refresh_err, refresh_err)

        row = self.stack.netbox_prefixes()[cidr]
        fields = row.get("custom_fields") or {}
        self.assertTrue(fields.get("platform_import_contributors_reconstructed"),
                        "a reconstructed list must carry the permanent flag")
        self.assertEqual(fields.get("platform_import_batch"), "pre-adr-0016",
                         "a refresh must never touch platform_import_batch")
        contributors = self._contributors(cidr)
        self.assertEqual(len(contributors), 1)
        self.assertEqual(contributors[0]["resource_id"], "vpc-0e2e20a")

    def test_21_an_owned_prefix_in_the_range_is_already_unreachable_from_a_refresh(self):
        """ADR 0016 names this "a pleasant result in the existing rules": a
        table row for an adopted (owned) network never even reaches
        RefreshOccupancy, because `overlapsManaged` already refuses it as a
        PLAN error (`RuleOverlapsManaged`) before apply ever writes -- the
        same refusal an ordinary re-import gets, since `childOfManagedVPC`
        requires strictly longer bits and an equal CIDR is not a child.
        `--refresh` inherits that refusal rather than needing its own.

        Ownership is simulated by writing the platform's own allocation
        markers directly through the NetBox REST API -- the same way this
        suite's test_02 reads them back and test_e2e_reservation_stuck.py
        seeds its own out-of-band obstacle -- rather than running the full
        `adopt` pipeline (covered end to end by test_e2e_adopt.py).
        RefreshOccupancy's OWN "any ownership field" refusal, for a caller
        that reaches it directly, is covered by
        internal/netbox/refresh_test.go's
        TestRefreshOccupancyNeverTouchesAnAdoptedPrefix and
        TestRefreshOccupancyRefusesAnyOwnershipFieldNotJustTheImportTag.
        """
        cidr = slot22(10)
        rows = [{"cidr": cidr, "account_id": "000000000037", "region": "eu-central-1",
                 "name": "t21-vpc-a", "type": "vpc", "resource_id": "vpc-0e2e21a",
                 "association_id": "vpc-cidr-assoc-0e2e21a", "observed_at": "2026-09-22T10:00:00Z"}]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t21")
        self.assertEqual(apply_lines[0]["action"], "created")
        prefix_id = self.stack.netbox_prefixes()[cidr]["id"]

        owned = self.stack.netbox_as(
            self.stack.netbox_token, "PATCH", f"/api/ipam/prefixes/{prefix_id}/",
            {"custom_fields": {
                "platform_allocation_id": "alloc-t21-simulated",
                "platform_allocation_key": "t21-simulated",
                "platform_operation_id": "op-t21-simulated",
                "platform_state": "RESERVED",
            }})
        self.assertEqual(owned.status, 200, owned.body)
        # This marker is never backed by a real ledger allocation, and
        # imported prefixes are permanent (this module's own docstring):
        # left in place, it would carry platform_allocation_id forever while
        # never appearing in GET /v1/allocations, which is exactly what
        # test_12's whole-system invariant (and the wider suite's
        # test_netbox_holds_no_managed_prefix_the_api_does_not_know) exists
        # to catch -- in a run long after this one, at whatever CIDR this
        # slot happens to rotate onto next. Clearing it back is this test's
        # own responsibility, the same way _AdoptFixtureCase's classes
        # release every allocation and restore the cloud fixture.
        try:
            before_row = self.stack.netbox_prefixes()[cidr]

            rows.append({"cidr": cidr, "account_id": "000000000038", "region": "eu-central-1",
                         "name": "t21-vpc-b", "type": "vpc", "resource_id": "vpc-0e2e21b",
                         "association_id": "vpc-cidr-assoc-0e2e21b", "observed_at": "2026-09-22T11:00:00Z"})
            path = f"/tmp/{run_key('t21')}.json"
            self.stack.onboard("parse", "-", "--out", path, stdin=_collector_networks_csv(rows))
            plan_stdout, _ = self.stack.onboard("plan", path, "--domain", DOMAIN_ID, expect_exit=3)
            plan_report = json.loads(plan_stdout)
            overlap_errors = [f for f in plan_report["Findings"] if f["Rule"] == "overlaps-managed"]
            self.assertEqual(len(overlap_errors), 1, plan_report["Findings"])

            # apply --refresh re-runs plan first, exactly as apply always
            # has, and refuses on the same error -- never reaching
            # RefreshOccupancy at all.
            _, refresh_err = self._refresh(path, run_key("t21-refresh"), expect_exit=3)
            self.assertIn("refusing to write; plan reported errors above", refresh_err, refresh_err)

            after_row = self.stack.netbox_prefixes()[cidr]
            self.assertEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                             "a refused refresh must write nothing")
        finally:
            cleared = self.stack.netbox_as(
                self.stack.netbox_token, "PATCH", f"/api/ipam/prefixes/{prefix_id}/",
                {"custom_fields": {
                    "platform_allocation_id": None, "platform_allocation_key": None,
                    "platform_operation_id": None, "platform_state": None,
                }})
            self.assertEqual(cleared.status, 200, cleared.body)
            restored_fields = self.stack.netbox_prefixes()[cidr].get("custom_fields") or {}
            for marker in ("platform_allocation_id", "platform_allocation_key",
                          "platform_operation_id", "platform_state"):
                self.assertFalse(restored_fields.get(marker),
                                 f"simulated ownership marker {marker} was not cleared: {restored_fields}")

    # -- 22/23. removal (ADR 0016, package M9b4) --------------------------
    #
    # SLOT_REMOVAL is one /22, lent whole by test_e2e_adopt.py (see its own
    # comment); onboard remove needs no minimum prefix length, so this suite
    # splits the one lent slot into two /24 sub-blocks rather than asking for
    # a second one: sub-block 0 is the progressive two-VPC-then-removed
    # scenario (test_22), sub-block 1 is the adopted-prefix refusal
    # (test_23). Both stay inside 10.64.128.0/18, so ordinary reserve()
    # calls (which fill the pool from the bottom) never reach them, exactly
    # as test_e2e_adopt.py's own module docstring explains for its own
    # sixteen slots.

    def _removal_subblocks(self) -> list:
        return list(network(_adopt_slot22(SLOT_REMOVAL)).subnets(new_prefix=24))

    def _write_container_file(self, path: str, content: str) -> None:
        """Write one evidence-collection file into the `api` container.

        `onboard remove` reads a raw networks.csv/accounts.json/run.json
        exactly as `onboard assess` does -- not a pre-parsed table.json, so
        `stack.onboard`'s own stdin plumbing (built for `parse -`) does not
        reach this; this mirrors the file-writing `Stack.adopt`'s own
        `files=` parameter already does for a reviewed table.
        """
        result = run(compose_argv("exec", "-T", "api", "sh", "-c", f"cat > {shlex.quote(path)}"),
                     stdin=content)
        if result.returncode != 0:
            raise AssertionError(f"writing {path} into the api container failed: {result.stderr[:400]}")

    def _removal_accounts_json(self, account_ids: list) -> str:
        return json.dumps({"Accounts": [
            {"Id": account_id, "Name": f"account-{account_id}", "Status": "ACTIVE"}
            for account_id in account_ids
        ]})

    def _removal_run_json(self, *, started_at: str, finished_at: str, attempts: list) -> str:
        """`attempts`: (account_id, region, outcome, row_count) tuples, the
        shape internal/onboardcmd's decodeRunJSON reads (rawRunAccount/
        rawRunRegion)."""
        by_account: dict = {}
        for account_id, region, outcome, row_count in attempts:
            by_account.setdefault(account_id, []).append({
                "region": region, "outcome": outcome,
                "stage": "describe" if outcome != "succeeded" else "",
                "row_count": row_count, "reason": "", "observed_at": finished_at,
            })
        accounts = [
            {"account_id": account_id, "account_name": f"account-{account_id}",
             "credential_source": "assumed-role", "regions_attempted": "known",
             "not_attempted_reason": None, "region_source": "configured", "regions": regions}
            for account_id, regions in by_account.items()
        ]
        return json.dumps({
            "script_version": "2", "started_at": started_at, "finished_at": finished_at,
            "role_name": "PlatformIpamReadOnly", "management_account_used": False,
            "management_account_id": None, "configured_regions": ["eu-central-1"],
            "accounts": accounts,
        })

    def _remove(self, cidr: str, *, networks_rows: list, account_ids: list, attempts: list,
               started_at: str = "2026-09-22T09:00:00Z", finished_at: str = "2026-09-22T09:30:00Z",
               apply: bool = False, netbox_id: str = None, expect_exit: int = 0):
        """Run `onboard remove` against a synthetic evidence collection this
        call writes into the container, and return (report, stderr)."""
        key = run_key("remove-" + cidr.replace("/", "-").replace(".", "-")
                      + ("-apply-" if apply else "-dry-") + str(time.time_ns()))
        networks_path, accounts_path, run_path = (f"/tmp/{key}-networks.csv",
                                                   f"/tmp/{key}-accounts.json", f"/tmp/{key}-run.json")
        self._write_container_file(networks_path, _collector_networks_csv(networks_rows))
        self._write_container_file(accounts_path, self._removal_accounts_json(account_ids))
        self._write_container_file(
            run_path, self._removal_run_json(started_at=started_at, finished_at=finished_at, attempts=attempts))
        args = ["remove", cidr, "--domain", DOMAIN_ID,
               "--networks", networks_path, "--accounts", accounts_path, "--run", run_path]
        if apply:
            args += ["--apply", "--id", netbox_id]
        stdout, stderr = self.stack.onboard(*args, expect_exit=expect_exit)
        return json.loads(stdout), stderr

    def test_22_remove_refuses_until_every_contributor_is_absent_with_complete_coverage_then_applies(self):
        """The gap M9 itself: import two VPCs at one CIDR, show `onboard
        remove` refuses while a contributor is still observed, refuses again
        while coverage is incomplete, reports removable once both are
        absent with complete coverage, and only then -- with --apply and the
        NetBox id the dry run named -- deletes exactly that prefix and frees
        the space it held.
        """
        cidr = str(self._removal_subblocks()[0])
        account_a, account_b = "000000000041", "000000000042"
        resource_a, resource_b = "vpc-0e2e22a", "vpc-0e2e22b"
        region = "eu-central-1"
        observed_at = "2026-09-22T08:00:00Z"

        # pool_dev_euc1 reports capacity at /20 and /22 only
        # (deploy/compose/fixtures/pools.yaml: allowed_prefix_lengths), so a
        # /24 occupancy is read back through the /22 bucket it lies inside --
        # occupiedBlocks (internal/service/capacity.go) marks a /22-sized
        # block occupied on any overlap, and this /24 never spans two
        # /22-aligned blocks, so exactly one /22 block moves.
        cap_before = self.stack.capacity_when_complete(POOL_ID)["by_prefix_length"]["22"]["allocatable"]

        rows = [
            {"cidr": cidr, "account_id": account_a, "region": region, "name": "t22-vpc-a",
             "type": "vpc", "resource_id": resource_a, "association_id": "vpc-cidr-assoc-0e2e22a",
             "observed_at": observed_at},
            {"cidr": cidr, "account_id": account_b, "region": region, "name": "t22-vpc-b",
             "type": "vpc", "resource_id": resource_b, "association_id": "vpc-cidr-assoc-0e2e22b",
             "observed_at": observed_at},
        ]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t22")
        self.assertEqual(apply_lines[0]["action"], "created")
        prefix_id = str(self.stack.netbox_prefixes()[cidr]["id"])

        cap_after_import = self.stack.capacity_when_complete(POOL_ID)["by_prefix_length"]["22"]["allocatable"]
        self.assertEqual(cap_after_import, cap_before - 1,
                         "importing the /24 must remove exactly one containing /22 block from capacity")

        # -- refused: vpc-b is still observed (only vpc-a is gone from the
        # collection) --
        report, _ = self._remove(
            cidr,
            networks_rows=[{"cidr": cidr, "account_id": account_b, "region": region,
                            "name": "t22-vpc-b", "type": "vpc", "resource_id": resource_b,
                            "association_id": "vpc-cidr-assoc-0e2e22b", "observed_at": observed_at}],
            account_ids=[account_a, account_b],
            attempts=[(account_a, region, "succeeded", 0), (account_b, region, "succeeded", 1)],
            expect_exit=3,
        )
        self.assertFalse(report["removable"], report)
        codes = {r["code"] for r in report["refusals"]}
        self.assertIn("not_observed_absent", codes, report["refusals"])
        absent_by_resource = {c["resource_id"]: c["observed_absent"] for c in report["contributors"]}
        self.assertTrue(absent_by_resource[resource_a], report["contributors"])
        self.assertFalse(absent_by_resource[resource_b], report["contributors"])
        self.assertIn(cidr, self.stack.netbox_prefixes(), "a refused dry run must not touch NetBox")

        # -- refused: both are gone from the collection, but one region's
        # coverage is only "partial" --
        report, _ = self._remove(
            cidr, networks_rows=[], account_ids=[account_a, account_b],
            attempts=[(account_a, region, "succeeded", 0), (account_b, region, "partial", 0)],
            finished_at="2026-09-22T09:00:00Z", expect_exit=3,
        )
        self.assertFalse(report["removable"], report)
        codes = {r["code"] for r in report["refusals"]}
        self.assertIn("coverage_incomplete", codes, report["refusals"])
        self.assertIn(cidr, self.stack.netbox_prefixes(), "a refused dry run must not touch NetBox")

        # -- removable: both gone, complete coverage for both accounts and
        # regions, and the dry run alone still writes nothing --
        dry_report, dry_stderr = self._remove(
            cidr, networks_rows=[], account_ids=[account_a, account_b],
            attempts=[(account_a, region, "succeeded", 0), (account_b, region, "succeeded", 0)],
            finished_at="2026-09-22T09:30:00Z", expect_exit=0,
        )
        self.assertTrue(dry_report["removable"], dry_report)
        self.assertFalse(dry_report["removed"], dry_report)
        self.assertEqual(dry_report["netbox_id"], prefix_id, dry_report)
        self.assertIn(f"removable; re-run with --apply --id {prefix_id}", dry_stderr, dry_stderr)
        self.assertIn(cidr, self.stack.netbox_prefixes(), "the dry run must not have deleted anything")
        prefix_count_before_apply = self._netbox_prefix_count()

        # -- --apply with the NetBox id the dry run reported removes exactly
        # that prefix --
        apply_report, apply_stderr = self._remove(
            cidr, networks_rows=[], account_ids=[account_a, account_b],
            attempts=[(account_a, region, "succeeded", 0), (account_b, region, "succeeded", 0)],
            finished_at="2026-09-22T09:30:00Z", apply=True, netbox_id=prefix_id, expect_exit=0,
        )
        self.assertTrue(apply_report["removed"], apply_report)
        self.assertEqual(apply_report["netbox_id"], prefix_id, apply_report)
        self.assertIn(f"removed prefix {cidr}", apply_stderr, apply_stderr)
        self.assertNotIn(cidr, self.stack.netbox_prefixes(), "the prefix must be gone from NetBox")
        self.assertEqual(self._netbox_prefix_count(), prefix_count_before_apply - 1,
                         "exactly one prefix must have been deleted, no other one changed")

        # The freed /24 is allocatable capacity again -- ADR 0016's "a
        # reservation may now select the space", demonstrated without
        # spending any of the suite's reservation quota: the OpenAPI
        # allocation request has no candidate-CIDR field to pin a real
        # reservation to this exact block (api/openapi.yaml's allocation
        # request schema), so capacity -- the same aggregate measure test_01
        # uses for the converse case, "occupying capacity" -- is the
        # honest, quota-free way to show the block returned to the pool.
        cap_after_remove = self.stack.capacity_when_complete(POOL_ID)["by_prefix_length"]["22"]["allocatable"]
        self.assertEqual(cap_after_remove, cap_before,
                         "removing the prefix must return the containing /22 block to allocatable capacity")

    def test_23_remove_refuses_an_adopted_prefix_in_the_range(self):
        """ADR 0016's outright refusal list: any ownership field refuses a
        removal, whatever the evidence says. Ownership is simulated the same
        way test_21 does -- writing the platform's own allocation markers
        directly through the NetBox REST API -- rather than running the full
        `adopt` pipeline (covered end to end by test_e2e_adopt.py)."""
        cidr = str(self._removal_subblocks()[1])
        account_id, region = "000000000043", "eu-central-1"
        resource_id, observed_at = "vpc-0e2e23a", "2026-09-22T08:00:00Z"

        rows = [{"cidr": cidr, "account_id": account_id, "region": region, "name": "t23-vpc",
                "type": "vpc", "resource_id": resource_id, "association_id": "vpc-cidr-assoc-0e2e23a",
                "observed_at": observed_at}]
        _, apply_lines, _, _ = self._import(_collector_networks_csv(rows), "t23")
        self.assertEqual(apply_lines[0]["action"], "created")
        prefix_id = str(self.stack.netbox_prefixes()[cidr]["id"])

        owned = self.stack.netbox_as(
            self.stack.netbox_token, "PATCH", f"/api/ipam/prefixes/{prefix_id}/",
            {"custom_fields": {
                "platform_allocation_id": "alloc-t23-simulated",
                "platform_allocation_key": "t23-simulated",
                "platform_operation_id": "op-t23-simulated",
                "platform_state": "RESERVED",
            }})
        self.assertEqual(owned.status, 200, owned.body)
        try:
            before_row = self.stack.netbox_prefixes()[cidr]

            report, _ = self._remove(
                cidr, networks_rows=[], account_ids=[account_id],
                attempts=[(account_id, region, "succeeded", 0)],
                finished_at="2026-09-22T09:30:00Z", expect_exit=3,
            )
            self.assertFalse(report["removable"], report)
            codes = {r["code"] for r in report["refusals"]}
            self.assertIn("prefix_owned", codes, report["refusals"])

            # --apply against an unremovable report must still refuse,
            # never reaching NetBox's DELETE at all.
            apply_report, _ = self._remove(
                cidr, networks_rows=[], account_ids=[account_id],
                attempts=[(account_id, region, "succeeded", 0)],
                finished_at="2026-09-22T09:30:00Z", apply=True, netbox_id=prefix_id, expect_exit=3,
            )
            self.assertFalse(apply_report["removed"], apply_report)
            self.assertIn(cidr, self.stack.netbox_prefixes(), "an owned prefix must never be deleted")

            after_row = self.stack.netbox_prefixes()[cidr]
            self.assertEqual(after_row.get("last_updated"), before_row.get("last_updated"),
                             "a refused removal must write nothing")
        finally:
            cleared = self.stack.netbox_as(
                self.stack.netbox_token, "PATCH", f"/api/ipam/prefixes/{prefix_id}/",
                {"custom_fields": {
                    "platform_allocation_id": None, "platform_allocation_key": None,
                    "platform_operation_id": None, "platform_state": None,
                }})
            self.assertEqual(cleared.status, 200, cleared.body)


if __name__ == "__main__":
    unittest.main()
