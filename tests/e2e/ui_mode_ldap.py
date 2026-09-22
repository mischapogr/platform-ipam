"""Minimum end-to-end proof for ui-proxy, `ldap` mode (package A5).

Named ui_mode_ldap.py, NOT test_e2e_ui_ldap.py, deliberately -- the same
opt-in convention browser_smoke.py already uses. `./tests/e2e/run-e2e.sh`
discovers every `test_e2e_*.py` file and runs it against the SHARED
platform-ipam-dev stack, which runs `basic` mode; a file matching that glob
would be swept into that run and fail there with 502s (no LDAPBackend, no
openldap service in that stack), not because ldap mode itself is broken.
This file is only ever invoked directly, by path, from
tests/e2e/run-ui-ldap.sh.

Unlike test_e2e_ui_proxy.py (`basic` mode, package A3), this file does NOT
use tests/e2e/harness.py: `ldap` mode is shipped as a throw-away overlay
(deploy/compose/compose.ui-ldap.yaml) run in its own, isolated Compose
project so it never touches the shared platform-ipam-dev project other
packages are using concurrently (see tests/e2e/run-ui-ldap.sh). It also
never uses the harness's host-loopback transport: this script is meant to
run *inside* a container attached to that throw-away project's network,
talking to the `ui-proxy` and `netbox` services by their Compose DNS names.
Stdlib only (urllib + http.cookiejar for the session/CSRF flow NetBox's own
login form requires), exactly like the harness's own HTTP layer.

Belt and suspenders: setUpClass ALSO skips (unittest.SkipTest) unless
IPAM_E2E_LDAP=1 is set -- an explicit env var only run-ui-ldap.sh sets -- so
running this file by hand against the wrong stack (or any stack that isn't
the throw-away ldap-mode project) skips cleanly instead of failing.

Covers the package A5 "Done when" list from docs/WORK_PLAN.md:
  * `viewer` logs in through NetBox's login form via the proxy and lands in
    platform-operators.
  * `viewer`'s session cannot create a prefix (403).
  * `outsider` (valid password, not in the required group) is refused.
  * a wrong password is refused.
  * a forged X-Remote-User: admin header (both spellings), unauthenticated,
    does not log anyone in.
  * /api/ through the proxy is 403 regardless of session.
The AUTH_LDAP_IS_SUPERUSER_DN left-unset claim in compose.ui-ldap.yaml
(`maintainer`, who legitimately gets write access through mirrored groups,
is not a superuser) is NOT proven here: this NetBox version's REST
UserSerializer and GraphQL UserType both omit is_superuser entirely
(confirmed empirically -- the field exists on neither API, apparently so a
read-only API caller cannot enumerate admin accounts). run-ui-ldap.sh
checks it directly through NetBox's ORM (`manage.py shell`) after this
suite passes, the only channel that can see it. (is_staff is not checked
anywhere, by anything: this NetBox version's User model has no is_staff
field at all -- see compose.ui-ldap.yaml's comment.)
"""

from __future__ import annotations

import http.cookiejar
import json
import os
import re
import unittest
import urllib.error
import urllib.parse
import urllib.request

UI_BASE_URL = os.environ.get("NETBOX_UI_LDAP_BASE_URL", "http://ui-proxy:8080")
NETBOX_BASE_URL = os.environ.get("NETBOX_INTERNAL_URL", "http://netbox:8080")
NETBOX_ADMIN_TOKEN = os.environ.get("IPAM_NETBOX_TOKEN", "")

VIEWER = ("viewer", os.environ.get("NETBOX_UI_LDAP_VIEWER_PASSWORD", "viewer-ldap-dev-password"))
MAINTAINER = ("maintainer", os.environ.get("NETBOX_UI_LDAP_MAINTAINER_PASSWORD", "maintainer-ldap-dev-password"))
OUTSIDER = ("outsider", os.environ.get("NETBOX_UI_LDAP_OUTSIDER_PASSWORD", "outsider-ldap-dev-password"))

CSRF_INPUT_RE = re.compile(r'name="csrfmiddlewaretoken" value="([^"]+)"')


class _Response:
    def __init__(self, status: int, headers, body: bytes, url: str):
        self.status = status
        self.headers = headers
        self.body = body
        # The URL after following redirects -- urllib's opener follows a
        # 302 automatically, so a request refused by a login-required view
        # still comes back as an outer status of 200: the login PAGE, not
        # the page that was actually asked for. `url` is what tells the two
        # apart.
        self.url = url

    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")


def _request(opener, method: str, url: str, data: bytes | None = None,
             headers: dict | None = None) -> _Response:
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with opener.open(req, timeout=15) as resp:
            return _Response(resp.status, dict(resp.headers), resp.read(), resp.geturl())
    except urllib.error.HTTPError as exc:
        return _Response(exc.code, dict(exc.headers or {}), exc.read(), exc.geturl())


