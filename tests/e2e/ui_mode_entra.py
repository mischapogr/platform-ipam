"""Minimum end-to-end proof for ui-proxy, `entra` mode (package A4).

Like tests/e2e/test_e2e_ui_ldap.py (package A5) and UNLIKE
tests/e2e/harness.py, this file does not use the harness: `entra` mode ships
as a throw-away overlay (deploy/compose/compose.ui-entra.yaml) run in its own
isolated Compose project (tests/e2e/run-ui-entra.sh) so it never touches the
shared platform-ipam-dev project other packages use concurrently. This file
is meant to run *inside* a container attached to that throw-away project's
network, talking to `ui-proxy`, `mock-oidc` and `netbox` by their Compose DNS
names -- never through a host-published port. Stdlib only: urllib +
http.cookiejar drive the whole OIDC round trip, the same way
test_e2e_ui_ldap.py drives NetBox's own login form.

Named outside the `test_e2e_*.py` glob run-e2e.sh discovers (like
tests/e2e/browser_smoke.py): this suite needs a throw-away Compose project
and a network-attached test container, neither of which exist when
run-e2e.sh runs its suite against the shared platform-ipam-dev project in
`basic` mode. As a second, independent guard against that -- the naming
convention alone is not self-enforcing -- every test below skips unless
IPAM_E2E_UI_ENTRA=1, which only tests/e2e/run-ui-entra.sh sets.

The mock issuer's login page (deploy/compose/compose.ui-entra.yaml's
JSON_CONFIG) is a single HTML form that POSTs a `username` field back to the
exact URL it was served from, and returns a `groups` claim for that username
from its own static configuration -- no interactive browser or `claims` form
field is needed here. The overlay defines three claim shapes:
  * ALLOWED_USER ("operator-alice") -- the allowed Entra-style group UUID.
  * DENIED_USER ("denied-bob") -- a different group UUID.
  * OVERAGE_USER ("overage-carol") -- an overage pointer but no groups list.

Covers the package A4 "Done when" list from docs/WORK_PLAN.md:
  * an unauthenticated request is redirected to the issuer, not served.
  * a full login through the mock issuer reaches a NetBox page as a user who
    is a member of platform-operators and NOT a superuser.
  * a forged X-Remote-User (both spellings), no session, is not honoured.
  * a forged X-Remote-User (both spellings) WITH a valid session yields the
    session's user, not the forgery.
  * /api/ through the proxy is 403.
  * a user outside the allowed group is refused.
"""

from __future__ import annotations

import http.cookiejar
import json
import os
import unittest
import urllib.error
import urllib.parse
import urllib.request

UI_BASE_URL = os.environ.get("NETBOX_UI_ENTRA_BASE_URL", "http://ui-proxy:8080")
NETBOX_BASE_URL = os.environ.get("NETBOX_INTERNAL_URL", "http://netbox:8080")
NETBOX_ADMIN_TOKEN = os.environ.get("IPAM_NETBOX_TOKEN", "")

ALLOWED_USER = "operator-alice"
DENIED_USER = "denied-bob"
OVERAGE_USER = "overage-carol"

MOCK_ISSUER_LOGIN_MARKER = "Mock OAuth2 Server Sign-in"


class _Response:
    def __init__(self, status: int, headers: dict, body: bytes, url: str):
        self.status = status
        # Header names lower-cased: HTTP header lookup must be
        # case-insensitive, and a plain dict built from the raw message
        # would not be.
        self.headers = {k.lower(): v for k, v in headers.items()}
        self.body = body
        self.url = url

    def text(self) -> str:
        return self.body.decode("utf-8", errors="replace")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Returns the 30x response itself instead of following it.

    Subclassing HTTPRedirectHandler (rather than just omitting one) is what
    stops urllib.request.build_opener from installing its own default
    redirect handler in its place.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _request(opener, method: str, url: str, data: bytes | None = None,
             headers: dict | None = None) -> _Response:
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with opener.open(req, timeout=20) as resp:
            return _Response(resp.status, dict(resp.headers), resp.read(), resp.geturl())
    except urllib.error.HTTPError as exc:
        return _Response(exc.code, dict(exc.headers or {}), exc.read(), exc.geturl())


def _new_session(follow_redirects: bool = True):
    """A cookie-jar-backed opener, one per test user -- mirrors a fresh
    browser: no cookies carried over between independent attempts."""
    jar = http.cookiejar.CookieJar()
    handlers = [urllib.request.HTTPCookieProcessor(jar)]
    if not follow_redirects:
        handlers.append(_NoRedirect())
    return urllib.request.build_opener(*handlers)


