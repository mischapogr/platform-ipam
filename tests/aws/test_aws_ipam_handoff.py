"""AWS IPAM handoff: one stable request per reviewed move, revalidated on return.

A stateful fake of the AWS CLI stands in for AWS IPAM. Nothing here has run
against a real AWS IPAM pool.
"""

from datetime import datetime, timezone
import importlib.util
import ipaddress
import json
from pathlib import Path
import subprocess
import sys
import unittest

SCRIPTS = Path(__file__).resolve().parents[2] / "scripts/aws"
sys.path.insert(0, str(SCRIPTS))
import aws_ipam  # noqa: E402


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, SCRIPTS / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


handoff = load("aws_ipam_reserve", "aws-ipam-reserve.py")
verifier = load("verify_reservations_aws", "verify-reservations.py")

POOL = "ipam-pool-0123456789abcdef0"


class FakeAws:
    """Allocates first-fit from one pool; honours client tokens and disallowed CIDRs."""

    def __init__(self, pool_cidr="10.64.0.0/16"):
        self.pool = ipaddress.ip_network(pool_cidr)
        self.allocations = []
        self.tokens = {}
        self.calls = []
        self.failures = []
        self.hidden = 0
        self.override_cidr = None

    def __call__(self, argv, **_):
        self.calls.append(argv)
        args = dict(zip(argv[3::2], argv[4::2]))
        if argv[2] == "get-ipam-pool-allocations":
            visible = self.allocations[:len(self.allocations) - self.hidden]
            return self.ok({"IpamPoolAllocations": visible})
        if self.failures:
            return subprocess.CompletedProcess(argv, 255, "", self.failures.pop(0))
        disallowed = [ipaddress.ip_network(item) for item in
                      argv[argv.index("--disallowed-cidrs") + 1:argv.index("--output")]
                      ] if "--disallowed-cidrs" in argv else []
        key = (args["--netmask-length"], args["--description"], tuple(map(str, disallowed)))
        token = args["--client-token"]
        if token in self.tokens:
            if self.tokens[token][0] != key:
                return subprocess.CompletedProcess(argv, 254, "", "An error occurred (IdempotentParameterMismatch)")
            return self.ok({"IpamPoolAllocation": self.tokens[token][1]})
        taken = disallowed + [ipaddress.ip_network(row["Cidr"]) for row in self.allocations]
        cidr = self.override_cidr or next(
            str(item) for item in self.pool.subnets(new_prefix=int(args["--netmask-length"]))
            if not any(item.overlaps(other) for other in taken))
        allocation = {"IpamPoolAllocationId": f"ipam-pool-alloc-{len(self.allocations):04x}",
                      "Cidr": cidr, "Description": args["--description"], "ResourceType": "custom"}
        self.allocations.append(allocation)
        self.tokens[token] = (key, allocation)
        return self.ok({"IpamPoolAllocation": allocation})

    @staticmethod
    def ok(body):
        return subprocess.CompletedProcess([], 0, json.dumps(body), "")

    def writes(self):
        return [call for call in self.calls if call[2] == "allocate-ipam-pool-cidr"]


