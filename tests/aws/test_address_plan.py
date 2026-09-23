"""Pilot address planning keeps protected ranges and allocation authority explicit."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/aws/address-plan.py"
spec = importlib.util.spec_from_file_location("address_plan", SCRIPT)
planner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(planner)


class AddressPlanTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.inventory = self.root / "networks.csv"
        self.inventory.write_text(
            "account_id,region,type,resource_id,cidr,primary\n"
            "111111111111,eu-central-1,vpc,vpc-a,10.20.0.0/22,true\n"
            "222222222222,eu-central-1,vpc,vpc-b,10.20.0.0/22,true\n",
            encoding="utf-8",
        )
        self.matrix = self.root / "matrix.yaml"
        self.matrix.write_text("version: 1\n", encoding="utf-8")
        self.protected = self.root / "protected.json"
        self.protected.write_text('[{"cidr":"10.64.0.0/22","owner":"vpn"}]\n', encoding="utf-8")
        self.approved = self.root / "approved.json"
        self.scope = self.root / "pilot-scope.json"
        self.scope.write_text(json.dumps({"version": 1, "owner": "network-team",
                                          "accounts": ["111111111111", "222222222222"],
                                          "regions": ["eu-central-1"]}), encoding="utf-8")
        (self.root / "accounts.json").write_text(json.dumps({"Accounts": [
            {"Id": "111111111111", "Status": "ACTIVE"},
            {"Id": "222222222222", "Status": "ACTIVE"}
        ]}), encoding="utf-8")
        self.write_pools("platform-ipam")

    def write_pools(self, authority):
        self.approved.write_text(json.dumps({"version": 1, "pools": [
            {"id": "pilot", "cidr": "10.64.0.0/16", "authority": authority,
             "region": "eu-central-1", "allowed_prefix_lengths": [22]}
        ]}), encoding="utf-8")

    def assessment(self, impact="confirmed", coverage=True, relation="must_communicate"):
        return {
            "report_version": 1,
            "inputs": [{"sha256": planner.digest(path)} for path in
                       (self.inventory, self.matrix, self.protected)],
            "coverage": {"complete": coverage},
            "input_limits": [],
            "conflicts": [{"id": "c-1", "impact": impact, "matrix_relation": relation,
                           "sides": [{"account_id": "111111111111", "region": "eu-central-1",
                                      "vpc_id": "vpc-a", "fixed": False},
                                     {"account_id": "222222222222", "region": "eu-central-1",
                                      "vpc_id": "vpc-b", "fixed": False}]}],
        }

    def run_plan(self, assessment):
        return planner.plan(assessment, self.inventory, self.matrix, self.protected,
                            self.approved, self.scope)

    def test_proposals_skip_protected_and_remain_distinct(self):
        first = self.run_plan(self.assessment())
        second = self.run_plan(self.assessment())
        self.assertEqual(first, second)
        self.assertEqual(first["address_readiness"], "CIDR_BLOCKED")
        self.assertEqual(first["network_readiness"], "NOT_ASSESSED")
        self.assertEqual([item["candidate_cidr"] for item in first["proposals"]],
                         ["10.64.4.0/22", "10.64.8.0/22"])

    def test_aws_ipam_pool_never_gets_local_cidr(self):
        self.write_pools("aws-ipam")
        report = self.run_plan(self.assessment())
        self.assertTrue(all(item["status"] == "REQUEST_FROM_AWS_IPAM" and
                            item["candidate_cidr"] is None for item in report["proposals"]))

    def test_reviewed_replacement_uses_target_size_before_unselected_alternative(self):
        self.approved.write_text(json.dumps({"version": 1, "pools": [
            {"id": "pilot", "cidr": "10.64.0.0/16", "authority": "platform-ipam",
             "region": "eu-central-1", "allowed_prefix_lengths": [20, 22]}
        ]}), encoding="utf-8")
        target = {("111111111111", "eu-central-1", "vpc-a"):
                  {"account_id": "111111111111", "region": "eu-central-1", "prefix_length": 20}}
        report = planner.plan(self.assessment(), self.inventory, self.matrix, self.protected,
                              self.approved, self.scope, targets=target)
        selected, alternative = report["proposals"]
        self.assertEqual(selected["candidate_cidr"], "10.64.16.0/20")
        self.assertTrue(selected["reviewed_target"])
        self.assertEqual(alternative["candidate_cidr"], "10.64.4.0/22")
        self.assertFalse(alternative["reviewed_target"])

    def test_incomplete_coverage_keeps_known_blocker_but_withholds_candidates(self):
        report = self.run_plan(self.assessment(coverage=False))
        self.assertEqual(report["address_readiness"], "CIDR_BLOCKED")
        self.assertTrue(all(item["status"] == "INSUFFICIENT_COVERAGE" and
                            item["candidate_cidr"] is None for item in report["proposals"]))

    def test_pool_exclusion_is_skipped(self):
        self.approved.write_text(json.dumps({"version": 1, "pools": [
            {"id": "pilot", "cidr": "10.64.0.0/16", "authority": "platform-ipam",
             "region": "eu-central-1", "allowed_prefix_lengths": [22],
             "excluded_cidrs": ["10.64.4.0/22"]}
        ]}), encoding="utf-8")
        report = self.run_plan(self.assessment())
        self.assertEqual(report["proposals"][0]["candidate_cidr"], "10.64.8.0/22")

    def test_unknown_coverage_and_intent_never_become_ready(self):
        report = self.run_plan(self.assessment(impact="unknown", coverage=False,
                                               relation="not_in_matrix"))
        self.assertEqual(report["address_readiness"], "UNKNOWN")
        self.assertEqual(report["proposals"], [])
        isolated = self.run_plan(self.assessment(impact="potential", relation="must_stay_isolated"))
        self.assertEqual(isolated["address_readiness"], "CIDR_READY")

    def test_overlapping_pools_are_refused_even_with_different_authorities(self):
        self.approved.write_text(json.dumps({"version": 1, "pools": [
            {"id": "a", "cidr": "10.64.0.0/16", "authority": "aws-ipam",
             "region": "eu-central-1", "allowed_prefix_lengths": [22]},
            {"id": "b", "cidr": "10.64.0.0/17", "authority": "platform-ipam",
             "region": "eu-central-1", "allowed_prefix_lengths": [22]}
        ]}), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "one authority per range"):
            self.run_plan(self.assessment())

    def test_changed_protected_input_is_refused(self):
        old_assessment = self.assessment()
        self.protected.write_text("[]\n", encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "do not match"):
            self.run_plan(old_assessment)

    def test_pilot_scope_limits_options_without_hiding_outside_occupancy(self):
        self.scope.write_text(json.dumps({"version": 1, "owner": "network-team",
                                          "accounts": ["111111111111"]}), encoding="utf-8")
        report = self.run_plan(self.assessment())
        self.assertEqual([item["vpc_id"] for item in report["proposals"]], ["vpc-a"])
        self.assertEqual(report["proposals"][0]["candidate_cidr"], "10.64.4.0/22")


if __name__ == "__main__":
    unittest.main()