def _login(opener, username: str) -> _Response:
    """Drives the whole OIDC round trip through ui-proxy -> oauth2-proxy ->
    the mock issuer's login form -> oauth2-proxy's callback -> back to
    ui-proxy, the way a browser would, in two requests on a redirect-
    following opener:

    1. GET ui-proxy's protected root. No session yet, so ui-proxy's
       forward_auth sends this to oauth2-proxy's /oauth2/start, which begins
       the OIDC flow and lands on the mock issuer's login page -- captured
       here via .url, the page's own address (query string included), which
       is exactly what its form implicitly POSTs back to.
    2. POST `username` to that address. The mock issuer redirects to
       oauth2-proxy's callback (which redeems the code, checks
       --allowed-group, and on success sets a session cookie); oauth2-proxy
       redirects back to ui-proxy; ui-proxy's forward_auth now succeeds and
       proxies through to NetBox. The final response is whatever NetBox (or
       oauth2-proxy's own refusal, if the group check failed) returns.
    """
    landed = _request(opener, "GET", f"{UI_BASE_URL}/")
    if MOCK_ISSUER_LOGIN_MARKER not in landed.text():
        raise AssertionError(
            f"expected to land on the mock issuer's login page; got status={landed.status} "
            f"url={landed.url} body={landed.text()[:300]!r}"
        )
    body = urllib.parse.urlencode({"username": username}).encode()
    return _request(opener, "POST", landed.url, data=body,
                     headers={"Content-Type": "application/x-www-form-urlencoded"})


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
    body = _netbox_admin_get(f"/api/users/users/?username={urllib.parse.quote(username)}")
    results = body.get("results") or []
    return results[0] if results else None


def _netbox_user_is_superuser(username: str) -> bool:
    """Whether NetBox itself considers `username` a superuser.

    Verified against the pinned image (netboxcommunity/netbox:v4.6.7-5.0.2):
    the /api/users/users/ serializer does NOT include `is_superuser` in its
    output (so `user["is_superuser"]` raises KeyError), and this NetBox's
    `users.models.User` has no `is_staff` field at all (AttributeError from
    `manage.py shell`) -- it is not Django's default AbstractUser. Filtering
    by `is_superuser` on this endpoint IS honoured, though, even though the
    field is not serialized back: querying for the bootstrapped `admin`
    (a real superuser) with `is_superuser=true` returns exactly that one
    user, confirming the filter is real rather than silently ignored (an
    unrecognised query parameter, like `is_staff`, is dropped with no error
    and no effect on the result set -- checked the same way).
    """
    query = urllib.parse.urlencode({"username": username, "is_superuser": "true"})
    body = _netbox_admin_get(f"/api/users/users/?{query}")
    return (body.get("count") or 0) >= 1


