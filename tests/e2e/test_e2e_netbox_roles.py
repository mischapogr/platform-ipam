"""End-to-end coverage for the read-only NetBox operator groups (package A2).

`platform-operators` is a view-only NetBox group intended for the operator
UI's eventual auth path (docs/GUI_AUTHENTICATION.md section 2, rule 5: "every
authenticated person lands in a read-only group"). This suite proves the
group's ObjectPermission actually constrains a NetBox user, using the
`e2e-viewer` development account and v1 API token that
`deploy/compose/netbox/bootstrap-groups.py` creates for this test only, when
NETBOX_E2E_VIEWER_TOKEN is set.

Whether the platform adapter's own token can still write is not tested here
by design: mutating through the adapter's credential risks leaving behind
state another e2e test does not expect, for a property (`platform_allocation_id`)
that is not what this group is about (see tests/e2e/test_e2e_allocation.py
for that coverage).
"""

from __future__ import annotations

import unittest

from harness import stack


class NetBoxOperatorRolesE2ETest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.stack = stack()
        cls.viewer_token = cls.stack.env.get("NETBOX_E2E_VIEWER_TOKEN", "")
        if not cls.viewer_token:
            raise unittest.SkipTest(
                "NETBOX_E2E_VIEWER_TOKEN is not set in deploy/compose/.env; "
                "re-run deploy/compose/create-env.sh or add it manually"
            )

    def test_viewer_can_read_prefixes(self):
        response = self.stack.netbox_as(self.viewer_token, "GET", "/api/ipam/prefixes/?limit=1")
        self.assertEqual(response.status, 200,
                         f"platform-operators viewer could not read prefixes: {response.body}")

    def test_viewer_cannot_create_prefixes(self):
        # A syntactically valid, non-colliding prefix: the assertion is about
        # authorization being refused, not about input validation being
        # refused for an unrelated reason before the permission check runs.
        response = self.stack.netbox_as(self.viewer_token, "POST", "/api/ipam/prefixes/", {
            "prefix": "203.0.113.0/32",
            "status": "active",
            "description": "test_e2e_netbox_roles: must never be created",
        })
        self.assertEqual(response.status, 403,
                         f"platform-operators viewer was able to write a prefix: {response.body}")


if __name__ == "__main__":
    unittest.main()
