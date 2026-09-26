"""End-to-end coverage for the NetBox operator groups (package A2).

`platform-operators` is a view-only NetBox group intended for the operator
UI's eventual auth path (docs/GUI_AUTHENTICATION.md section 2, rule 5: "every
authenticated person lands in a read-only group"). This suite proves the
group's ObjectPermission actually constrains a NetBox user, using the
`e2e-viewer` development account and v1 API token that
`deploy/compose/netbox/bootstrap-groups.py` creates for this test only, when
NETBOX_E2E_VIEWER_TOKEN is set. The optional `e2e-maintainer` account proves
that add/change permissions actually work, and that delete is still refused.
The adapter credential only cleans up the exact prefix this test created.
"""

from __future__ import annotations

import unittest
from uuid import uuid4

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

    def test_inventory_maintainer_can_create_and_change_but_not_delete_prefix(self):
        maintainer_token = self.stack.env.get("NETBOX_E2E_MAINTAINER_TOKEN", "")
        if not maintainer_token:
            self.skipTest("NETBOX_E2E_MAINTAINER_TOKEN is not set in deploy/compose/.env")

        cidr = "192.0.2.253/32"
        marker = f"test_e2e_netbox_roles:{uuid4().hex}"
        created_description = marker + ":created"
        updated_description = marker + ":updated"
        prefix_id = None
        try:
            created = self.stack.netbox_as(maintainer_token, "POST", "/api/ipam/prefixes/", {
                "prefix": cidr, "status": "active", "description": created_description,
            })
            self.assertEqual(created.status, 201, created.body)
            self.assertIsInstance(created.body, dict)
            prefix_id = created.body.get("id")
            self.assertIsInstance(prefix_id, int, created.body)
            self.assertEqual(created.body.get("prefix"), cidr, created.body)

            path = f"/api/ipam/prefixes/{prefix_id}/"
            changed = self.stack.netbox_as(maintainer_token, "PATCH", path, {
                "description": updated_description,
            })
            self.assertEqual(changed.status, 200, changed.body)
            self.assertEqual(changed.body.get("description"), updated_description, changed.body)
            read_back = self.stack.netbox_as(maintainer_token, "GET", path)
            self.assertEqual(read_back.status, 200, read_back.body)
            self.assertEqual(read_back.body.get("description"), updated_description, read_back.body)

            refused = self.stack.netbox_as(maintainer_token, "DELETE", path)
            self.assertEqual(refused.status, 403, refused.body)
        finally:
            if prefix_id is not None:
                path = f"/api/ipam/prefixes/{prefix_id}/"
                current = self.stack.netbox_as(self.stack.netbox_token, "GET", path)
                self.assertEqual(current.status, 200, current.body)
                self.assertEqual(current.body.get("prefix"), cidr, current.body)
                self.assertIn(current.body.get("description"),
                              (created_description, updated_description), current.body)
                removed = self.stack.netbox_as(self.stack.netbox_token, "DELETE", path)
                self.assertIn(removed.status, (200, 204), removed.body)


if __name__ == "__main__":
    unittest.main()
