"""End-to-end coverage for package G3b5, the last package of Track G (ADR
0011): the gate the whole track exists to fix.

`platform-ipam client findings --fail-if-open` (package E1) is a CI gate: it
is meant to fail a pipeline when the estate holds an open finding, and pass
when it does not. Before ADR 0011, whether it could do that at all depended
on who ran it -- an identity eligible for no pool always saw an empty
findings list and always passed, whatever the true state of the estate
(ADR 0011's Context: "A gate whose verdict depends on who was given the
credential is not a gate.") Package G2 taught the gate to refuse a verdict
(`ExitNotEligible`, exit `8`) for such an identity instead of passing
silently; packages G3a-G3b4 then built the operator role that gives a
pipeline an identity that CAN see the whole estate, so the gate can produce a
real verdict rather than either an accidental pass or a refusal.

This module is the demonstration: with one genuine OPEN `unmanaged_occupancy`
finding present, the gate exits `7` (open findings found) under both the
developer tenant that owns the estate and the operator that reads across all
of it, and `8` (refused) under the ops-observer identity, which has a tenant
but that tenant is eligible for no pool -- the exact contrast ADR 0011's
Decision names as the reason the role exists. Removing the resource and
letting the worker resolve the finding then shows the same three identities
converge the other way: developer and operator agree the estate is clean,
and ops-observer still cannot render a verdict at all.

Reusing test_e2e_adopt.py's fake-cloud-fixture machinery: an
`unmanaged_occupancy` finding needs only an *observed* AWS resource with no
platform-ipam claim tag and no adoption record (internal/service/worker.go's
reconcileAllocations, the loop below the `st.Findings[id] ... = RESOLVED`
reset) -- never a NetBox import and never a reservation. This module
therefore never imports anything and reserves no allocation of its own
(docs/WORK_PLAN.md's G3b5 quota note); it only ever writes one untagged
resource into deploy/compose/fixtures/cloud.json, restored byte-identically
in cleanup exactly as test_e2e_adopt.py restores it, whether or not a test
here fails first.

Pagination (test_03) rides on whatever findings and allocations already
exist on the ledger from every module that ran before this one alphabetically
-- it asserts a relation (every row an operator's `limit=1` walk visits
appears exactly once, and the set matches the unpaged read taken back to
back), never a row count, so it needs no allocation or finding of its own
either. internal/transport/http_test.go's TestOperatorGrantedReads and
TestOperatorFindingsAreTheDomainView already prove the same property at the
unit boundary, over a fabricated multi-row state; this is its end-to-end
counterpart over whatever the real ledger holds right now.

Not covered here, and not claimed: an operator seeing a finding a tenant
literally cannot -- this stack has exactly one eligible tenant per pool, so
an operator's de-duplicated view and the developer's own view are always the
same set (test_e2e_operator_role.py's module docstring already states this
limit, for the same reason). What this module shows instead, and what the
gate actually needed, is that the SAME verdict-bearing flag produces a
verdict for an identity that can see the estate and a refusal for one that
cannot, with a real open finding on the table to make the contrast concrete
rather than vacuous.
"""

from __future__ import annotations

import hashlib
import json
import time
import unittest

from harness import ROOT, run_key, stack

CLOUD_FIXTURE = ROOT / "deploy/compose/fixtures/cloud.json"
DOMAIN_ID = "local-development"
ACCOUNT_ID = "000000000000"
REGION = "eu-central-1"

# internal/cli/cli.go: ExitFindings = 7, ExitNotEligible = 8. Mirrored here
# the same way every other e2e module does, for the same reason: the Python
# suite has no import path into the Go package.
EXIT_FINDINGS = 7
EXIT_NOT_ELIGIBLE = 8


