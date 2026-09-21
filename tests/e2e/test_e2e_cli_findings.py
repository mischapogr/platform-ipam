"""End-to-end coverage for `platform-ipam client findings` (packages E1, G2).

The verb is a transport client only (ADR 0003, ADR 0008): it selects no
severity, drops no field, and adds no filter the API does not already apply.
This suite asserts that CLI and REST agree about the same list of findings,
the way test_e2e_allocation.py's CLI tests already assert for reserve and
capacity.

Unit coverage for query-parameter passthrough, the --fail-if-open exit code
across the full status vocabulary, and unknown-flag/401/redirect handling
lives in internal/cli/cli_test.go; it uses httptest servers and does not need
a running stack. The same file also carries unit coverage for package G2's
pool-eligibility guard (TestFindingsFailIfOpenChecksPoolEligibility and
neighbors): no pools + zero findings, no pools + an open finding returned
anyway, pools present in either open state, a failing/unreadable/looping
/v1/pools, and the without-the-flag case where /v1/pools is never requested.

This module adds only the positive-path end-to-end case: under an identity
that IS eligible for a pool, --fail-if-open must never report the new
"ineligible" exit code. The negative path -- an identity eligible for no
pool -- has NO end-to-end coverage: the development stack (deploy/compose)
authenticates every request as one identity (local auth mode,
IPAM_LOCAL_SUBJECT, tenant "developer" in deploy/compose/fixtures/
identities.yaml), and provisioning a second, pool-ineligible identity for
this stack is out of scope for package G2.
"""

from __future__ import annotations

import unittest

from harness import stack

# internal/cli/cli.go: ExitFindings = 7, ExitNotEligible = 8. Mirrored here
# rather than imported because the Python suite has no import path into the
# Go package; both values are part of the CLI's documented exit-code
# contract (helpText, docs/CLIENTS.md section 6).
EXIT_FINDINGS = 7
EXIT_NOT_ELIGIBLE = 8


class CLIFindingsE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()

    def test_findings_returns_an_items_list(self):
        through_cli = self.stack.cli("findings")
        self.assertIsInstance(through_cli, dict,
                              "findings must answer a JSON object, not a bare list")
        self.assertIn("items", through_cli,
                      "the API's list envelope (internal/transport/http.go's page()) "
                      "always carries an items key")
        self.assertIsInstance(through_cli["items"], list)

    def test_cli_and_rest_agree_about_the_same_findings(self):
        # Read through both surfaces back to back. Nothing in this test
        # writes a finding, so barring a worker cycle landing between the two
        # calls -- the same assumption test_cli_capacity_matches_rest_capacity
        # already makes about capacity -- both should see the same set.
        through_cli = self.stack.cli("findings")
        through_rest = self.stack.api("GET", "/v1/findings").body

        self.assertIn("items", through_rest)
        cli_items = through_cli["items"]
        rest_items = through_rest["items"]

        # Compare only the fields that cannot change between the two calls:
        # id and code identify a finding; status, severity or the observed
        # timestamps could in principle move if the worker ran in between.
        cli_keys = {(item["id"], item["code"]) for item in cli_items}
        rest_keys = {(item["id"], item["code"]) for item in rest_items}
        self.assertEqual(cli_keys, rest_keys,
                         "the CLI must show exactly the findings REST shows, "
                         "unfiltered and unranked")

        # Every field the REST response carries for a given id must appear
        # unchanged in the CLI's copy of the same item -- the CLI re-encodes
        # the server payload (writeJSON) rather than reshaping it.
        rest_by_id = {item["id"]: item for item in rest_items}
        for item in cli_items:
            rest_item = rest_by_id.get(item["id"])
            if rest_item is None:
                continue  # a worker cycle resolved/removed it between calls
            for field in ("code", "severity", "allocation_id", "account_id", "region"):
                self.assertEqual(item.get(field), rest_item.get(field),
                                 f"field {field!r} differs between CLI and REST for {item['id']}")

    def test_fail_if_open_matches_the_items_it_just_printed(self):
        # Derive the expectation from the same response the flag will act on,
        # rather than assuming anything about the fixture data: whatever
        # findings this stack happens to hold, --fail-if-open must exit
        # EXIT_FINDINGS exactly when at least one of them is OPEN, and it must
        # never change how many items got printed.
        through_cli = self.stack.cli("findings")
        has_open = any(item.get("status") == "OPEN" for item in through_cli["items"])

        gated = self.stack.cli(
            "findings", "--fail-if-open",
            expect_exit=EXIT_FINDINGS if has_open else 0,
        )
        if gated is not None:
            self.assertEqual(len(gated["items"]), len(through_cli["items"]),
                             "--fail-if-open must never hide a returned finding")

    def test_fail_if_open_never_reports_ineligible_for_the_development_identity(self):
        # Package G2: --fail-if-open first asks GET /v1/pools and exits
        # EXIT_NOT_ELIGIBLE instead of a clean verdict when the caller's
        # tenant is eligible for no pool (docs/WORK_PLAN.md Track G,
        # following ADR 0008's finding that every API read is scoped by
        # tenant). The development stack's identity belongs to tenant
        # "developer" (deploy/compose/fixtures/identities.yaml), which
        # deploy/compose/fixtures/pools.yaml makes eligible for
        # pool_dev_euc1 -- so GET /v1/pools must return at least one item
        # for it, and this is the positive path only: proof that an eligible
        # identity is never mistaken for an ineligible one. See the module
        # docstring for why the negative path has no coverage here.
        through_cli = self.stack.cli("findings")
        has_open = any(item.get("status") == "OPEN" for item in through_cli["items"])
        want_exit = EXIT_FINDINGS if has_open else 0

        # expect_exit checks the code exactly: if this identity were ever
        # (mistakenly) treated as eligible for no pool, this call would
        # raise on EXIT_NOT_ELIGIBLE instead of matching want_exit.
        self.stack.cli("findings", "--fail-if-open", expect_exit=want_exit)


if __name__ == "__main__":
    unittest.main()
