"""End-to-end coverage for the second local development identity (package
G3c, IPAM_LOCAL_EXTRA_CREDENTIALS).

Single-identity local auth mode (IPAM_LOCAL_TOKEN/IPAM_LOCAL_SUBJECT) meant
neither G2's nor G3a's negative path -- an identity that is not the
allocation's tenant, or that is eligible for no pool -- had any end-to-end
coverage. This module adds the ops-observer identity: tenant "ops"
(deploy/compose/fixtures/identities.yaml), which
deploy/compose/fixtures/pools.yaml lists in no pool's eligible_tenants, on
purpose, so its every read below should come back empty or refused, never
leaking the developer's allocation.

GET /v1/pools is the one read that is refused rather than empty (ADR 0011
stage one, package G3a): it is the entitlement endpoint, and "eligible for no
pool" is not the same fact as "there is nothing". The allocation and finding
lists stay an honest empty 200 for the same identity, and the developer's
view of /v1/pools is unchanged. The start-up warning G3a logs for such an
identity is asserted here too, from the API container's log.
"""

from __future__ import annotations

import unittest

from harness import compose_argv, run, run_key, stack

POOL_ID = "pool_dev_euc1"

# internal/cli/cli.go: ExitFindings = 7, ExitNotEligible = 8. Mirrored here
# the same way test_e2e_cli_findings.py already does, for the same reason:
# the Python suite has no import path into the Go package.
EXIT_FINDINGS = 7
EXIT_NOT_ELIGIBLE = 8


