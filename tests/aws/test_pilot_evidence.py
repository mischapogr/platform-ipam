"""Safety checks for the shared pilot overview projection."""

import importlib.util
from datetime import datetime, timezone
import json
from pathlib import Path
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/aws/pilot-evidence.py"
spec = importlib.util.spec_from_file_location("pilot_evidence", SCRIPT)
pilot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pilot)


class PilotEvidenceTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.network_path = self.root / "networks.csv"
        self.network_path.write_text(
            "account_id,region,type,resource_id,cidr,primary\n"
            "111111111111,eu-central-1,vpc,vpc-a,10.20.0.0/22,true\n"
            "222222222222,eu-central-1,vpc,vpc-b,10.20.0.0/22,true\n",
            encoding="utf-8",
        )
        self.run_path = self.root / "run.json"
        self.run = {
            "started_at": "2026-09-23T12:00:00Z", "finished_at": "2026-09-23T12:01:00Z",
            "accounts": [{"account_id": account, "regions": [{"region": "eu-central-1",
                          "outcome": "succeeded", "observed_at": "2026-09-23T12:00:30Z"}]}
                         for account in ("111111111111", "222222222222")],
        }
        self.run_path.write_text(json.dumps(self.run), encoding="utf-8")
        self.networks = [{"account_id": account, "region": "eu-central-1", "type": "vpc",
                          "resource_id": vpc, "cidr": "10.20.0.0/22"}
                         for account, vpc in (("111111111111", "vpc-a"), ("222222222222", "vpc-b"))]
        self.hashes = {"networks": pilot.digest(self.network_path),
                       "matrix": "matrix-hash", "protected": "protected-hash",
                       "approved_plan": "pool-hash", "pilot_scope": "scope-hash"}
        self.address = {
            "version": 1, "pilot_scope": {"owner": "network-team",
                                        "accounts": ["111111111111", "222222222222"],
                                        "regions": ["eu-central-1"]},
            "input_sha256": self.hashes, "coverage_complete": True,
            "address_readiness": "CIDR_BLOCKED", "confirmed_conflicts": 1,
            "unresolved_intent_conflicts": 0,
            "conflicts": [{"id": "c-1"}],
            "proposals": [{"account_id": "111111111111", "region": "eu-central-1",
                           "vpc_id": "vpc-a", "status": "ADVISORY_CANDIDATE",
                           "candidate_cidr": "10.64.0.0/22", "pool_id": "pilot", "authority": "platform-ipam",
                           "reviewed_target": True, "target_account_id": "111111111111",
                           "target_region": "eu-central-1", "prefix_length": 22},
                          {"account_id": "222222222222", "region": "eu-central-1",
                           "vpc_id": "vpc-b", "status": "ADVISORY_CANDIDATE",
                           "candidate_cidr": "10.64.4.0/22", "pool_id": "pilot", "authority": "platform-ipam",
                           "reviewed_target": False, "target_account_id": "222222222222",
                           "target_region": "eu-central-1", "prefix_length": 22}],
        }
        self.progress = {
            "report_version": 1,
            "inputs": [{"path": "migration.yaml", "sha256": "reviewed-plan-hash"}],
            "assessment": {"inputs": [{"sha256": value} for value in
                                      (*[self.hashes[name] for name in ("networks", "matrix", "protected")],
                                       pilot.digest(self.run_path))],
                           "coverage": {"complete": True},
                           "conflicts": [{"id": "c-1", "impact": "confirmed", "kind": "equal-cidr",
                                          "matrix_relation": "must_communicate",
                                          "sides": [{"account_id": "111111111111", "region": "eu-central-1",
                                                     "vpc_id": "vpc-a", "cidr": "10.20.0.0/22"},
                                                    {"account_id": "222222222222", "region": "eu-central-1",
                                                     "vpc_id": "vpc-b", "cidr": "10.20.0.0/22"}]}]},
            "moves": [{"subject": "aws:111111111111:eu-central-1:vpc-a",
                       "account_id": "111111111111", "region": "eu-central-1", "vpc_id": "vpc-a",
                       "disposition": "replace", "approval": {"approved_by": "network-team", "approved_at": "2026-09-23T11:00:00Z", "approves": "VPC replacement"},
                       "wave_id": "wave-1", "depends_on": ["aws:222222222222:eu-central-1:vpc-b"],
                       "blockers": [{"id": "owner-review", "description": "Confirm workload cutover"}],
                       "rollback": "Restore the original route and DNS record.",
                       "target": {"tenant_id": "developer", "allocation_key": "vpc-a-new",
                                  "scope": "vpc", "environment": "development",
                                  "region": "eu-central-1", "account_id": "111111111111",
                                  "prefix_length": 22}, "target_fact": "active",
                       "resolves": [{"conflict_id": "c-1", "status": "present"}]},
                      {"subject": "aws:222222222222:eu-central-1:vpc-b",
                       "account_id": "222222222222", "region": "eu-central-1", "vpc_id": "vpc-b",
                       "disposition": "keep", "target_fact": "none"}],
        }

    def result(self):
        return pilot.derive(self.address, self.progress, self.run, self.networks,
                            self.network_path, self.run_path)

    def test_only_reviewed_replacement_is_counted_and_target_is_not_verified(self):
        report = self.result()
        self.assertEqual(report["discovery"]["scanned_cells"], 2)
        self.assertEqual(report["discovery"]["requested_cells"], 2)
        self.assertEqual(report["discovery"]["duration_seconds"], 60)
        self.assertEqual(report["replacements"]["planned"], 1)
        self.assertEqual(report["replacements"]["proposed_cidrs"], 1)
        self.assertEqual(report["replacements"]["target_facts_reserved_or_active"], 1)
        self.assertEqual(report["replacements"]["reservations_verified"], 0)
        self.assertEqual(report["current_address_readiness"], "CIDR_BLOCKED")
        self.assertEqual(report["pilot_gates"]["planning"], "PASS")
        self.assertEqual(report["pilot_gates"]["allocation"], "UNKNOWN")
        self.assertEqual(report["network_readiness"], "NOT_ASSESSED")
        self.assertEqual(report["relationships"][0]["selected_moves"][0]["subject"],
                         "aws:111111111111:eu-central-1:vpc-a")
        self.assertEqual(report["replacements"]["moves"][0]["allocation_key"], "vpc-a-new")
        review = report["replacements"]["moves"][0]
        self.assertEqual(review["approval"]["approved_by"], "network-team")
        self.assertEqual(review["wave_id"], "wave-1")
        self.assertEqual(review["depends_on"], ["aws:222222222222:eu-central-1:vpc-b"])
        self.assertEqual(review["blockers"][0]["id"], "owner-review")
        self.assertIn("DNS", review["rollback"])

    def test_customer_execution_reference_does_not_change_pilot_gates(self):
        report = self.result()
        gates = dict(report["pilot_gates"])
        evidence = {"version": 1, "input_sha256": report["input_sha256"],
                    "moves": [{"allocation_key": "vpc-a-new", "status": "APPLIED",
                               "commit": "abc123", "run": "customer-terraform-42"}]}
        pilot.attach_execution(report, evidence)
        self.assertEqual(report["pilot_gates"], gates)
        self.assertEqual(report["network_readiness"], "NOT_ASSESSED")
        self.assertEqual(report["replacements"]["moves"][0]["execution"]["status"], "APPLIED")
        evidence["input_sha256"] = {}
        with self.assertRaisesRegex(ValueError, "different pilot"):
            pilot.attach_execution(self.result(), evidence)

    def test_mismatched_proposal_cannot_pass_planning(self):
        self.address["proposals"][0]["prefix_length"] = 20
        report = self.result()
        self.assertEqual(report["pilot_gates"]["planning"], "UNKNOWN")
        self.assertEqual(report["replacements"]["proposed_cidrs"], 0)
        self.assertIn("PROPOSAL_TARGET_MISMATCH", [item["code"] for item in report["blockers"]])

    def test_workspace_sample_matches_derived_evidence(self):
        sample = Path(__file__).resolve().parents[2] / "deploy/compose/netbox/ui/platform_ipam_workspace/sample_pilot.json"
        self.assertEqual(json.loads(sample.read_text(encoding="utf-8")), self.result())

    def test_full_assessment_relationships_require_same_snapshot(self):
        full = {"inputs": self.progress["assessment"]["inputs"],
                "summary": {"total_relationships": 1},
                "conflicts": self.progress["assessment"]["conflicts"]}
        self.progress["assessment"]["summary"] = {"total_relationships": 1}
        report = pilot.derive(self.address, self.progress, self.run, self.networks,
                              self.network_path, self.run_path, full)
        self.assertEqual(len(report["relationships"]), 1)
        full["inputs"] = []
        with self.assertRaisesRegex(ValueError, "full assessment differs"):
            pilot.derive(self.address, self.progress, self.run, self.networks,
                         self.network_path, self.run_path, full)

    def test_denied_cell_prevents_complete_discovery(self):
        self.run["accounts"][1]["regions"][0]["outcome"] = "failed"
        self.progress["assessment"]["coverage"]["complete"] = False
        self.progress["assessment"]["coverage"]["failed"] = [
            {"account_id": "222222222222", "region": "eu-central-1",
             "stage": "describe", "error": "AccessDenied"}]
        self.run_path.write_text(json.dumps(self.run), encoding="utf-8")
        self.progress["assessment"]["inputs"][-1]["sha256"] = pilot.digest(self.run_path)
        self.address["coverage_complete"] = False
        report = self.result()
        self.assertFalse(report["discovery"]["complete"])
        self.assertEqual(report["discovery"]["scanned_cells"], 1)
        self.assertEqual(report["discovery"]["cells"][1]["reason"], "AccessDenied")
        self.assertEqual(report["pilot_gates"]["planning"], "UNKNOWN")

    def test_mixed_report_snapshots_are_refused(self):
        self.progress["assessment"]["inputs"][0]["sha256"] = "another-inventory"
        with self.assertRaisesRegex(ValueError, "different assessment inputs"):
            self.result()

    def test_unclaimed_conflict_keeps_planning_gate_unknown(self):
        self.progress["moves"][0]["resolves"] = []
        report = self.result()
        self.assertEqual(report["pilot_gates"]["planning"], "UNKNOWN")
        self.assertIn("CONFLICT_WITHOUT_SELECTED_MOVE", [item["code"] for item in report["blockers"]])

    def verification(self, report):
        return {"version": 1, "authority": "platform-ipam",
                "pilot_sha256": pilot.canonical_digest(report),
                "read_at": "2026-09-23T12:01:00Z", "valid_until": "2026-09-23T12:10:00Z",
                "inventory_oldest_observed_at": "2026-09-23T12:00:30Z",
                "max_inventory_age_seconds": 570,
                "required": 1, "verified": 1, "allocation_gate": "PASS",
                "network_readiness": "NOT_ASSESSED",
                "results": [{"subject": "aws:111111111111:eu-central-1:vpc-a",
                             "status": "VERIFIED", "allocation_id": "alloc-1",
                             "returned_cidr": "10.64.0.0/22", "reasons": []}]}

    def test_matching_authority_report_attaches_one_verified_move(self):
        report = self.result()
        attached = pilot.attach_verification(report, self.verification(report),
                                             datetime(2026, 9, 23, 12, 2, tzinfo=timezone.utc))
        self.assertEqual(attached["pilot_gates"]["allocation"], "PASS")
        self.assertEqual(attached["replacements"]["reservations_verified"], 1)
        self.assertEqual(attached["replacements"]["moves"][0]["allocation_id"], "alloc-1")
        self.assertEqual(attached["network_readiness"], "NOT_ASSESSED")

    def test_stale_or_wrong_pilot_authority_report_cannot_turn_gate_green(self):
        report = self.result()
        verification = self.verification(report)
        stale = pilot.attach_verification(report, verification,
                                          datetime(2026, 9, 23, 12, 11, tzinfo=timezone.utc))
        self.assertEqual(stale["pilot_gates"]["allocation"], "UNKNOWN")
        self.assertEqual(stale["replacements"]["reservations_verified"], 0)
        report = self.result()
        verification["pilot_sha256"] = "wrong"
        with self.assertRaisesRegex(ValueError, "different pilot"):
            pilot.attach_verification(report, verification,
                                      datetime(2026, 9, 23, 12, 2, tzinfo=timezone.utc))

    def test_aws_ipam_request_is_a_reviewed_option_closed_only_by_its_authority(self):
        proposal = self.address["proposals"][0]
        proposal.update(status="REQUEST_FROM_AWS_IPAM", candidate_cidr=None, authority="aws-ipam")
        report = self.result()
        self.assertEqual(report["pilot_gates"]["planning"], "PASS")
        self.assertEqual(report["replacements"]["aws_ipam_requests"], 1)
        self.assertEqual(report["replacements"]["proposed_cidrs"], 0)
        self.assertNotIn("CIDR_CANDIDATES_INCOMPLETE", [item["code"] for item in report["blockers"]])
        self.assertEqual(report["pilot_gates"]["allocation"], "UNKNOWN")
        verification = dict(self.verification(report), authority="aws-ipam")
        attached = pilot.attach_verification(report, verification,
                                             datetime(2026, 9, 23, 12, 2, tzinfo=timezone.utc))
        self.assertEqual(attached["pilot_gates"]["allocation"], "PASS")
        verification["authority"] = "unreviewed-tool"
        with self.assertRaisesRegex(ValueError, "unsupported authority"):
            pilot.attach_verification(self.result(), verification)

    def test_unreviewed_plan_cannot_import_a_green_verification(self):
        self.progress["moves"][0]["approval"] = None
        report = self.result()
        with self.assertRaisesRegex(ValueError, "passing planning gate"):
            pilot.attach_verification(report, self.verification(report),
                                      datetime(2026, 9, 23, 12, 2, tzinfo=timezone.utc))

    def reviewed_report(self):
        self.run["script_version"] = "2"
        self.run_path.write_text(json.dumps(self.run), encoding="utf-8")
        self.progress["assessment"]["inputs"][-1]["sha256"] = pilot.digest(self.run_path)
        return self.result()

    def review(self, report):
        return {"version": 1, "pilot_sha256": pilot.canonical_digest(report),
                "pilot_owner": "network-team", "approved_scope": report["scope"],
                "decision": "approved", "reviewed_by": "network-team-owner",
                "reviewed_at": "2026-09-23T12:02:00Z",
                "input_sha256": report["input_sha256"],
                "discovery": {"source": "live_org_inventory",
                              "run_sha256": report["input_sha256"]["run"],
                              "max_age_seconds": 600,
                              "reconciled_samples": [{"account_id": "111111111111",
                                                      "region": "eu-central-1",
                                                      "resource_id": "vpc-a"}]}}

    def test_review_derives_gates_from_exact_inputs_and_discovery(self):
        report = self.reviewed_report()
        template = pilot.review_template(report)
        self.assertEqual(template["pilot_sha256"], pilot.canonical_digest(report))
        self.assertEqual(template["reviewed_by"], "")
        self.assertEqual(template["decision"], "unreviewed")
        self.assertEqual(template["discovery"]["source"], "not_reviewed")
        review = self.review(report)
        pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["input"], "PASS")
        self.assertEqual(report["pilot_gates"]["discovery"], "PASS")
        self.assertEqual(report["input_review"]["source"], "uploaded_reviewer_attestation")
        self.assertNotIn("discovery", review.get("pilot_gates", {}))

    def test_review_never_turns_missing_or_stale_collection_green(self):
        report = self.reviewed_report()
        review = self.review(report)
        review["discovery"]["reconciled_samples"] = []
        pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertIn("SAMPLE_RECONCILIATION_MISSING", report["input_review"]["discovery_reasons"])
        report = self.reviewed_report()
        review = self.review(report)
        pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 12, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertIn("DISCOVERY_STALE_OR_UNBOUNDED", report["input_review"]["discovery_reasons"])
        report = self.reviewed_report()
        report["discovery"]["complete"] = False
        report["discovery"]["oldest_observed_at"] = None
        report["discovery"]["finished_at"] = None
        review = self.review(report)
        pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertIn("DISCOVERY_INCOMPLETE", report["input_review"]["discovery_reasons"])

    def test_review_rejects_mixed_snapshot_and_invented_sample(self):
        report = self.reviewed_report()
        review = self.review(report)
        review["input_sha256"] = {**review["input_sha256"], "networks": "different"}
        with self.assertRaisesRegex(ValueError, "current input hash"):
            pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        review = self.review(report)
        review["discovery"]["reconciled_samples"][0]["resource_id"] = "invented-vpc"
        with self.assertRaisesRegex(ValueError, "absent or repeated"):
            pilot.attach_review(report, review, datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))

    def test_review_keeps_future_observations_unknown_even_with_a_current_oldest(self):
        for future_cells in (1, 2):
            with self.subTest(future_cells=future_cells):
                for account in self.run["accounts"]:
                    account["regions"][0]["observed_at"] = "2026-09-23T12:00:30Z"
                for account in self.run["accounts"][:future_cells]:
                    account["regions"][0]["observed_at"] = "2026-09-24T12:00:30Z"
                report = self.reviewed_report()
                if future_cells == 1:
                    self.assertEqual(report["discovery"]["oldest_observed_at"], "2026-09-23T12:00:30Z")
                pilot.attach_review(report, self.review(report),
                                    datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
                self.assertEqual(report["pilot_gates"]["input"], "PASS")
                self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
                self.assertIn("OBSERVATION_AFTER_COLLECTION", report["input_review"]["discovery_reasons"])
                self.assertIn("OBSERVATION_TIME_IN_FUTURE", report["input_review"]["discovery_reasons"])

    def test_review_requires_observation_then_collection_then_review(self):
        self.run["accounts"][0]["regions"][0]["observed_at"] = "2026-09-23T12:01:30Z"
        report = self.reviewed_report()
        pilot.attach_review(report, self.review(report),
                            datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertIn("OBSERVATION_AFTER_COLLECTION", report["input_review"]["discovery_reasons"])
        self.run["accounts"][0]["regions"][0]["observed_at"] = "2026-09-23T12:00:30Z"
        report = self.reviewed_report()
        review = self.review(report)
        review["reviewed_at"] = "2026-09-23T12:00:45Z"
        pilot.attach_review(report, review,
                            datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertIn("REVIEW_PRECEDES_COLLECTION", report["input_review"]["discovery_reasons"])

    def test_review_requires_every_successful_observation_time(self):
        for value in (None, "malformed", "2026-09-23T12:00:30", 123):
            with self.subTest(observed_at=value):
                self.run["accounts"][0]["regions"][0]["observed_at"] = value
                report = self.reviewed_report()
                pilot.attach_review(report, self.review(report),
                                    datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
                self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
                self.assertIn("OBSERVATION_TIME_INVALID", report["input_review"]["discovery_reasons"])

    def test_review_requires_a_valid_completed_collection(self):
        for value, reason in ((None, "COLLECTION_TIME_INVALID"),
                              ("malformed", "COLLECTION_TIME_INVALID"),
                              ("2026-09-23T12:01:00", "COLLECTION_TIME_INVALID"),
                              (123, "COLLECTION_TIME_INVALID"),
                              ("2026-09-23T12:04:00Z", "COLLECTION_TIME_IN_FUTURE")):
            with self.subTest(finished_at=value):
                self.run["finished_at"] = value
                report = self.reviewed_report()
                pilot.attach_review(report, self.review(report),
                                    datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
                self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
                self.assertIn(reason, report["input_review"]["discovery_reasons"])

    def test_review_requires_a_valid_start_before_every_observation(self):
        for value, reason in ((None, "COLLECTION_START_TIME_INVALID"),
                              ("malformed", "COLLECTION_START_TIME_INVALID"),
                              ("2026-09-23T12:00:00", "COLLECTION_START_TIME_INVALID"),
                              (123, "COLLECTION_START_TIME_INVALID"),
                              ("2026-09-23T12:00:45Z", "OBSERVATION_BEFORE_COLLECTION"),
                              ("2026-09-23T12:02:00Z", "COLLECTION_START_AFTER_FINISH"),
                              ("2026-09-24T12:00:00Z", "COLLECTION_START_TIME_IN_FUTURE")):
            with self.subTest(started_at=value):
                self.run["started_at"] = value
                report = self.reviewed_report()
                pilot.attach_review(report, self.review(report),
                                    datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
                self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
                self.assertIn(reason, report["input_review"]["discovery_reasons"])

    def test_review_accepts_start_with_a_timezone_offset(self):
        self.run["started_at"] = "2026-09-23T14:00:00+02:00"
        report = self.reviewed_report()
        pilot.attach_review(report, self.review(report),
                            datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
        self.assertEqual(report["discovery"]["duration_seconds"], 60)
        self.assertEqual(report["pilot_gates"]["discovery"], "PASS")

    def test_review_with_an_invalid_or_future_time_keeps_both_gates_unknown(self):
        for value, reason in ((None, "REVIEW_TIME_INVALID"),
                              ("malformed", "REVIEW_TIME_INVALID"),
                              ("2026-09-23T12:02:00", "REVIEW_TIME_INVALID"),
                              (123, "REVIEW_TIME_INVALID"),
                              ("2026-09-23T12:04:00Z", "REVIEW_TIME_IN_FUTURE")):
            with self.subTest(reviewed_at=value):
                report = self.reviewed_report()
                review = self.review(report)
                review["reviewed_at"] = value
                pilot.attach_review(report, review,
                                    datetime(2026, 9, 23, 12, 3, tzinfo=timezone.utc))
                self.assertEqual(report["pilot_gates"]["input"], "UNKNOWN")
                self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
                self.assertIn(reason, report["input_review"]["discovery_reasons"])

    def test_observation_bounds_and_freshness_compare_actual_utc_times(self):
        self.run["accounts"][0]["regions"][0]["observed_at"] = "2026-09-23T14:00:15+02:00"
        self.run["accounts"][1]["regions"][0]["observed_at"] = "2026-09-23T12:00:45Z"
        report = self.reviewed_report()
        self.assertEqual(report["discovery"]["oldest_observed_at"], "2026-09-23T14:00:15+02:00")
        self.assertEqual(report["discovery"]["newest_observed_at"], "2026-09-23T12:00:45Z")
        review = self.review(report)
        review["reviewed_at"] = "2026-09-23T14:01:10+02:00"
        review["discovery"]["max_age_seconds"] = 60
        pilot.attach_review(report, review,
                            datetime(2026, 9, 23, 12, 1, 20, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "UNKNOWN")
        self.assertEqual(report["input_review"]["discovery_valid_until"], "2026-09-23T12:01:15+00:00")
        self.assertIn("DISCOVERY_STALE_OR_UNBOUNDED", report["input_review"]["discovery_reasons"])

    def test_review_allows_equal_chronological_boundaries(self):
        self.run["started_at"] = self.run["finished_at"]
        for account in self.run["accounts"]:
            account["regions"][0]["observed_at"] = self.run["finished_at"]
        report = self.reviewed_report()
        review = self.review(report)
        review["reviewed_at"] = self.run["finished_at"]
        pilot.attach_review(report, review,
                            datetime(2026, 9, 23, 12, 1, tzinfo=timezone.utc))
        self.assertEqual(report["pilot_gates"]["discovery"], "PASS")


if __name__ == "__main__":
    unittest.main()