def _new_session():
    """A cookie-jar-backed opener, one per test user -- mirrors a fresh
    browser: no credentials carried over between login attempts."""
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    return opener


def _csrf_token(opener, path: str = "/login/") -> str:
    resp = _request(opener, "GET", f"{UI_BASE_URL}{path}")
    match = CSRF_INPUT_RE.search(resp.text())
    if not match:
        raise AssertionError(f"no CSRF token found on {path}: status={resp.status} body={resp.text()[:500]!r}")
    return match.group(1)


def _login(opener, username: str, password: str) -> _Response:
    """Submits NetBox's own login form through ui-proxy -- the ldap-mode
    path: ui-proxy does not authenticate at all (Caddyfile.ldap has no
    basic_auth), so this is the ONLY way in. NetBox's LDAPBackend does the
    directory bind and group evaluation server-side."""
    token = _csrf_token(opener)
    body = urllib.parse.urlencode({
        "csrfmiddlewaretoken": token,
        "username": username,
        "password": password,
    }).encode()
    return _request(
        opener, "POST", f"{UI_BASE_URL}/login/", data=body,
        headers={
            "Content-Type": "application/x-www-form-urlencoded",
            "Referer": f"{UI_BASE_URL}/login/",
        },
    )


def _is_logged_in(opener) -> bool:
    resp = _request(opener, "GET", f"{UI_BASE_URL}/user/profile/")
    # NetBox's @login_required view 302s an unauthenticated request to
    # /login/?next=/user/profile/, and urllib's opener follows that
    # automatically -- so an outer status of 200 alone proves nothing; it is
    # the login PAGE's 200, not the profile page's. Only a final URL that is
    # still /user/profile/ (not /login/) means the session was accepted.
    return resp.status == 200 and "/login/" not in resp.url


def _netbox_admin_get(path: str) -> dict:
    """Reads NetBox's REST API on the INTERNAL network with the adapter's
    own superuser token -- never through ui-proxy, which refuses /api/
    regardless of session (rule 3, ADR 0006)."""
    req = urllib.request.Request(
        f"{NETBOX_BASE_URL}{path}",
        headers={"Authorization": f"Token {NETBOX_ADMIN_TOKEN}"},
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read())


def _netbox_user(username: str) -> dict | None:
    body = _netbox_admin_get(f"/api/users/users/?username={username}")
    results = body.get("results") or []
    return results[0] if results else None