class SecondIdentityE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        # One allocation for the whole class, not one per test. A released
        # RESERVED allocation is quarantined, and a quarantined allocation
        # still counts against the pool's per-tenant quota, so a reservation
        # per test would spend a quota slot per test for good -- and no test
        # here changes the allocation: every request against it comes from the
        # ops identity and is expected to be refused.
        cls.allocation = cls.stack.reserve(run_key("second-identity"), 22)
        cls.addClassCleanup(cls._release, cls.allocation["id"])

    @classmethod
    def _release(cls, allocation_id: str) -> None:
        response = cls.stack.api("DELETE", f"/v1/allocations/{allocation_id}")
        # A RESERVED (never-bound) allocation's release is accepted (202) and
        # immediately quarantined -- there is no pending durable operation to
        # poll, unlike POST reserve/PUT binding's 202s (see await_operation),
        # so 202 here is a terminal, synchronous result, not "still running".
        if response.status not in (200, 202, 204):
            raise AssertionError(
                f"cleanup release of {allocation_id} returned "
                f"{response.status}: {response.body}"
            )

    # -- reads: empty, never the developer's data ----------------------------

    def test_ops_identity_lists_no_allocations(self):
        response = self.stack.api("GET", "/v1/allocations", token=self.stack.ops_token)
        self.assertEqual(response.status, 200)
        self.assertEqual((response.body or {}).get("items"), [])

    def test_ops_identity_sees_no_findings(self):
        response = self.stack.api("GET", "/v1/findings", token=self.stack.ops_token)
        self.assertEqual(response.status, 200)
        self.assertEqual((response.body or {}).get("items"), [])

    def test_ops_identity_is_refused_at_pools_and_the_developer_is_not(self):
        response = self.stack.api("GET", "/v1/pools", token=self.stack.ops_token)
        self.assertEqual(response.status, 403, response.body)
        error = (response.body or {}).get("error", {})
        self.assertEqual(error.get("code"), "no_eligible_pool", response.body)
        self.assertIs(error.get("retryable"), False, response.body)
        # The refusal names no pool: not an id, not a count.
        self.assertNotIn(POOL_ID, str(response.body))
        # Paging parameters do not turn the refusal into an empty page.
        paged = self.stack.api("GET", "/v1/pools?limit=1&cursor=zzzz", token=self.stack.ops_token)
        self.assertEqual(paged.status, 403, paged.body)

        developer = self.stack.api("GET", "/v1/pools")
        self.assertEqual(developer.status, 200, developer.body)
        self.assertIn(POOL_ID, [item.get("id") for item in (developer.body or {}).get("items", [])])

    def test_api_warned_at_start_about_the_identity_without_a_pool(self):
        result = run(compose_argv("logs", "--no-log-prefix", "api"))
        self.assertEqual(result.returncode, 0, result.stderr)
        logs = result.stdout + result.stderr
        warned = [line for line in logs.splitlines()
                  if "eligible for no pool" in line and "ops-observer" in line]
        self.assertTrue(warned, "the API did not warn about ops-observer at start-up")
        self.assertFalse(any(self.stack.ops_token in line for line in logs.splitlines()),
                         "a local credential appears in the API log")

    # -- the developer's allocation is 404, not 403, for the ops identity ---

    def test_ops_identity_gets_404_not_403_for_the_developers_allocation(self):
        allocation_id = self.allocation["id"]
        # Every body/header here is otherwise valid, so a 404 can only be the
        # tenant-scoping check firing before any of the request's content is
        # ever considered -- not a validation error dressed up as a 404.
        cases = [
            ("GET", f"/v1/allocations/{allocation_id}", None, {}),
            ("PATCH", f"/v1/allocations/{allocation_id}",
             {"description": "ops identity probe"},
             {"Idempotency-Key": f"e2e-ops-patch-{allocation_id}", "If-Match": '"1"'}),
            ("PUT", f"/v1/allocations/{allocation_id}/binding",
             {"provider": "aws", "resource_type": "vpc", "resource_id": "vpc-0ops00000000000",
              "account_id": "000000000000", "region": "eu-central-1"},
             {"Idempotency-Key": f"e2e-ops-bind-{allocation_id}"}),
            ("DELETE", f"/v1/allocations/{allocation_id}", None, {}),
        ]
        for method, path, body, headers in cases:
            with self.subTest(method=method):
                response = self.stack.api(method, path, body, headers,
                                          token=self.stack.ops_token)
                self.assertEqual(response.status, 404, f"{method} {path}: {response.body}")
                self.assertEqual((response.body or {}).get("error", {}).get("code"), "not_found",
                                 f"{method} {path}: {response.body}")

        # None of the refused attempts above may have touched the developer's
        # own view of their allocation.
        still_there = self.stack.api("GET", f"/v1/allocations/{allocation_id}")
        self.assertEqual(still_there.status, 200)
        self.assertEqual(still_there.body["state"], "RESERVED")

    # -- reservation is refused as a policy violation, not silently scoped --

    def test_ops_identity_cannot_reserve(self):
        key = run_key("ops-forbidden")
        response = self.stack.api("POST", "/v1/allocations", {
            "allocation_key": key, "scope": "vpc",
            "environment": self.stack.env.get("IPAM_ENVIRONMENT", "development"),
            "region": "eu-central-1", "account_id": "000000000000",
            "prefix_length": 22, "description": "", "labels": {},
        }, {"Idempotency-Key": f"e2e-{key}"}, token=self.stack.ops_token)
        self.assertEqual(response.status, 422)
        self.assertEqual((response.body or {}).get("error", {}).get("code"), "policy_violation")

    # -- an invalid token is 401 ----------------------------------------------

    def test_invalid_token_is_401(self):
        response = self.stack.api("GET", "/v1/allocations",
                                  token="not-a-configured-token-at-all-000000")
        self.assertEqual(response.status, 401)

    # -- CLI: findings --fail-if-open's exit code differs by identity -------

    def test_findings_fail_if_open_exit_code_differs_by_identity(self):
        # The ops identity is eligible for no pool, so this must always end
        # ExitNotEligible -- never a clean pass, and never ExitFindings, no
        # matter what the (always-empty, for this tenant) findings list
        # looks like. See notEligibleError in internal/cli/cli.go.
        under_ops = self.stack.cli("findings", "--fail-if-open",
                                   token=self.stack.ops_token, expect_exit=EXIT_NOT_ELIGIBLE)
        if under_ops is not None:
            self.assertEqual(under_ops.get("items"), [])

        # The developer's tenant IS eligible for pool_dev_euc1
        # (deploy/compose/fixtures/pools.yaml), so the same flag must never
        # report ExitNotEligible for it -- only a clean pass or ExitFindings,
        # whichever the current findings list actually calls for.
        through_developer = self.stack.cli("findings")
        has_open = any(item.get("status") == "OPEN" for item in through_developer["items"])
        want_exit = EXIT_FINDINGS if has_open else 0
        self.stack.cli("findings", "--fail-if-open", expect_exit=want_exit)


if __name__ == "__main__":
    unittest.main()
