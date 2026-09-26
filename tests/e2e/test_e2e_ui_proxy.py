"""Minimum end-to-end proof for ui-proxy, `basic` mode (package A3).

ADR 0006 and docs/GUI_AUTHENTICATION.md section 2 describe the full contract;
this file proves only what package A3 itself is responsible for. The complete
security matrix (wrong password, /graphql/, NetBox's port unreachable from
the host, a mutation-based proof that each guard actually guards something)
is package A6's job, in a later tests/e2e/test_e2e_ui_auth.py.

Every request here goes through `Stack.ui()` (tests/e2e/harness.py), which
always uses the published path -- ui-proxy -- never the internal NetBox
address `netbox()`/`netbox_as()` use.

This module also carries package T2's quota canary
(`test_zz_pool_ends_below_capacity`, below): `unittest`'s `TestLoader` sorts
discovered files alphabetically within each `IPAM_E2E_PATTERN` half and sorts
classes and test methods within a file alphabetically by name (not by
definition order), and this file -- `test_e2e_ui_proxy.py` -- is the
alphabetically last module matching `test_e2e_[j-z]*.py`, `UIProxyE2ETest`
its only class. Placing the assertion here, named to sort after every other
method in the class, makes it the very last thing the second half of the
suite does.
"""

from __future__ import annotations

import unittest

from harness import ROOT, stack

POOL_ID = "pool_dev_euc1"


