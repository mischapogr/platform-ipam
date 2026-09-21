"""End-to-end coverage for the operator role (packages G3b1 and G3b2, ADR 0011
stage two): "domain.Principal" carries a "role" field whose only legal
non-empty value is "operator" -- a principal with NO tenant. Package G3b1
proved the deny-by-default state end to end, against the real stack, the way
tests/e2e/test_e2e_second_identity.py proved package G3c's second identity.

Package G3b2 grants four reads in internal/service and changes exactly the
assertions that cover them: GET /v1/pools now answers 200 with every
configured pool, GET /v1/pools/{id}/capacity 200 for any pool, GET
/v1/allocations lists the developer's allocation, and GET
/v1/allocations/{id} reads it. Because /v1/pools answers 200, "client
findings --fail-if-open" under the operator no longer exits ExitNotEligible.

Package G3b3 grants the fifth read: GET /v1/findings answers the whole
estate, with the occupancy fan-out's per-tenant duplicates of one
domain-level fact collapsed into one row. On THIS stack the collapse is a
no-op and says so: deploy/compose/fixtures/pools.yaml lists exactly one
eligible tenant, "developer", for the one pool, so the fan-out has one
recipient and every stored row is a group of one. What the two tests below
can therefore assert exactly is the relation -- the operator sees precisely
the findings the developer sees, each once -- and that "client findings
--fail-if-open" under the operator is now judged on the ESTATE rather than
on an empty list. Whether that estate holds an open finding is derived from
the response the flag acts on, the way test_e2e_cli_findings.py already
derives it, because the modules that run before this one in the suite's
alphabetical order own their own state: the shipped cloud fixture observes
no resources at all (deploy/compose/fixtures/cloud.json) and run-e2e.sh
truncates the ledger, so a clean run has nothing open, but asserting that
as a constant would be asserting a fixture rather than the behaviour.

The write assertions -- reserve, patch, bind, release, and GET
/v1/operations/{id}, which stays refused in v1 by design and not only by
omission -- are untouched by G3b2 and stay refused for the lifetime of this
role, by the same checks named in the comments below. So do the 401 and the
start-up-log assertions. Packages G3b3 to G3b5 may change further READ
assertions; none of them may change a write.

Package G3b4 adds the operator-only response fields and read logging, and
inverts exactly the absence assertions the two comments above name: an
operator's allocation (list and get) now carries tenant_id ("developer", this
stack's one tenant); an operator's finding now carries domain_id
("local-development", this stack's one overlap domain) and, only where
allocation_id is set, tenant_id -- absent, not null, on a domain-level
(occupancy) row. None of these fields ever reaches the developer's own
response, operator configured or not, which is asserted alongside each
inversion. A new test proves an operator's read is logged at the API
container's application log with the operator's subject and no credential.
"""

from __future__ import annotations

import unittest

from harness import compose_argv, run, run_key, stack

POOL_ID = "pool_dev_euc1"

# internal/cli/cli.go: ExitFindings = 7, ExitNotEligible = 8. Mirrored here
# the same way test_e2e_second_identity.py does, for the same reason: the
# Python suite has no import path into the Go package.
EXIT_FINDINGS = 7
EXIT_NOT_ELIGIBLE = 8


