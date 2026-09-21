"""Minimum end-to-end proof for ui-proxy, `basic` mode (package A3).

ADR 0006 and docs/GUI_AUTHENTICATION.md section 2 describe the full contract;
this file proves only what package A3 itself is responsible for. The complete
security matrix (wrong password, /graphql/, NetBox's port unreachable from
the host, a mutation-based proof that each guard actually guards something)
is package A6's job, in a later tests/e2e/test_e2e_ui_auth.py.

Every request here goes through `Stack.ui()` (tests/e2e/harness.py), which
always uses the published path -- ui-proxy -- never the internal NetBox
address `netbox()`/`netbox_as()` use.
"""

from __future__ import annotations

import unittest

from harness import stack


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


if __name__ == "__main__":
    unittest.main()