class UILdapE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        # Explicit opt-in, the same pattern browser_smoke.py uses
        # (IPAM_E2E_BROWSER=1): this file's default 502-producing target,
        # http://ui-proxy:8080, only exists inside the throw-away
        # `ldap`-mode project tests/e2e/run-ui-ldap.sh brings up. The
        # filename already keeps this file out of run-e2e.sh's
        # `test_e2e_*.py` glob; this guard additionally protects anyone who
        # runs it directly, by hand, against the wrong stack.
        if os.environ.get("IPAM_E2E_LDAP") != "1":
            raise unittest.SkipTest(
                "set IPAM_E2E_LDAP=1 to run the ldap-mode UI tests; they target the throw-away "
                "project tests/e2e/run-ui-ldap.sh creates, not the shared platform-ipam-dev stack "
                "run-e2e.sh uses (which runs basic mode)"
            )
        if not NETBOX_ADMIN_TOKEN:
            raise unittest.SkipTest("IPAM_NETBOX_TOKEN not set; cannot read NetBox's admin API")

    # -- header trust is off in this mode -----------------------------------

    def test_forged_header_unauthenticated_does_not_log_anyone_in(self):
        for header_name in ("X-Remote-User", "X_Remote_User"):
            opener = _new_session()
            resp = _request(opener, "GET", f"{UI_BASE_URL}/user/profile/",
                             headers={header_name: "admin"})
            # See _is_logged_in: NetBox 302s an unauthenticated request to
            # /login/?next=..., which urllib follows, so a bare status of
            # 200 is the login PAGE, not a proof the header logged anyone
            # in. The final URL is what actually says which page this is.
            self.assertIn("/login/", resp.url,
                          f"a forged {header_name} with no NetBox session must not reach a page: "
                          f"status={resp.status} url={resp.url}")

    # -- wrong / refused credentials -----------------------------------------

    def test_wrong_password_is_refused(self):
        opener = _new_session()
        _login(opener, VIEWER[0], "not-the-real-password")
        self.assertFalse(_is_logged_in(opener), "a wrong password must not start a NetBox session")

    def test_outsider_valid_password_wrong_group_is_refused(self):
        opener = _new_session()
        _login(opener, *OUTSIDER)
        self.assertFalse(
            _is_logged_in(opener),
            "outsider has a correct LDAP password but is not a member of the AUTH_LDAP_REQUIRE_GROUP_DN "
            "group (platform-operators); login must still be refused",
        )

    # -- viewer: read-only ----------------------------------------------------

    def test_viewer_logs_in_and_is_a_platform_operator(self):
        # is_superuser is NOT checked here: this NetBox version's
        # UserSerializer (both the list and the detail view) and its
        # GraphQL UserType omit it entirely (confirmed empirically -- neither
        # /api/users/users/ nor a `{ user_list { isSuperuser } }` query
        # exposes it, presumably so a read-only API caller cannot enumerate
        # admin accounts). run-ui-ldap.sh checks it directly through
        # NetBox's ORM (`manage.py shell`) after this suite passes, which is
        # the only channel that can see it. is_staff is not checked
        # anywhere: this NetBox version's User model has no such field.
        opener = _new_session()
        login_resp = _login(opener, *VIEWER)
        self.assertIn(login_resp.status, (200, 302), login_resp.text()[:500])
        self.assertTrue(_is_logged_in(opener), "viewer's LDAP credential should start a NetBox session")

        netbox_viewer = _netbox_user(VIEWER[0])
        self.assertIsNotNone(netbox_viewer, "NetBox's LDAP backend never created/mirrored the viewer user")
        groups = {group["name"] for group in netbox_viewer.get("groups") or []}
        self.assertIn("platform-operators", groups,
                      f"AUTH_LDAP_MIRROR_GROUPS should have mirrored viewer into platform-operators: {groups}")
        self.assertNotIn("platform-inventory-maintainers", groups,
                         f"viewer is not an LDAP member of platform-inventory-maintainers: {groups}")

    def test_viewer_session_cannot_create_a_prefix(self):
        opener = _new_session()
        _login(opener, *VIEWER)
        self.assertTrue(_is_logged_in(opener))

        # NetBox's permission-required view refuses viewer (view-only)
        # before ever rendering the add form -- a hard 403 at the GET
        # stage, confirmed empirically, not a 200 form re-render with a
        # permission-denied message. There is therefore no CSRF token to
        # scrape and no form to submit: the 403 on GET already is the
        # "403/permission denial" this test needs to show. Also confirm the
        # object was never created, the same invariant a weaker denial
        # (a 200 re-render) would still have to satisfy.
        resp = _request(opener, "GET", f"{UI_BASE_URL}/ipam/prefixes/add/")
        self.assertEqual(resp.status, 403,
                         f"viewer (read-only) must be refused the prefix-add page: {resp.text()[:500]}")

        prefixes = _netbox_admin_get("/api/ipam/prefixes/?prefix=203.0.113.0/28")
        self.assertEqual(prefixes.get("count", 0), 0,
                         f"viewer (read-only) must not have been able to create a prefix: {prefixes}")

    # -- maintainer: read + write -------------------------------------------

    def test_maintainer_is_mirrored_into_both_groups(self):
        # is_staff/is_superuser: see the comment on
        # test_viewer_logs_in_and_is_a_platform_operator -- not visible
        # through any API this NetBox version exposes; checked by
        # run-ui-ldap.sh through the ORM instead.
        opener = _new_session()
        _login(opener, *MAINTAINER)
        self.assertTrue(_is_logged_in(opener), "maintainer's LDAP credential should start a NetBox session")

        netbox_maintainer = _netbox_user(MAINTAINER[0])
        self.assertIsNotNone(netbox_maintainer)
        groups = {group["name"] for group in netbox_maintainer.get("groups") or []}
        expected = {"platform-operators", "platform-inventory-maintainers"}
        extra = os.environ.get("NETBOX_UI_LDAP_EXTRA_GROUP")
        if extra:
            expected.add(extra)
        self.assertEqual(groups, expected,
                         f"maintainer's mirrored NetBox groups do not match its LDAP memberships: {groups}")

    # -- API/GraphQL stay blocked regardless of session ------------------------

    def test_api_is_refused_through_the_proxy_regardless_of_session(self):
        opener = _new_session()
        _login(opener, *VIEWER)
        resp = _request(opener, "GET", f"{UI_BASE_URL}/api/ipam/prefixes/")
        self.assertEqual(resp.status, 403, resp.text()[:500])

        anon_opener = _new_session()
        resp = _request(anon_opener, "GET", f"{UI_BASE_URL}/api/ipam/prefixes/")
        self.assertEqual(resp.status, 403, resp.text()[:500])


if __name__ == "__main__":
    unittest.main()