class OperatorRoleE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        # One allocation for the whole class, not one per test -- see
        # test_e2e_second_identity.py's setUpClass for why: a released
        # RESERVED allocation is quarantined and still counts against the
        # pool's per-tenant quota. Reserved directly (not through
        # Stack.reserve) so an operation id can be captured when the
        # reservation happens to stay PENDING long enough to return 202 --
        # but on this stack (fake cloud, instant "complete" observation) a
        # vpc-scope reservation like this one commits synchronously (201),
        # and ADR 0011 notes that an operation id is then never exposed to
        # any caller at all, tenant included. test_get_operation_is_refused
        # below is conditional on that below for exactly this reason.
        key = run_key("operator-role")
        body = {
            "allocation_key": key,
            "scope": "vpc",
            "environment": cls.stack.env.get("IPAM_ENVIRONMENT", "development"),
            "region": "eu-central-1",
            "account_id": "000000000000",
            "prefix_length": 22,
            "description": "",
            "labels": {},
        }
        response = cls.stack.api("POST", "/v1/allocations", body,
                                 {"Idempotency-Key": f"e2e-{key}"})
        if response.status == 202:
            cls.operation_id = response.body["id"]
            cls.allocation = cls.stack.await_operation(response.body)
        elif response.status in (200, 201):
            cls.operation_id = None
            cls.allocation = response.body
        else:
            raise AssertionError(f"reserve returned {response.status}: {response.body}")
        cls.addClassCleanup(cls._release, cls.allocation["id"])

    @classmethod
    def _release(cls, allocation_id: str) -> None:
        response = cls.stack.api("DELETE", f"/v1/allocations/{allocation_id}")
        if response.status not in (200, 202, 204):
            raise AssertionError(
                f"cleanup release of {allocation_id} returned "
                f"{response.status}: {response.body}"
            )

    # -- reads: empty, never the developer's data ----------------------------

    def test_operator_lists_the_developers_allocation(self):
        # Package G3b2, internal/service/service.go List: the tenant conjunct
        # is `p.IsOperator() || a.TenantID == p.TenantID`, so an operator
        # lists committed allocations across tenants. `a.Committed` is
        # unchanged, so an uncommitted hold stays invisible to it.
        response = self.stack.api("GET", "/v1/allocations", token=self.stack.operator_token)
        self.assertEqual(response.status, 200, response.body)
        items = (response.body or {}).get("items", [])
        ids = [item.get("id") for item in items]
        self.assertIn(self.allocation["id"], ids, response.body)
        # Package G3b4: an operator's allocation now carries the owning
        # tenant -- this stack's only tenant is "developer"
        # (deploy/compose/fixtures/identities.yaml) -- and the key is present
        # on every row, not merely the one this test reserved.
        for item in items:
            self.assertEqual(item.get("tenant_id"), "developer",
                             f"operator allocation {item.get('id')} tenant_id={item.get('tenant_id')!r}")

        # The developer's own response is byte-for-byte what it always was:
        # tenant_id never reaches a tenant, operator configured or not.
        developer = self.stack.api("GET", "/v1/allocations")
        self.assertEqual(developer.status, 200, developer.body)
        self.assertNotIn("tenant_id", str(developer.body))

    def test_operator_sees_the_whole_estate_once(self):
        # Package G3b3, internal/service/service.go Findings: an operator gets
        # operatorFindings(st) -- every finding, with the occupancy fan-out's
        # per-tenant copies of one domain-level fact collapsed on the resource
        # identity domain.Finding now carries. This stack has one eligible
        # tenant, so the operator's set and the developer's are the same set;
        # what the collapse guarantees here is that it is the same SIZE too.
        operator = self.stack.api("GET", "/v1/findings", token=self.stack.operator_token)
        self.assertEqual(operator.status, 200, operator.body)
        operator_items = (operator.body or {}).get("items", [])

        developer = self.stack.api("GET", "/v1/findings")
        self.assertEqual(developer.status, 200, developer.body)
        developer_items = (developer.body or {}).get("items", [])

        operator_ids = [item.get("id") for item in operator_items]
        self.assertEqual(len(operator_ids), len(set(operator_ids)),
                         f"the operator's list repeats a finding: {operator_ids}")
        # Both reads are taken back to back and nothing here writes; the worker
        # resolves and re-opens a code inside one ledger transaction, so a pass
        # landing between them cannot show a row in an intermediate state, and
        # only a change to the estate itself could change the set.
        self.assertEqual(set(operator_ids), {item.get("id") for item in developer_items},
                         f"operator={operator.body} developer={developer.body}")

        # Package G3b4: an operator's findings response carries domain_id on
        # every row (this stack has one overlap domain, "local-development" --
        # deploy/compose/fixtures/pools.yaml), and tenant_id only where
        # allocation_id is set -- absent, not null, on a domain-level
        # (occupancy) row, since a grouped row has no single recipient tenant.
        for item in operator_items:
            self.assertIn("domain_id", item, f"domain_id missing from operator finding: {item}")
            self.assertEqual(item["domain_id"], "local-development", item)
            if item.get("allocation_id") is not None:
                self.assertEqual(item.get("tenant_id"), "developer",
                                 f"allocation-scoped finding without tenant_id: {item}")
            else:
                self.assertNotIn("tenant_id", item,
                                 f"a domain-level finding named a recipient tenant: {item}")

        # None of these four fields, nor the grouping, ever reaches a tenant's
        # own response -- with or without an operator configured.
        for item in developer_items:
            for key in ("tenant_id", "domain_id", "resource_type", "resource_id"):
                self.assertNotIn(key, item, f"{key} reached a tenant's findings response: {item}")

        # The ops-observer identity (package G3c) has a tenant, and that tenant
        # is eligible for no pool and owns nothing, so it sees none of this.
        observer = self.stack.api("GET", "/v1/findings", token=self.stack.ops_token)
        self.assertEqual(observer.status, 200, observer.body)
        self.assertEqual((observer.body or {}).get("items"), [])

    def test_operator_receives_every_configured_pool(self):
        # Package G3b2, internal/service/service.go Pools: an operator
        # receives every configured pool, so the list is not empty and
        # internal/transport/http.go's ADR 0011 stage-one refusal (403
        # no_eligible_pool, decided there on that full list) stops applying to
        # it -- without that handler changing. The ops-observer identity
        # (package G3c) still meets the refusal: it has a tenant, and that
        # tenant is eligible for no pool.
        response = self.stack.api("GET", "/v1/pools", token=self.stack.operator_token)
        self.assertEqual(response.status, 200, response.body)
        self.assertIn(POOL_ID, [item.get("id") for item in (response.body or {}).get("items", [])])

        observer = self.stack.api("GET", "/v1/pools", token=self.stack.ops_token)
        self.assertEqual(observer.status, 403, observer.body)
        self.assertEqual((observer.body or {}).get("error", {}).get("code"), "no_eligible_pool",
                         observer.body)

        developer = self.stack.api("GET", "/v1/pools")
        self.assertEqual(developer.status, 200, developer.body)
        self.assertIn(POOL_ID, [item.get("id") for item in (developer.body or {}).get("items", [])])

    def test_operator_reads_capacity_of_any_pool(self):
        # Package G3b2, internal/service/capacity.go: for an operator the pool
        # is selected by id alone -- tenant eligibility, environment, region
        # and account are dropped -- and the lifecycle breakdown loses its
        # single tenant conjunct. The occupied set and `allocatable` were
        # already domain-wide and are the same numbers the developer sees.
        response = self.stack.api("GET", f"/v1/pools/{POOL_ID}/capacity",
                                  token=self.stack.operator_token)
        self.assertEqual(response.status, 200, response.body)
        self.assertEqual((response.body or {}).get("pool_id"), POOL_ID, response.body)

        developer = self.stack.api("GET", f"/v1/pools/{POOL_ID}/capacity")
        self.assertEqual(developer.status, 200, developer.body)
        self.assertEqual(set((response.body or {}).get("by_prefix_length", {})),
                         set(developer.body.get("by_prefix_length", {})),
                         response.body)
        # An observation can finish, or a reservation can start, between these
        # two calls; either flips `complete` and therefore zeroes
        # `allocatable` for everybody. Compare only when both answers were
        # taken in the same state -- the unit boundary
        # (internal/service's TestOperatorCapacityIsTheEstateNotAnEntitlement)
        # asserts the same equality unconditionally, on a frozen state.
        if response.body.get("complete") == developer.body.get("complete"):
            for bits, counts in (response.body or {}).get("by_prefix_length", {}).items():
                self.assertEqual(counts.get("allocatable"),
                                 developer.body["by_prefix_length"][bits]["allocatable"],
                                 f"allocatable differs at /{bits}: {response.body}")

        # An id that names no configured pool is still 404 for an operator.
        unknown = self.stack.api("GET", "/v1/pools/pool_does_not_exist/capacity",
                                 token=self.stack.operator_token)
        self.assertEqual(unknown.status, 404, unknown.body)
        self.assertEqual((unknown.body or {}).get("error", {}).get("code"), "not_found",
                         unknown.body)

    def test_operator_reads_the_developers_allocation_by_id(self):
        # Package G3b2, internal/service/service.go Get: only the tenant
        # comparison is dropped; `a.Committed` is not. This case used to sit
        # in the 404 table below and was moved out deliberately -- it is a
        # read, and reads are what G3b2 grants.
        response = self.stack.api("GET", f"/v1/allocations/{self.allocation['id']}",
                                  token=self.stack.operator_token)
        self.assertEqual(response.status, 200, response.body)
        self.assertEqual(response.body.get("id"), self.allocation["id"], response.body)
        # Package G3b4: reading by id also carries the owning tenant.
        self.assertEqual(response.body.get("tenant_id"), "developer", response.body)

        # The developer's own GET by id is unaffected.
        own = self.stack.api("GET", f"/v1/allocations/{self.allocation['id']}")
        self.assertEqual(own.status, 200, own.body)
        self.assertNotIn("tenant_id", str(own.body))

    def test_api_did_not_warn_about_the_operator_but_did_about_ops_observer(self):
        # internal/config.IdentitiesWithoutPool skips operators (package
        # G3b1): they have no tenant, so the "eligible for no pool" warning
        # would be false for one. ops-observer (package G3c) still gets it.
        result = run(compose_argv("logs", "--no-log-prefix", "api"))
        self.assertEqual(result.returncode, 0, result.stderr)
        logs = result.stdout + result.stderr
        lines = logs.splitlines()
        self.assertFalse(
            any("eligible for no pool" in line and "ops-operator" in line for line in lines),
            "the API warned about ops-operator at start-up, which has no tenant to be "
            "ineligible with",
        )
        self.assertTrue(
            any("eligible for no pool" in line and "ops-observer" in line for line in lines),
            "the API did not warn about ops-observer at start-up",
        )
        self.assertFalse(any(self.stack.operator_token in line for line in lines),
                         "a credential appears in the API log")

    def test_operator_reads_are_logged_with_subject_and_no_credential(self):
        # Package G3b4: internal/transport/http.go's secureRead logs one
        # slog.Info("operator read", ...) line per operator request -- subject,
        # method, path, status, and rows for a list endpoint -- at the
        # transport layer, never into the ledger, and never with the token. A
        # fresh, identifiable GET here (rather than relying on another test's
        # timing) makes the assertion self-contained.
        path = f"/v1/allocations/{self.allocation['id']}"
        response = self.stack.api("GET", path, token=self.stack.operator_token)
        self.assertEqual(response.status, 200, response.body)

        result = run(compose_argv("logs", "--no-log-prefix", "api"))
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = (result.stdout + result.stderr).splitlines()
        self.assertTrue(
            any('"msg":"operator read"' in line
                and '"subject":"ops-operator"' in line
                and path in line
                and '"status":200' in line
                for line in lines),
            f"no operator-read log line named the operator's subject and {path}",
        )
        self.assertFalse(any(self.stack.operator_token in line for line in lines),
                         "a credential appeared in the API log")

    # -- the developer's allocation and operation are 404 for the operator --

    def test_operator_gets_404_not_403_for_the_developers_allocation(self):
        allocation_id = self.allocation["id"]
        # Every body/header here is otherwise valid, so a 404 can only be the
        # tenant-scoping check firing before any of the request's content is
        # ever considered -- not a validation error dressed up as a 404.
        # GET by id is NOT in this table: package G3b2 grants it, and
        # test_operator_reads_the_developers_allocation_by_id above asserts
        # the 200. Every entry that remains is a write, and none of them may
        # ever move.
        cases = [
            # internal/service/service.go Patch.
            ("PATCH", f"/v1/allocations/{allocation_id}",
             {"description": "operator probe"},
             {"Idempotency-Key": f"e2e-operator-patch-{allocation_id}", "If-Match": '"1"'}),
            # internal/service/service.go Bind.
            ("PUT", f"/v1/allocations/{allocation_id}/binding",
             {"provider": "aws", "resource_type": "vpc", "resource_id": "vpc-0operator0000000",
              "account_id": "000000000000", "region": "eu-central-1"},
             {"Idempotency-Key": f"e2e-operator-bind-{allocation_id}"}),
            # internal/service/service.go Release.
            ("DELETE", f"/v1/allocations/{allocation_id}", None, {}),
        ]
        for method, path, body, headers in cases:
            with self.subTest(method=method):
                response = self.stack.api(method, path, body, headers,
                                          token=self.stack.operator_token)
                self.assertEqual(response.status, 404, f"{method} {path}: {response.body}")
                self.assertEqual((response.body or {}).get("error", {}).get("code"), "not_found",
                                 f"{method} {path}: {response.body}")

        # internal/service/service.go Operation: `o.TenantID != p.TenantID`.
        # GET /v1/operations/{id} is deliberately not granted to an operator
        # in v1 (ADR 0011) -- this stays 404 even after later packages grant
        # other reads. Only exercised when setUpClass's reservation happened
        # to stay PENDING long enough to expose an operation id (202); on
        # this stack it normally commits synchronously (201), in which case
        # no caller -- tenant or operator -- is ever handed the operation id
        # at all (ADR 0011), so there is nothing to probe here. The unit
        # boundary in internal/transport/http_test.go's
        # TestOperatorDeniedByDefault covers this exact case unconditionally,
        # with a directly seeded operation.
        if self.operation_id is not None:
            op_response = self.stack.api("GET", f"/v1/operations/{self.operation_id}",
                                         token=self.stack.operator_token)
            self.assertEqual(op_response.status, 404, op_response.body)
            self.assertEqual((op_response.body or {}).get("error", {}).get("code"), "not_found",
                             op_response.body)

        # None of the refused attempts above may have touched the developer's
        # own view of their allocation.
        still_there = self.stack.api("GET", f"/v1/allocations/{allocation_id}")
        self.assertEqual(still_there.status, 200)
        self.assertEqual(still_there.body["state"], "RESERVED")

    # -- reservation is refused before any pool is ever considered ----------

    def test_operator_cannot_reserve(self):
        # internal/service/service.go validateRequest's first gate:
        # `if p.TenantID == ""` -> 403 forbidden, "authenticated principal
        # has no tenant" -- reached before the pool-eligibility check that
        # gives the ops-observer identity (package G3c) 422 policy_violation
        # instead, because the operator has no tenant to look a pool up by.
        key = run_key("operator-forbidden")
        response = self.stack.api("POST", "/v1/allocations", {
            "allocation_key": key, "scope": "vpc",
            "environment": self.stack.env.get("IPAM_ENVIRONMENT", "development"),
            "region": "eu-central-1", "account_id": "000000000000",
            "prefix_length": 22, "description": "", "labels": {},
        }, {"Idempotency-Key": f"e2e-{key}"}, token=self.stack.operator_token)
        self.assertEqual(response.status, 403)
        self.assertEqual((response.body or {}).get("error", {}).get("code"), "forbidden")

    # -- an invalid token is 401 ----------------------------------------------

    def test_invalid_token_is_401(self):
        response = self.stack.api("GET", "/v1/allocations",
                                  token="not-a-configured-token-at-all-000000")
        self.assertEqual(response.status, 401)

    # -- CLI: findings --fail-if-open under the operator ---------------------

    def test_findings_fail_if_open_judges_the_estate_under_the_operator(self):
        # Package G3b3 is what makes this gate worth running as an operator.
        # Under G3b2 the operator's findings list was empty, so the gate always
        # exited 0 -- a verdict about an entitlement rather than about the
        # estate. Now the list IS the estate, de-duplicated, so the exit code
        # is the estate's verdict: ExitFindings when anything in it is open,
        # 0 when nothing is.
        #
        # Which of the two this run gets is derived from the same response the
        # flag acts on, exactly as test_e2e_cli_findings.py derives it, rather
        # than asserted as a constant: the shipped cloud fixture observes no
        # resources, so a clean stack has no open occupancy finding and this
        # exits 0, but the modules that run before this one own their own state
        # and a constant would be a claim about them.
        printed = self.stack.cli("findings", token=self.stack.operator_token)
        open_ids = {item["id"] for item in printed["items"] if item.get("status") == "OPEN"}
        gated = self.stack.cli("findings", "--fail-if-open",
                               token=self.stack.operator_token,
                               expect_exit=EXIT_FINDINGS if open_ids else 0)
        if gated is not None:
            self.assertEqual(len(gated["items"]), len(printed["items"]),
                             "--fail-if-open must never hide a returned finding")

        # The estate the operator judges is the developer's findings and
        # nothing duplicated: one eligible tenant means every group has one
        # member, so the two open sets are equal, not merely the same size.
        developer_open = {item["id"] for item in self.stack.cli("findings")["items"]
                          if item.get("status") == "OPEN"}
        self.assertEqual(open_ids, developer_open,
                         "the operator's open findings must be the developer's, "
                         f"de-duplicated: operator={sorted(open_ids)} "
                         f"developer={sorted(developer_open)}")

        # The ops-observer identity (package G3c) is the contrast that makes
        # the grant legible: it sees NOTHING -- it has a tenant, that tenant is
        # eligible for no pool, and every read is scoped by it -- and still
        # exits ExitNotEligible rather than a clean 0, because a verdict over
        # an empty list proves nothing. That is the whole reason the operator
        # role exists: the same gate, run by an identity that can see the
        # estate, produces a verdict about the estate.
        self.stack.cli("findings", "--fail-if-open", token=self.stack.ops_token,
                       expect_exit=EXIT_NOT_ELIGIBLE)

        # The developer's tenant IS eligible for pool_dev_euc1, so the same
        # flag must never report ExitNotEligible for it, and -- because the two
        # sets are equal above -- it reaches the same verdict as the operator.
        self.stack.cli("findings", "--fail-if-open",
                       expect_exit=EXIT_FINDINGS if developer_open else 0)


if __name__ == "__main__":
    unittest.main()