class OperatorGateE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        cls.original_cloud = CLOUD_FIXTURE.read_bytes()
        cls.original_cloud_hash = hashlib.sha256(cls.original_cloud).hexdigest()
        cls.addClassCleanup(cls._restore_cloud_fixture)
        run_id = run_key("operator-gate")
        cls.resource_id = "vpc-" + hashlib.sha1(run_id.encode()).hexdigest()[:17]
        # An address nothing else in this suite imports or reserves from --
        # unmanaged_occupancy is raised for any observed, unclaimed resource
        # regardless of whether its CIDR overlaps a pool or a NetBox prefix
        # (internal/service/worker.go's reconcileAllocations does not
        # consult inventory or capacity for this code at all), so this only
        # needs to be a syntactically valid, otherwise-unused CIDR.
        cls.resource_cidr = "10.90.0.0/22"

    @classmethod
    def _restore_cloud_fixture(cls) -> None:
        CLOUD_FIXTURE.write_bytes(cls.original_cloud)
        got = hashlib.sha256(CLOUD_FIXTURE.read_bytes()).hexdigest()
        if got != cls.original_cloud_hash:
            raise AssertionError(
                "deploy/compose/fixtures/cloud.json was not restored byte-identically: "
                f"expected sha256 {cls.original_cloud_hash}, got {got}"
            )

    # -- fake-cloud fixture: one resource, added then removed -----------

    def _flush_cloud(self, resources: list) -> None:
        payload = {
            "domain_id": DOMAIN_ID,
            "generation": "development-1",
            "complete": True,
            "resources": resources,
        }
        tmp = CLOUD_FIXTURE.with_suffix(".json.e2e-gate-tmp")
        tmp.write_text(json.dumps(payload), encoding="utf-8")
        tmp.replace(CLOUD_FIXTURE)

    def _put_gate_resource(self) -> None:
        self._flush_cloud([{
            "account_id": ACCOUNT_ID, "region": REGION, "type": "vpc",
            "id": self.resource_id, "cidr": self.resource_cidr,
            "cidrs": [self.resource_cidr], "tags": {},
        }])

    def _remove_gate_resource(self) -> None:
        self._flush_cloud([])

    # -- waiting: never a blind sleep, same budget as test_e2e_adopt.py -

    def _open_unmanaged_count(self) -> int:
        findings = (self.stack.api("GET", "/v1/findings").body or {}).get("items", [])
        return len([
            f for f in findings
            if f.get("code") == "unmanaged_occupancy" and f.get("status") == "OPEN"
            and f.get("account_id") == ACCOUNT_ID and f.get("region") == REGION
        ])

    def _wait_until(self, predicate, *, timeout: float = 90.0, interval: float = 3.0, desc: str = ""):
        # full_scan_interval_seconds is 30 in deploy/compose/fixtures/pools.yaml,
        # so three cycles is a generous margin without ever sleeping blindly.
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            last = predicate()
            if last:
                return last
            time.sleep(interval)
        raise AssertionError(f"condition not reached within {timeout}s ({desc}); last value: {last!r}")

    # =====================================================================
    # 1. THE GATE: one OPEN finding, three identities, two verdicts
    # =====================================================================

    def test_01_the_gate_judges_developer_and_operator_alike_and_refuses_ops_observer(self):
        cls = type(self)
        baseline = self._open_unmanaged_count()
        self._put_gate_resource()
        self._wait_until(
            lambda: self._open_unmanaged_count() >= baseline + 1,
            desc="worker reports unmanaged_occupancy for the newly observed resource",
        )

        # The developer tenant is pool_dev_euc1's only eligible tenant
        # (deploy/compose/fixtures/pools.yaml) and therefore the only stored
        # copy of this finding: the gate must see it and fail.
        self.stack.cli("findings", "--fail-if-open", expect_exit=EXIT_FINDINGS)

        # The operator (packages G3b2-G3b4) reads GET /v1/findings across the
        # whole estate, de-duplicated by internal/service/service.go's
        # operatorFindings/collapseOccupancy -- the same OPEN finding, judged
        # the same way, exit for exit. This is the property Track G exists
        # for: the verdict no longer depends on who was given the credential,
        # it depends on the estate.
        self.stack.cli("findings", "--fail-if-open", token=self.stack.operator_token,
                       expect_exit=EXIT_FINDINGS)

        # The ops-observer identity (package G3c) has a tenant, and that
        # tenant is eligible for no pool (fixtures/pools.yaml lists none),
        # so every read it makes is scoped to nothing -- ADR 0011 stage one
        # refuses it at GET /v1/pools with 403 no_eligible_pool, and the CLI
        # (package G2) treats exactly that refusal as "cannot render a
        # verdict", exit 8, never a pass and never ExitFindings, however
        # loud the estate actually is.
        self.stack.cli("findings", "--fail-if-open", token=self.stack.ops_token,
                       expect_exit=EXIT_NOT_ELIGIBLE)

        # The operator's own findings list -- not just the gate's exit code
        # -- names this exact resource exactly once. domain_id/resource_type/
        # resource_id are operator-only fields (package G3b4); a tenant
        # cannot make this same assertion because its own response never
        # carries resource_id at all (see test_e2e_operator_role.py).
        operator_findings = self.stack.cli("findings", token=self.stack.operator_token)
        matches = [
            item for item in operator_findings["items"]
            if item.get("resource_type") == "vpc" and item.get("resource_id") == cls.resource_id
        ]
        self.assertEqual(len(matches), 1,
                         f"operator findings should name resource {cls.resource_id} exactly once: "
                         f"{[i for i in operator_findings['items'] if i.get('code') == 'unmanaged_occupancy']}")
        self.assertEqual(matches[0]["status"], "OPEN", matches[0])
        self.assertEqual(matches[0]["domain_id"], DOMAIN_ID, matches[0])
        self.assertNotIn("tenant_id", matches[0],
                         "a domain-level (occupancy) finding must name no recipient tenant")

    # =====================================================================
    # 2. Remove the resource; the gate converges back for developer and
    #    operator alike, and ops-observer stays refused regardless.
    # =====================================================================

    def test_02_removing_the_resource_resolves_the_finding_and_the_gate_converges(self):
        cls = type(self)
        baseline_before_removal = self._open_unmanaged_count()
        self.assertGreaterEqual(baseline_before_removal, 1,
                                "test_01 must have left an OPEN finding for this test to resolve")
        self._remove_gate_resource()

        def resolved():
            return self._open_unmanaged_count() < baseline_before_removal or None

        self._wait_until(resolved, desc="the worker resolves the removed resource's finding")

        # Confirm the specific finding this test raised is the one that
        # resolved, using the operator's resource-identity view.
        operator_findings = self.stack.cli("findings", token=self.stack.operator_token)
        matches = [
            item for item in operator_findings["items"]
            if item.get("resource_type") == "vpc" and item.get("resource_id") == cls.resource_id
        ]
        self.assertEqual(len(matches), 1, matches)
        self.assertEqual(matches[0]["status"], "RESOLVED", matches[0])

        # Derive the verdict each identity should reach from the estate as it
        # actually stands now, exactly as test_e2e_cli_findings.py and
        # test_e2e_operator_role.py already do -- not a hard-coded 0, since
        # this suite's other modules own their own state and a constant here
        # would be an assertion about them, not about this property. What
        # this proves is the contrast: developer and operator must reach the
        # SAME verdict (their sets are equal on this one-eligible-tenant
        # fixture), and ops-observer must never render one at all.
        developer_findings = self.stack.cli("findings")
        developer_open = {item["id"] for item in developer_findings["items"] if item.get("status") == "OPEN"}
        want_exit = EXIT_FINDINGS if developer_open else 0

        self.stack.cli("findings", "--fail-if-open", expect_exit=want_exit)
        self.stack.cli("findings", "--fail-if-open", token=self.stack.operator_token,
                       expect_exit=want_exit)
        self.stack.cli("findings", "--fail-if-open", token=self.stack.ops_token,
                       expect_exit=EXIT_NOT_ELIGIBLE)

    # =====================================================================
    # 3. Operator pagination: GET /v1/findings?limit=1 and
    #    GET /v1/allocations?limit=1 visit every row exactly once.
    # =====================================================================

    def _walk_pages(self, path: str) -> list:
        seen = []
        cursor = None
        for _ in range(10000):  # a finite safety cap, never expected to bind
            query = f"{path}?limit=1"
            if cursor:
                query += f"&cursor={cursor}"
            response = self.stack.api("GET", query, token=self.stack.operator_token)
            self.assertEqual(response.status, 200, response.body)
            items = (response.body or {}).get("items", [])
            self.assertLessEqual(len(items), 1, f"limit=1 returned {len(items)} items: {response.body}")
            seen.extend(item["id"] for item in items)
            cursor = (response.body or {}).get("next_cursor")
            if not cursor:
                return seen
        raise AssertionError(f"{path}?limit=1 pagination did not terminate within 10000 pages")

    def test_03_operator_pagination_visits_every_row_exactly_once(self):
        # Findings and allocations are each read back to back so a worker
        # cycle landing between the paged walk and the unpaged read cannot
        # manufacture a mismatch that is not really there --
        # test_e2e_operator_role.py's test_operator_sees_the_whole_estate_once
        # makes the identical assumption about its own back-to-back reads.
        findings_paged = self._walk_pages("/v1/findings")
        findings_unpaged = {
            item["id"] for item in
            (self.stack.api("GET", "/v1/findings", token=self.stack.operator_token).body or {}).get("items", [])
        }
        self.assertEqual(len(findings_paged), len(set(findings_paged)),
                         f"a paged findings walk repeated a row: {findings_paged}")
        self.assertEqual(set(findings_paged), findings_unpaged,
                         "the operator's paged findings walk did not visit exactly the unpaged set")

        allocations_paged = self._walk_pages("/v1/allocations")
        allocations_unpaged = {
            item["id"] for item in
            (self.stack.api("GET", "/v1/allocations", token=self.stack.operator_token).body or {}).get("items", [])
        }
        self.assertEqual(len(allocations_paged), len(set(allocations_paged)),
                         f"a paged allocations walk repeated a row: {allocations_paged}")
        self.assertEqual(set(allocations_paged), allocations_unpaged,
                         "the operator's paged allocations walk did not visit exactly the unpaged set")


if __name__ == "__main__":
    unittest.main()
