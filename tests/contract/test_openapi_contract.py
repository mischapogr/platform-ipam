#!/usr/bin/env python3
"""Structural checks for the implemented OpenAPI contract.

These tests complement HTTP behavior tests. They prevent schema drift from
dropping documented API surface or reintroducing caller-controlled tenancy.
"""

from pathlib import Path
import json
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
SPEC = yaml.safe_load((ROOT / "api/openapi.yaml").read_text(encoding="utf-8"))


class OpenAPIContractTest(unittest.TestCase):
    def test_internal_references_resolve(self):
        """Catch dangling component references without requiring a generator."""
        def visit(value):
            if isinstance(value, dict):
                reference = value.get("$ref")
                if reference:
                    self.assertTrue(reference.startswith("#/"), reference)
                    target = SPEC
                    for part in reference.removeprefix("#/").split("/"):
                        self.assertIn(part, target, reference)
                        target = target[part]
                for child in value.values():
                    visit(child)
            elif isinstance(value, list):
                for child in value:
                    visit(child)
        visit(SPEC)

    def test_documented_paths_and_methods_are_present(self):
        expected = {
            "/v1/allocations": {"get", "post"},
            "/v1/allocations/{allocation_id}": {"get", "patch", "delete"},
            "/v1/allocations/{allocation_id}/binding": {"put"},
            "/v1/operations/{operation_id}": {"get"},
            "/v1/pools": {"get"},
            "/v1/pools/{pool_id}/capacity": {"get"},
            "/v1/findings": {"get"},
        }
        self.assertEqual(set(SPEC["paths"]), set(expected))
        for path, methods in expected.items():
            self.assertTrue(methods <= set(SPEC["paths"][path]))

    def test_async_lifecycle_and_precondition_responses_are_explicit(self):
        paths = SPEC["paths"]
        self.assertEqual(set(paths["/v1/allocations"]["post"]["responses"]),
                         {"200", "201", "202", "400", "401", "403", "409", "422", "429", "503"})
        self.assertIn("412", paths["/v1/allocations/{allocation_id}"]["patch"]["responses"])
        self.assertEqual(set(paths["/v1/allocations/{allocation_id}"]["delete"]["responses"]),
                         {"202", "204", "401", "403", "404", "409", "503"})
        self.assertEqual(set(paths["/v1/allocations/{allocation_id}/binding"]["put"]["responses"]),
                         {"200", "202", "400", "401", "403", "404", "409", "422", "429", "503"})
        operation = SPEC["components"]["schemas"]["Operation"]
        self.assertEqual(operation["discriminator"]["propertyName"], "status")
        self.assertEqual(len(operation["oneOf"]), 3)

    def test_inputs_are_strict_snake_case_without_caller_tenant_authority(self):
        schemas = SPEC["components"]["schemas"]
        for name in ("AllocationRequest", "AllocationPatch", "BindingCandidate"):
            schema = schemas[name]
            self.assertFalse(schema["additionalProperties"])
            for key in schema.get("properties", {}):
                self.assertNotRegex(key, r"[A-Z]")
        request_properties = schemas["AllocationRequest"]["properties"]
        self.assertTrue({"allocation_key", "scope", "environment", "region", "prefix_length"} <= set(request_properties))
        self.assertFalse({"tenant_id", "owner", "role", "cidr", "pool_id", "state"} & set(request_properties))
        self.assertEqual(schemas["AllocationRequest"]["properties"]["address_family"]["default"], "ipv4")

    def test_revision_idempotency_and_request_headers_are_contractual(self):
        components = SPEC["components"]
        self.assertTrue(components["parameters"]["IdempotencyKey"]["required"])
        self.assertTrue(components["parameters"]["IfMatch"]["required"])
        for response_name, response in components["responses"].items():
            if response_name == "CreatedAllocation":
                self.assertIn("ETag", response["headers"])
                self.assertIn("Location", response["headers"])
            self.assertIn("X-Request-ID", response.get("headers", {}), response_name)
        allocation = components["schemas"]["Allocation"]
        self.assertIn("revision", allocation["required"])
        self.assertIn("release_blockers", allocation["required"])

    def test_collection_pagination_and_read_models_are_concrete(self):
        schemas = SPEC["components"]["schemas"]
        for name, item_name in (("AllocationPage", "Allocation"), ("PoolPage", "Pool"), ("FindingPage", "Finding")):
            page = schemas[name]
            self.assertEqual(set(page["required"]), {"items", "next_cursor"})
            self.assertEqual(page["properties"]["items"]["items"]["$ref"], f"#/components/schemas/{item_name}")
        self.assertEqual(schemas["Capacity"]["properties"]["complete"]["type"], "boolean")
        self.assertEqual(schemas["Finding"]["properties"]["status"]["enum"], ["OPEN", "RESOLVED"])
        for field in ("allocation_id", "account_id", "region"):
            alternatives = schemas["Finding"]["properties"][field]["oneOf"]
            self.assertIn({"type": "null"}, alternatives, field)

    def test_published_reservation_fixture_and_python_client_match_the_contract(self):
        request = json.loads((ROOT / "examples/rest/allocate.json").read_text(encoding="utf-8"))
        schema = SPEC["components"]["schemas"]["AllocationRequest"]
        self.assertTrue(set(schema["required"]) <= set(request))
        self.assertTrue(set(request) <= set(schema["properties"]))
        client = (ROOT / "examples/python/reserve.py").read_text(encoding="utf-8")
        self.assertIn('"POST", "/v1/allocations"', client)
        self.assertIn('path = "/v1/operations/"', client)
        self.assertIn('"GET", path', client)
        self.assertIn('"GET", "/v1/allocations/" +', client)
        self.assertIn('"RESERVED", "ACTIVE"', client)


if __name__ == "__main__":
    unittest.main()