class UIEntraE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        # Second, independent guard beyond this file's name not matching
        # run-e2e.sh's `test_e2e_*.py` discovery glob: only
        # tests/e2e/run-ui-entra.sh sets this, against its own throw-away
        # Compose project. Without it, UI_BASE_URL/NETBOX_BASE_URL above
        # would resolve nowhere (or somewhere unintended) if this file were
        # ever picked up by a discovery run against the shared dev stack.
        if os.environ.get("IPAM_E2E_UI_ENTRA") != "1":
            raise unittest.SkipTest(
                "IPAM_E2E_UI_ENTRA != 1; run via tests/e2e/run-ui-entra.sh, "
                "not directly and not through run-e2e.sh's discovery"
            )
        if not NETBOX_ADMIN_TOKEN:
            raise unittest.SkipTest("IPAM_NETBOX_TOKEN not set; cannot read NetBox's admin API")

    # -- unauthenticated / forged-header paths never reach NetBox -----------

    def test_unauthenticated_request_is_redirected_to_the_issuer_not_served(self):
        opener = _new_session(follow_redirects=False)
        first = _request(opener, "GET", f"{UI_BASE_URL}/")
        self.assertEqual(first.status, 302,
                          f"an unauthenticated request must not be served directly: {first.status}")
        start_location = first.headers.get("location", "")
        self.assertTrue(start_location.startswith("/oauth2/start"),
                         f"expected a redirect to /oauth2/start, got {start_location!r}")

        second = _request(opener, "GET", f"{UI_BASE_URL}{start_location}")
        self.assertEqual(second.status, 302, f"/oauth2/start must itself redirect: {second.status}")
        issuer_location = second.headers.get("location", "")
        self.assertTrue(
            issuer_location.startswith("http://mock-oidc:8080/default/authorize"),
            f"expected the second redirect to reach the mock issuer, got {issuer_location!r}",
        )

    def test_forged_header_no_session_is_not_honoured(self):
        opener = _new_session(follow_redirects=False)
        for header_name in ("X-Remote-User", "X_Remote_User"):
            resp = _request(opener, "GET", f"{UI_BASE_URL}/", headers={header_name: "forged-nosession"})
            self.assertEqual(resp.status, 302,
                              f"a forged {header_name} with no session must not be served: {resp.status}")

    # -- the proxy serves browsers only --------------------------------------

    def test_api_and_graphql_are_blocked_regardless_of_auth(self):
        opener = _new_session()
        for path in ("/api/", "/graphql/"):
            resp = _request(opener, "GET", f"{UI_BASE_URL}{path}")
            self.assertEqual(resp.status, 403, f"{path} through the proxy must be 403: {resp.status}")

    # -- a real login --------------------------------------------------------

    def test_login_reaches_netbox_as_a_readonly_non_superuser(self):
        opener = _new_session()
        final = _login(opener, ALLOWED_USER)
        self.assertEqual(final.status, 200,
                          f"login as {ALLOWED_USER} must reach a NetBox page: "
                          f"status={final.status} body={final.text()[:500]!r}")
        self.assertIn("NetBox", final.text())
        self.assertNotIn(MOCK_ISSUER_LOGIN_MARKER, final.text(),
                          "the final page must be NetBox, not the mock issuer's own login form")

        user = _netbox_user(ALLOWED_USER)
        self.assertIsNotNone(user, "oauth2-proxy verified the login but NetBox never created/saw the user")
        # Not user["is_superuser"]/["is_staff"]: this NetBox's serializer
        # does not expose either field, and its User model has no is_staff
        # field at all -- see _netbox_user_is_superuser's docstring.
        self.assertFalse(_netbox_user_is_superuser(ALLOWED_USER),
                          f"{ALLOWED_USER} must not be a superuser: {user}")
        groups = {group["name"] for group in user.get("groups") or []}
        self.assertEqual(
            groups, {"platform-operators"},
            f"an unmapped entra user must land in platform-operators only (no group sync "
            f"is configured -- docs/GUI_AUTHENTICATION.md, package A4): {groups}",
        )

        # A forged header WITH this valid session must still yield the
        # session's user, not the forgery -- proven two ways: the request
        # still succeeds (the session, not the header, is what authenticates
        # it), and NetBox never created a user for the forged name.
        forged_name = "forged-with-session"
        resp = _request(opener, "GET", f"{UI_BASE_URL}/",
                         headers={"X-Remote-User": forged_name, "X_Remote_User": forged_name})
        self.assertEqual(resp.status, 200,
                          "the authenticated session must still reach NetBox despite the forged header")
        self.assertIsNone(
            _netbox_user(forged_name),
            "a forged X-Remote-User must never create/reach a NetBox user, even alongside a valid session",
        )

        # Forging a name that does not exist is the easy case: an honoured
        # forgery would show up as a newly created user. Forging an EXISTING
        # privileged name leaves no such trace, and it is the attack that
        # matters in a header-trust setup -- so impersonate the bootstrapped
        # superuser, in every spelling a client could try, and show that its
        # last_login does not move. Added in review of package A4.
        admin_name = os.environ.get("NETBOX_SUPERUSER_NAME", "admin")
        before = (_netbox_user(admin_name) or {}).get("last_login")
        for header in ("X-Remote-User", "X_Remote_User", "X-Auth-Request-User",
                       "X_Auth_Request_User", "X-Forwarded-User",
                       "X-Auth-Request-Preferred-Username"):
            resp = _request(opener, "GET", f"{UI_BASE_URL}/user/profile/",
                             headers={header: admin_name})
            self.assertEqual(resp.status, 200, f"{header}: the session must still work")
            # Positive as well as negative: in a fresh project the superuser
            # has never logged in, so an unchanged last_login alone would
            # prove nothing. The profile page must be the SESSION user's.
            self.assertIn(ALLOWED_USER, resp.text(),
                          f"{header}: the profile page must show the session's own user")
            self.assertNotIn(f">{admin_name}<", resp.text(),
                             f"{header}: the profile page must not be the superuser's")
        after = (_netbox_user(admin_name) or {}).get("last_login")
        self.assertEqual(before, after,
                         f"a forged header logged someone in as {admin_name}: "
                         f"last_login moved from {before} to {after}")

    # -- group gating at oauth2-proxy ----------------------------------------

    def test_user_outside_the_allowed_group_is_refused(self):
        opener = _new_session()
        final = _login(opener, DENIED_USER)
        self.assertEqual(
            final.status, 403,
            f"a user outside --allowed-group must be refused by oauth2-proxy, never reaching NetBox: "
            f"status={final.status} body={final.text()[:300]!r}",
        )
        self.assertNotIn("NetBox", final.text())
        self.assertIsNone(_netbox_user(DENIED_USER),
                           "a user outside the allowed group must never reach/create a NetBox user")

    def test_group_overage_without_lookup_is_refused(self):
        opener = _new_session()
        final = _login(opener, OVERAGE_USER)
        self.assertEqual(
            final.status, 403,
            f"an overage pointer without a groups list must fail closed: "
            f"status={final.status} body={final.text()[:300]!r}",
        )
        self.assertIsNone(_netbox_user(OVERAGE_USER),
                          "an overage user must never reach/create a NetBox user")


if __name__ == "__main__":
    unittest.main()
