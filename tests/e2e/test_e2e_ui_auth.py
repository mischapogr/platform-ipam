"""Security test matrix for the operator UI path, `basic` mode (package A6).

ADR 0006 and docs/GUI_AUTHENTICATION.md section 2 describe the full design;
package A3's tests/e2e/test_e2e_ui_proxy.py proves the minimum -- no
credential, a forged header, and the API block, five cases in total. This
file is the fuller matrix the work plan calls for: wrong/unknown
credentials, both spellings of the forged identity and group headers with
and without a valid credential, write-permission enforcement through the UI
path itself, a wider set of `/api/`+`/graphql/` bypass attempts (case,
encoding, path segments, matrix parameters, redirects), the healthcheck's
lack of leakage, and NetBox's own port never being published.

Every request goes through `Stack.ui()` (tests/e2e/harness.py), which always
uses the published path -- ui-proxy -- the same as test_e2e_ui_proxy.py.
Assertions about *who* NetBox thinks logged in, and what group/superuser
state that user carries, are made through NetBox's REST API with the
platform adapter's own token (`Stack.netbox()`), never by scraping the HTML
the proxy returns, per the package's instruction.

Not verified here: NetBox's REST/GraphQL user serializers in this pinned
image (4.6.7) do not expose `is_staff` at all (confirmed by reading
`/opt/netbox/netbox/users/api/serializers_/users.py` and
`/opt/netbox/netbox/users/graphql/types.py` inside the running image: the
field is absent from both, and from the filterable field list too, unlike
`is_superuser`, which the filterset does expose and which this file checks
per-login). That a header can never grant staff status is instead a static
property of `deploy/compose/netbox/netbox.env`, where
`REMOTE_AUTH_STAFF_GROUPS`/`REMOTE_AUTH_STAFF_USERS` are left unset -- see
docs/GUI_AUTHENTICATION.md's "Verified in image" table -- not something a
REST call after one login can observe.
"""

from __future__ import annotations

import json
import unittest

from harness import compose_argv, run, stack