class AwsIpamHandoffTest(unittest.TestCase):
    def setUp(self):
        self.now = datetime(2026, 9, 23, 16, 1, 0, tzinfo=timezone.utc)
        self.pilot = {
            "version": 1, "discovery": {"complete": True, "finished_at": "2026-09-23T16:00:00Z",
                                         "oldest_observed_at": "2026-09-23T16:00:00Z"},
            "pilot_gates": {"planning": "PASS"},
            "replacements": {"moves": [{
                "subject": "aws:111111111111:eu-central-1:vpc-a", "approved": True,
                "authority": "aws-ipam", "pool_id": "pilot-euc1", "proposed_cidr": None,
                "proposal_status": "REQUEST_FROM_AWS_IPAM", "proposal_target_match": True,
                "target": {"tenant_id": "payments", "allocation_key": "payments-vpc-new",
                           "scope": "vpc", "environment": "development", "region": "eu-central-1",
                           "account_id": "111111111111", "prefix_length": 22},
            }]},
        }
        self.approved = {"version": 1, "pools": [{
            "id": "pilot-euc1", "cidr": "10.64.0.0/16", "authority": "aws-ipam",
            "aws_ipam_pool_id": POOL, "region": "eu-central-1", "allowed_prefix_lengths": [22],
            "eligible_accounts": ["111111111111"], "excluded_cidrs": ["10.64.252.0/22"],
        }]}
        self.protected = [{"cidr": "10.64.0.0/22"}]
        self.observed = ["10.64.4.0/22", "10.20.0.0/22"]
        self.aws = FakeAws()

    def requests(self):
        return handoff.plan_requests(self.pilot, self.approved, self.protected, self.observed, self.now, 600)

    def run_handoff(self):
        return handoff.execute(self.requests(), runner=self.aws, pause=0)

    def verify(self):
        rows = verifier.aws_ipam_allocations(self.approved, self.pilot["replacements"]["moves"], runner=self.aws)
        return verifier.verify(self.pilot, self.approved, self.protected, self.observed, None,
                               "2026-09-23T16:01:00Z", self.now, 600, aws_allocations=rows)

    def test_request_avoids_known_space_and_verifies_from_a_fresh_read(self):
        request = self.requests()[0]
        self.assertEqual(request["disallowed_cidrs"], ["10.64.0.0/22", "10.64.4.0/22", "10.64.252.0/22"])
        self.assertEqual(len(request["client_token"]), 64)
        [result] = self.run_handoff()
        self.assertEqual((result["status"], result["cidr"]), ("ALLOCATED", "10.64.8.0/22"))
        report = self.verify()
        self.assertEqual(report["authority"], "aws-ipam")
        self.assertEqual(report["allocation_gate"], "PASS")
        self.assertEqual(report["results"][0]["returned_cidr"], "10.64.8.0/22")
        self.assertEqual(report["network_readiness"], "NOT_ASSESSED")

    def test_rerun_adopts_instead_of_allocating_again(self):
        self.run_handoff()
        [again] = self.run_handoff()
        self.assertEqual(again["status"], "ADOPTED")
        self.assertEqual(len(self.aws.writes()), 1)
        self.assertEqual(len(self.aws.allocations), 1)

    def test_rerun_before_the_read_catches_up_reuses_the_client_token(self):
        self.run_handoff()
        self.aws.hidden = 1
        [again] = self.run_handoff()
        self.assertEqual((again["status"], again["cidr"]), ("ALLOCATED", "10.64.8.0/22"))
        self.assertEqual(len(self.aws.allocations), 1)

    def test_transient_failures_retry_with_the_same_token(self):
        self.aws.failures = ["An error occurred (RequestLimitExceeded)"]
        [result] = self.run_handoff()
        self.assertEqual(result["status"], "ALLOCATED")
        tokens = {call[call.index("--client-token") + 1] for call in self.aws.writes()}
        self.assertEqual(len(tokens), 1)

    def test_unanswered_write_is_unknown_never_a_guess(self):
        self.aws.failures = ["Could not connect to the endpoint URL"] * 3
        [result] = self.run_handoff()
        self.assertEqual(result["status"], "UNKNOWN_OUTCOME")
        self.assertIsNone(result["cidr"])

    def test_changed_inputs_under_one_identity_are_not_reallocated(self):
        self.run_handoff()
        self.aws.hidden = 1
        self.protected.append({"cidr": "10.64.128.0/17"})
        [result] = self.run_handoff()
        self.assertEqual(result["status"], "UNKNOWN_OUTCOME")
        self.assertIn("different parameters", " ".join(result["reasons"]))
        self.assertEqual(len(self.aws.allocations), 1)

    def test_unsafe_response_is_rejected_and_never_released_automatically(self):
        self.aws.override_cidr = "10.64.0.0/22"
        [result] = self.run_handoff()
        self.assertEqual(result["status"], "REJECTED")
        self.assertIn("release-ipam-pool-allocation", result["release_command"])
        self.assertFalse(any(call[2].startswith("release") for call in self.aws.calls))
        report = self.verify()
        self.assertEqual(report["allocation_gate"], "FAIL")
        self.assertIn("protected range", " ".join(report["results"][0]["reasons"]))

    def test_duplicate_identity_fails_verification(self):
        self.run_handoff()
        self.aws.allocations.append(dict(self.aws.allocations[0], IpamPoolAllocationId="other",
                                         Cidr="10.64.12.0/22"))
        self.assertEqual(self.verify()["allocation_gate"], "FAIL")
        [result] = self.run_handoff()
        self.assertEqual(result["status"], "REJECTED")

    def test_missing_allocation_or_unread_authority_stays_unknown(self):
        self.assertEqual(self.verify()["allocation_gate"], "UNKNOWN")
        report = verifier.verify(self.pilot, self.approved, self.protected, self.observed, [],
                                 "2026-09-23T16:01:00Z", self.now, 600)
        self.assertIn("rerun with --aws-ipam", " ".join(report["results"][0]["reasons"]))

    def test_unreviewed_or_unbound_pool_is_refused_before_any_call(self):
        del self.approved["pools"][0]["aws_ipam_pool_id"]
        with self.assertRaisesRegex(ValueError, "aws_ipam_pool_id is required"):
            self.requests()
        self.approved["pools"][0]["aws_ipam_pool_id"] = POOL
        self.pilot["replacements"]["moves"][0]["approved"] = False
        with self.assertRaisesRegex(ValueError, "approval is missing"):
            self.requests()
        self.pilot["replacements"]["moves"][0]["approved"] = True
        self.pilot["pilot_gates"]["planning"] = "UNKNOWN"
        with self.assertRaisesRegex(ValueError, "planning gate"):
            self.requests()
        self.assertEqual(self.aws.calls, [])

    def test_stale_inventory_is_refused(self):
        with self.assertRaisesRegex(ValueError, "stale"):
            handoff.plan_requests(self.pilot, self.approved, self.protected, self.observed,
                                  datetime(2026, 9, 23, 17, 0, tzinfo=timezone.utc), 600)

    def test_pagination_follows_tokens(self):
        pages = iter([{"IpamPoolAllocations": [{"Cidr": "10.64.8.0/22"}], "NextToken": "n1"},
                      {"IpamPoolAllocations": [{"Cidr": "10.64.12.0/22"}]}])
        runner = lambda argv, **_: FakeAws.ok(next(pages))  # noqa: E731
        self.assertEqual(len(aws_ipam.pool_allocations(POOL, "eu-central-1", runner)), 2)


if __name__ == "__main__":
    unittest.main()
