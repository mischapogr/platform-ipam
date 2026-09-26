"""An allocation is verified only by a matching authoritative committed row."""

from datetime import datetime, timezone
import importlib.util
from pathlib import Path
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/aws/verify-reservations.py"
spec = importlib.util.spec_from_file_location("verify_reservations", SCRIPT)
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


class ReservationVerifierTest(unittest.TestCase):
    def setUp(self):
        self.read_at = "2026-09-23T16:01:00Z"
        self.now = datetime(2026, 9, 23, 16, 1, 1, tzinfo=timezone.utc)
        self.pilot = {
            "version": 1, "discovery": {"complete": True, "finished_at": "2026-09-23T16:00:00Z",
                                         "oldest_observed_at": "2026-09-23T16:00:00Z"},
            "pilot_gates": {"planning": "PASS"},
            "replacements": {"moves": [{
                "subject": "aws:111111111111:eu-central-1:vpc-a",
                "approved": True, "authority": "platform-ipam", "pool_id": "pilot-euc1",
                "proposed_cidr": "10.64.0.0/22",
                "target": {"tenant_id": "payments", "allocation_key": "payments-vpc-new",
                           "scope": "vpc", "environment": "development", "region": "eu-central-1",
                           "account_id": "111111111111", "prefix_length": 22},
            }]},
        }
        self.approved = {"version": 1, "pools": [{
            "id": "pilot-euc1", "cidr": "10.64.0.0/16", "authority": "platform-ipam",
            "region": "eu-central-1", "allowed_prefix_lengths": [22],
            "eligible_accounts": ["111111111111"], "excluded_cidrs": ["10.64.252.0/22"],
        }]}
        self.allocations = [{
            "id": "alloc-1", "tenant_id": "payments", "allocation_key": "payments-vpc-new",
            "scope": "vpc", "environment": "development", "region": "eu-central-1",
            "account_id": "111111111111", "prefix_length": 22, "pool_id": "pilot-euc1",
            "cidr": "10.64.0.0/22", "state": "RESERVED",
        }]

    def result(self, occupied=(), protected=()):
        return verifier.verify(self.pilot, self.approved, protected, occupied,
                               self.allocations, self.read_at, self.now, 120)

    def test_matching_authority_hold_passes_without_claiming_network_readiness(self):
        result = self.result(occupied=["10.20.0.0/22"])
        self.assertEqual(result["verified"], 1)
        self.assertEqual(result["allocation_gate"], "PASS")
        self.assertEqual(result["valid_until"], "2026-09-23T16:02:00Z")
        self.assertEqual(result["results"][0]["allocation_id"], "alloc-1")
        self.assertEqual(result["network_readiness"], "NOT_ASSESSED")

    def test_changed_cidr_needs_owner_review(self):
        self.allocations[0]["cidr"] = "10.64.4.0/22"
        result = self.result()
        self.assertEqual(result["verified"], 0)
        self.assertIn("different CIDR", " ".join(result["results"][0]["reasons"]))

    def test_protected_or_observed_overlap_blocks(self):
        self.assertEqual(self.result(protected=[{"cidr": "10.64.0.0/20"}])["verified"], 0)
        self.assertEqual(self.result(occupied=["10.64.0.0/22"])["verified"], 0)

    def test_wrong_pool_or_target_blocks(self):
        self.allocations[0]["pool_id"] = "other"
        self.allocations[0]["account_id"] = "222222222222"
        result = self.result()
        self.assertEqual(result["verified"], 0)
        self.assertEqual(result["allocation_gate"], "FAIL")
        self.assertIn("allocation pool differs", " ".join(result["results"][0]["reasons"]))

    def test_aws_ipam_pool_cannot_pass_platform_verifier(self):
        self.approved["pools"][0]["authority"] = "aws-ipam"
        self.pilot["replacements"]["moves"][0]["authority"] = "aws-ipam"
        self.assertEqual(self.result()["verified"], 0)

    def test_stale_inventory_refused_before_any_allocation_is_verified(self):
        with self.assertRaisesRegex(ValueError, "inventory is stale"):
            verifier.verify(self.pilot, self.approved, [], [], self.allocations,
                            "2026-09-23T17:00:00Z", datetime(2026, 9, 23, 17, 0, 1, tzinfo=timezone.utc), 120)

    def test_long_scan_uses_oldest_cell_for_freshness(self):
        self.pilot["discovery"]["oldest_observed_at"] = "2026-09-23T15:50:00Z"
        with self.assertRaisesRegex(ValueError, "inventory is stale"):
            self.result()

    def test_ambiguous_tenant_key_refused(self):
        self.allocations.append(dict(self.allocations[0], id="alloc-2"))
        with self.assertRaisesRegex(ValueError, "duplicate tenant/allocation keys"):
            self.result()

    def test_two_selected_moves_cannot_verify_the_same_cidr(self):
        second = dict(self.pilot["replacements"]["moves"][0])
        second["subject"] = "aws:222222222222:eu-central-1:vpc-b"
        second["target"] = dict(second["target"], allocation_key="payments-vpc-b-new")
        self.pilot["replacements"]["moves"].append(second)
        self.allocations.append(dict(self.allocations[0], id="alloc-2", allocation_key="payments-vpc-b-new"))
        result = self.result()
        self.assertEqual(result["verified"], 0)
        self.assertTrue(all(item["status"] == "FAIL" for item in result["results"]))

    def test_discovery_snapshot_is_cross_checked_before_authority_read(self):
        self.pilot["scope"] = {"accounts": ["111111111111"], "regions": ["eu-central-1"]}
        self.pilot["discovery"].update({"requested_cells": 1, "scanned_cells": 1,
                                        "vpcs": 1, "vpc_cidr_associations": 1})
        run = {"finished_at": "2026-09-23T16:00:00Z", "accounts": [
            {"account_id": "111111111111", "regions": [
                {"region": "eu-central-1", "outcome": "succeeded",
                 "observed_at": "2026-09-23T16:00:00Z"}]}]}
        networks = [{"account_id": "111111111111", "region": "eu-central-1",
                     "type": "vpc", "resource_id": "vpc-old", "cidr": "10.20.0.0/22"}]
        verifier.check_discovery_snapshot(self.pilot, run, networks)
        self.pilot["discovery"]["oldest_observed_at"] = "2026-09-23T16:00:30Z"
        with self.assertRaisesRegex(ValueError, "timing"):
            verifier.check_discovery_snapshot(self.pilot, run, networks)


if __name__ == "__main__":
    unittest.main()