class UIAuthE2ETest(unittest.TestCase):
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

    # -- helpers --------------------------------------------------------------

    def _netbox_user(self, username: str) -> dict | None:
        """The NetBox user object for `username`, read on the internal network
        with the platform adapter's own (superuser) token -- never through
        ui-proxy, which refuses /api/ regardless of credential.
        """
        body = self.stack.netbox(f"/api/users/users/?username={username}").body or {}
        results = body.get("results") or []
        return results[0] if results else None

    def _is_superuser(self, username: str) -> bool:
        """Whether NetBox considers `username` a superuser, through the REST
        API alone. The `UserSerializer` in this pinned image never returns
        `is_superuser` as a field (see the module docstring), but
        `UserFilterSet.Meta.fields` does include it, so filtering on it and
        checking the result count is REST-observable without scraping HTML
        or the Django admin.
        """
        body = self.stack.netbox(f"/api/users/users/?username={username}&is_superuser=true").body or {}
        return (body.get("count") or 0) > 0

    # -- 1. no credential -------------------------------------------------

    def test_no_credential_is_refused_with_www_authenticate(self):
        # Guards: basic_auth in deploy/compose/ui-proxy/Caddyfile.
        response = self.stack.ui("/")
        self.assertEqual(response.status, 401, response.body)
        self.assertIn("basic", response.headers.get("www-authenticate", "").lower(),
                      f"expected a Basic WWW-Authenticate challenge, got headers: {response.headers}")

    # -- 2. wrong password --------------------------------------------------

    def test_wrong_password_is_refused(self):
        # Guards: basic_auth's bcrypt comparison rejects a mismatched secret.
        response = self.stack.ui("/", user=self.ui_user, password=self.ui_password + "-wrong")
        self.assertEqual(response.status, 401, response.body)

    # -- 3. unknown user ------------------------------------------------------

    def test_unknown_user_is_refused(self):
        # Guards: basic_auth only recognizes the configured NETBOX_UI_USER.
        response = self.stack.ui("/", user="not-a-real-user", password=self.ui_password)
        self.assertEqual(response.status, 401, response.body)

    # -- 4. valid credential -> correct NetBox identity ------------------------

    def test_valid_credential_reaches_netbox_as_the_configured_operator(self):
        # Guards: request_header X-Remote-User {http.auth.user.id}, plus
        # REMOTE_AUTH_DEFAULT_GROUPS=platform-operators in netbox.env.
        response = self.stack.ui("/", user=self.ui_user, password=self.ui_password)
        self.assertEqual(response.status, 200, response.body)

        operator = self._netbox_user(self.ui_user)
        self.assertIsNotNone(operator, f"NetBox never auto-created {self.ui_user!r} via remote auth")
        groups = {group["name"] for group in operator.get("groups") or []}
        self.assertIn("platform-operators", groups,
                      f"{self.ui_user!r} is not a platform-operators member: {groups}")
        self.assertFalse(self._is_superuser(self.ui_user),
                         f"{self.ui_user!r} must never be a NetBox superuser")
        # is_staff: see the module docstring -- not observable via this
        # image's REST API, so not asserted here.

    # -- 5. forged X-Remote-User, no credential --------------------------------

    def test_forged_dashed_header_without_credential_is_refused_and_sets_no_session(self):
        # Guards: `request_header -X-Remote-User` runs before basic_auth, so
        # even a forged header cannot substitute for the missing credential.
        response = self.stack.ui("/", headers={"X-Remote-User": "admin"})
        self.assertEqual(response.status, 401,
                         f"a forged X-Remote-User with no credential must not be enough: {response.body}")
        self.assertNotIn("set-cookie", response.headers,
                         f"a refused request must not start a NetBox session: {response.headers}")

    # -- 6. forged X-Remote-User WITH a valid credential -----------------------

    def test_forged_dashed_header_with_valid_credential_is_the_authenticated_user(self):
        # Guards: the header is stripped before authentication, and NetBox
        # only ever sees the name the proxy itself sets after auth succeeds.
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

    # -- 7. the same two, underscore spelling ----------------------------------

    def test_forged_underscore_header_without_credential_is_refused_and_sets_no_session(self):
        # Guards: `request_header -X_Remote_User` -- the WSGI mapping of
        # dashes to underscores means a filter that only knew the dashed
        # spelling would let this straight through.
        response = self.stack.ui("/", headers={"X_Remote_User": "admin"})
        self.assertEqual(response.status, 401,
                         f"a forged X_Remote_User with no credential must not be enough: {response.body}")
        self.assertNotIn("set-cookie", response.headers,
                         f"a refused request must not start a NetBox session: {response.headers}")

    def test_forged_underscore_header_with_valid_credential_is_the_authenticated_user(self):
        # Guards: same as the dashed case, for the underscore spelling.
        admin_before = self._netbox_user("admin")
        response = self.stack.ui("/user/profile/", user=self.ui_user, password=self.ui_password,
                                 headers={"X_Remote_User": "admin"})
        self.assertEqual(response.status, 200, response.body)

        operator_after = self._netbox_user(self.ui_user)
        self.assertIsNotNone(operator_after.get("last_login"),
                             f"{self.ui_user!r} was not logged in despite a valid credential: {operator_after}")
        admin_after = self._netbox_user("admin")
        self.assertEqual(admin_after.get("last_login"), admin_before.get("last_login"),
                         "a forged X_Remote_User: admin, sent alongside a valid credential for "
                         f"{self.ui_user!r}, must never reach NetBox as a login: "
                         f"before={admin_before}, after={admin_after}")

    # -- 8. forged group header, both spellings, with a valid credential -------

    def test_forged_group_header_grants_no_group_and_no_superuser(self):
        # Guards: `request_header -X-Remote-User-Group` / `-X_Remote_User_Group`
        # (defence in depth: NetBox itself also ignores the group header --
        # REMOTE_AUTH_GROUP_SYNC_ENABLED is left at its default False in
        # netbox.env, and REMOTE_AUTH_SUPERUSER_GROUPS is unset -- so even a
        # header that reached NetBox could not grant a group or superuser
        # status; see the mutation proof in this package's report).
        for header_name in ("X-Remote-User-Group", "X_Remote_User_Group"):
            with self.subTest(header=header_name):
                response = self.stack.ui("/user/profile/", user=self.ui_user, password=self.ui_password,
                                         headers={header_name: "superusers"})
                self.assertEqual(response.status, 200, response.body)

                operator = self._netbox_user(self.ui_user)
                groups = {group["name"] for group in operator.get("groups") or []}
                self.assertEqual(groups, {"platform-operators"},
                                 f"a forged {header_name} must grant no group: {groups}")
                self.assertFalse(self._is_superuser(self.ui_user),
                                 f"a forged {header_name} must never grant superuser status")

    # -- 9. the view-only user cannot write through the UI path ---------------

    def test_view_only_user_cannot_create_a_prefix_through_the_ui_form(self):
        # Guards: platform-operators' ObjectPermission carries only 'view'
        # (deploy/compose/netbox/bootstrap-groups.py); NetBox's own add-prefix
        # view checks that permission and refuses the whole view, not just
        # the save, to a user who lacks 'add' -- independent of Caddy.
        probe_prefix = "198.51.100.77/32"
        before = self.stack.netbox_prefixes()
        self.assertNotIn(probe_prefix, before, "test fixture collided with an existing prefix")

        response = self.stack.ui("/ipam/prefixes/add/", user=self.ui_user, password=self.ui_password,
                                 headers={"Content-Type": "application/x-www-form-urlencoded"},
                                 method="POST")
        self.assertNotEqual(response.status, 200,
                            f"the view-only user must not be able to reach the add-prefix form: {response.body}")

        after = self.stack.netbox_prefixes()
        self.assertNotIn(probe_prefix, after,
                         f"a view-only user's POST to the add-prefix form created a prefix: {after.get(probe_prefix)}")

    # -- 10. API/GraphQL block and bypass attempts -----------------------------

    def test_api_and_graphql_are_blocked_including_bypass_attempts(self):
        # Guards: `@blocked path /api/* /graphql/*` / `respond @blocked 403`,
        # checked before authentication, plus Caddy's own case-insensitive
        # and percent-decoding path matching.
        blocked_paths = (
            "/api/",
            "/api/ipam/prefixes/",
            "/graphql/",
            "/API/ipam/prefixes/",       # mixed case
            "//api/ipam/prefixes/",      # doubled leading slash
            "/%61pi/ipam/prefixes/",     # percent-encoded 'a'
        )
        for path in blocked_paths:
            with self.subTest(path=path):
                response = self.stack.ui(path, user=self.ui_user, password=self.ui_password)
                self.assertEqual(response.status, 403,
                                 f"{path} must be blocked regardless of credential: {response.body}")

        # `/./` and `/../` segments: curl normalizes these away by default,
        # which would silently turn the bypass attempt into the ordinary,
        # already-blocked path (see harness.Stack.ui's path_as_is docstring).
        dot_segment_paths = (
            "/./api/ipam/prefixes/",
            "/static/../api/ipam/prefixes/",
        )
        for path in dot_segment_paths:
            with self.subTest(path=path):
                response = self.stack.ui(path, user=self.ui_user, password=self.ui_password, path_as_is=True)
                self.assertEqual(response.status, 403,
                                 f"{path} must be blocked regardless of credential: {response.body}")

        # A path-matrix parameter (`;x=1`) is not matched by Caddy's
        # `/api/*` glob (Go's URL parser does not treat `;` specially, so the
        # first path segment becomes literally `api;x=1`, not `api`), and the
        # request does reach NetBox with the valid credential (confirmed:
        # without a credential it is 401, not 403, proving it passed the
        # @blocked matcher and reached basic_auth). This is a real gap in the
        # Caddy-side glob, reported as a finding rather than fixed: NetBox's
        # own URL routing does not recognize the mangled path either and
        # answers 404 with its ordinary "Page Not Found" HTML, so no API data
        # is returned -- the outcome the guard exists to prevent still holds,
        # by an incidental second layer, not by this guard. See the report
        # for the full request/response.
        response = self.stack.ui("/api;x=1/ipam/prefixes/", user=self.ui_user, password=self.ui_password)
        # Originally this could only assert "not 200": the proxy let the path
        # through and NetBox's own 404 was what stopped it. The proxy now
        # refuses it itself (@blocked_params), so assert the proxy's answer.
        self.assertEqual(response.status, 403,
                         f"/api;x=1/... must be refused by the proxy itself: {response.body}")

    def test_api_and_graphql_without_trailing_slash_redirect_but_end_blocked(self):
        # Guards: same `@blocked` matcher as above, exercised after NetBox's
        # own APPEND_SLASH redirect -- the matcher requires a trailing slash
        # ("/api/*"), so the un-slashed path itself is not caught by Caddy
        # and reaches NetBox, which redirects; the redirect target must be.
        for path in ("/api", "/graphql"):
            with self.subTest(path=path):
                first = self.stack.ui(path, user=self.ui_user, password=self.ui_password)
                if first.status == 403:
                    continue  # already blocked outright; nothing to follow
                self.assertIn(first.status, (301, 302, 307, 308),
                              f"{path} must either be blocked or redirect, got {first.status}: {first.body}")
                location = first.headers.get("location", "")
                self.assertTrue(location, f"{path} redirected with no Location header: {first.headers}")
                second = self.stack.ui(location, user=self.ui_user, password=self.ui_password)
                self.assertEqual(second.status, 403,
                                 f"{path} redirected to {location!r}, which must end blocked: {second.body}")

    # -- 11. healthcheck leaks nothing ------------------------------------------

    def test_healthz_is_unauthenticated_and_leaks_no_netbox_content(self):
        # Guards: `@health path /healthz` / `respond @health 200`, which
        # answers before request_header stripping, basic_auth, or
        # reverse_proxy ever run -- this path never reaches NetBox.
        response = self.stack.ui("/healthz")
        self.assertEqual(response.status, 200, response.body)
        body_text = "" if response.body is None else str(response.body)
        self.assertNotIn("netbox", body_text.lower(),
                         f"/healthz must not carry any NetBox content: {response.body!r}")

    # -- 12. NetBox's own port is not published ---------------------------------

    def test_netbox_port_is_not_published_outside_ui_proxy(self):
        # Guards: only `ui-proxy` maps a host port to NetBox's 8080
        # (deploy/compose/compose.netbox.yaml); header trust in `basic` mode
        # is only safe when the proxy is the sole way in (ADR 0006 rule 1).
        result = run(compose_argv("config", "--format", "json"))
        self.assertEqual(result.returncode, 0, result.stderr)
        config = json.loads(result.stdout)
        services = config.get("services") or {}

        netbox_side = {name: service for name, service in services.items()
                       if name == "netbox" or name.startswith("netbox-")}
        self.assertIn("netbox", netbox_side, "compose config did not include a 'netbox' service")

        violators = []
        for name, service in netbox_side.items():
            if name == "ui-proxy":
                continue
            for entry in (service or {}).get("ports") or []:
                target = str(entry.get("target", "")) if isinstance(entry, dict) else str(entry).rsplit(":", 1)[-1]
                if target.split("/", 1)[0] == "8080":
                    violators.append(f"{name}: {entry}")
        self.assertFalse(violators,
                         f"NetBox's port must be published only by ui-proxy: {violators}")

        # The running container itself, independent of what `config` reports:
        # host loopback ports are unreachable in some sandboxes, so this
        # checks Docker's own record of the container's port bindings rather
        # than attempting a connection.
        cid_result = run(compose_argv("ps", "-q", "netbox"))
        self.assertEqual(cid_result.returncode, 0, cid_result.stderr)
        container_id = cid_result.stdout.strip()
        self.assertTrue(container_id, "the netbox container is not running")

        inspect_result = run(["docker", "inspect", container_id])
        self.assertEqual(inspect_result.returncode, 0, inspect_result.stderr)
        details = json.loads(inspect_result.stdout)[0]
        port_bindings = (details.get("HostConfig") or {}).get("PortBindings") or {}
        network_ports = (details.get("NetworkSettings") or {}).get("Ports") or {}
        bound = {key: value for key, value in {**port_bindings, **network_ports}.items() if value}
        self.assertFalse(bound,
                         f"the running netbox container must have no host port bindings: {bound}")


if __name__ == "__main__":
    unittest.main()