class UIProxyE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        cls.ui_user = cls.stack.env.get("NETBOX_UI_USER", "")
        cls.ui_password = cls.stack.env.get("NETBOX_UI_PASSWORD", "")
        if not cls.ui_user or not cls.ui_password:
            raise unittest.SkipTest(
                "NETBOX_UI_USER/NETBOX_UI_PASSWORD are not set in deploy/compose/.env; "
                "re-run deploy/compose/create-env.sh or add them manually"
            )

    def _netbox_user(self, username: str) -> dict | None:
        """The NetBox user object for `username`, read on the internal network
        with the platform adapter's own (superuser) token -- never through
        ui-proxy, which refuses /api/ regardless of credential.
        """
        body = self.stack.netbox(f"/api/users/users/?username={username}").body or {}
        results = body.get("results") or []
        return results[0] if results else None

    # -- no credential --------------------------------------------------------

    def test_no_credential_is_refused_with_www_authenticate(self):
        response = self.stack.ui("/")
        self.assertEqual(response.status, 401, response.body)
        self.assertIn("basic", response.headers.get("www-authenticate", "").lower(),
                      f"expected a Basic WWW-Authenticate challenge, got headers: {response.headers}")

    def test_forged_header_without_credential_is_still_refused(self):
        # If the proxy trusted the header before authenticating, this would
        # be a 200 as "admin". It must not be.
        response = self.stack.ui("/", headers={"X-Remote-User": "admin"})
        self.assertEqual(response.status, 401,
                         f"a forged X-Remote-User with no credential must not be enough: {response.body}")

    # -- valid credential ------------------------------------------------------

    def test_valid_credential_reaches_netbox_as_a_platform_operator(self):
        response = self.stack.ui("/", user=self.ui_user, password=self.ui_password)
        self.assertEqual(response.status, 200, response.body)

        operator = self._netbox_user(self.ui_user)
        self.assertIsNotNone(operator, f"NetBox never auto-created {self.ui_user!r} via remote auth")
        groups = {group["name"] for group in operator.get("groups") or []}
        self.assertIn("platform-operators", groups,
                      f"{self.ui_user!r} is not a platform-operators member: {groups}")

    def test_forged_header_with_valid_credential_is_the_authenticated_user_not_the_forgery(self):
        # A before/after delta on "admin", not an assumed null baseline: this
        # development NetBox is long-lived and its superuser may carry a
        # last_login from an earlier session (for example an older
        # browser_smoke.py, before package A3, which logged into NetBox's own
        # form as the superuser). What must hold regardless of that history
        # is that THIS request -- a valid ui-proxy credential for `ui_user`
        # carrying a forged X-Remote-User: admin -- never advances it.
        admin_before = self._netbox_user("admin")
        self.assertIsNotNone(admin_before, "the bootstrapped superuser 'admin' is unexpectedly missing")

        response = self.stack.ui("/user/profile/", user=self.ui_user, password=self.ui_password,
                                 headers={"X-Remote-User": "admin"})
        self.assertEqual(response.status, 200, response.body)

        operator_after = self._netbox_user(self.ui_user)
        self.assertIsNotNone(operator_after.get("last_login"),
                             f"{self.ui_user!r} was not logged in despite a valid credential: {operator_after}")

        admin_after = self._netbox_user("admin")
        self.assertEqual(admin_after.get("last_login"), admin_before.get("last_login"),
                         "a forged X-Remote-User: admin, sent alongside a valid credential for "
                         f"{self.ui_user!r}, must never reach NetBox as a login: "
                         f"before={admin_before}, after={admin_after}")

    # -- API/GraphQL block -------------------------------------------------

    def test_api_is_refused_through_the_proxy_regardless_of_credential(self):
        response = self.stack.ui("/api/ipam/prefixes/")
        self.assertEqual(response.status, 403, response.body)

        response = self.stack.ui("/api/ipam/prefixes/", user=self.ui_user, password=self.ui_password)
        self.assertEqual(response.status, 403,
                         "the platform adapter's internal path (http://netbox:8080) must stay the "
                         f"only way to NetBox's REST API; ui-proxy answered: {response.body}")

    # -- T2: the suite's own quota canary --------------------------------

    def test_zz_pool_ends_below_capacity(self):
        """The last thing the second half of the suite does (see this
        module's own docstring for why this test lands here): read the
        tenant's own committed allocations in `pool_dev_euc1` and assert
        their count is strictly below `max_unreclaimed_allocations_per_tenant`.

        This does NOT use GET /v1/pools/{id}/capacity's by_prefix_length
        buckets: that endpoint reports BLOCK occupancy at each configured
        prefix length independently (internal/service/capacity.go,
        occupiedBlocks), so a single committed /20 allocation shows as 4
        blocks in the /22 bucket and 1 in the /20 bucket -- summing buckets
        double-counts, and trusting one bucket alone silently misrepresents
        any allocation not of that exact size. test_e2e_allocation.py's
        test_mixed_sizes_never_overlap and test_e2e_import.py's
        test_01_imported_network_is_never_allocated both commit real /20
        allocations in this pool, so that ambiguity is not theoretical here.

        GET /v1/allocations (internal/service/service.go's Service.List)
        returns exactly the committed allocations this tenant owns --
        `a.Committed` is the filter, unconditional on scope or prefix
        length -- so summing its rows is exact for what it covers, with no
        block-occupancy math. It IS a cursor page (api/openapi.yaml's
        `Limit` parameter defaults to 50, maximum 200), so `limit=200` is
        passed explicitly rather than relying on the default happening to
        exceed the pool's 32-slot quota.

        This is deliberately the COMMITTED count, not the full quota
        `internal/service/service.go`'s countTenant enforces: countTenant
        also counts an allocation that is RESERVED or ADOPTED but not yet
        committed (a PENDING hold, Service.List's `a.Committed` excludes
        it), so an in-flight pending operation at the exact moment this
        test runs would not be reflected here. Every module that seeds a
        pending hold in this suite resolves or cancels it inside its own
        class before the class ends (see tests/e2e/README.md's "Quota
        budget" section), so by the time this, the very last test of the
        second half, runs, nothing should still be pending -- but a
        pending hold that outlived its own module's cleanup is exactly the
        kind of leak this canary cannot see, and is worth remembering if a
        future `quota_exceeded` is not explained by this table.
        """
        import yaml

        pools = yaml.safe_load(
            (ROOT / "deploy/compose/fixtures/pools.yaml").read_text(encoding="utf-8")
        )
        pool_cfg = next((p for p in pools.get("pools", []) if p.get("id") == POOL_ID), None)
        self.assertIsNotNone(pool_cfg, f"pool {POOL_ID} is not defined in the Compose fixture")
        max_allocations = pool_cfg["max_unreclaimed_allocations_per_tenant"]

        response = self.stack.api("GET", "/v1/allocations?limit=200")
        self.assertEqual(response.status, 200, response.body)
        listed = response.body
        self.assertIsInstance(listed, dict, listed)
        self.assertIsInstance(listed.get("items"), list, listed)
        self.assertIsNone(listed.get("next_cursor"),
                          f"/v1/allocations paginated past limit=200: {listed}")
        committed = [row for row in listed["items"]
                     if row.get("pool_id") == POOL_ID and row.get("state") != "RELEASED"]

        self.assertLess(
            len(committed), max_allocations,
            f"{POOL_ID} holds {len(committed)} of {max_allocations} per-tenant "
            "allocation quota slots (every committed RESERVED/ACTIVE/QUARANTINED "
            "allocation counts) -- at or over budget. See tests/e2e/README.md's "
            "\"Quota budget\" section for the per-module accounting and which "
            "module's slot to look at first before adding another reservation "
            "or adoption to this pool."
        )


if __name__ == "__main__":
    unittest.main()
