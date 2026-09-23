"""Coverage and route evidence checks for the separate topology collector."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[2] / "scripts/aws/org-topology.py"
spec = importlib.util.spec_from_file_location("org_topology", SCRIPT)
topology = importlib.util.module_from_spec(spec)
spec.loader.exec_module(topology)


def inventory(root):
    (root / "accounts.json").write_text(json.dumps({
        "Accounts": [{"Id": "111111111111", "Status": "ACTIVE", "Name": "Example"}]
    }))
    (root / "run.json").write_text(json.dumps({
        "accounts": [{"account_id": "111111111111", "regions": [
            {"region": "eu-central-1", "outcome": "succeeded", "row_count": 1}
        ]}]
    }))


def aws_response(env, *args):
    operation = args[1]
    if operation == "assume-role":
        return {"Credentials": {"AccessKeyId": "test-access-key",
                                "SecretAccessKey": "test-secret", "SessionToken": "test-session"}}
    if operation == "get-caller-identity":
        return {"Account": "111111111111"}
    if operation == "describe-route-tables":
        return {"RouteTables": [{"RouteTableId": "rtb-1", "VpcId": "vpc-1",
                                 "Routes": [{"DestinationCidrBlock": "10.0.0.0/8",
                                             "TransitGatewayId": "tgw-1", "State": "active"}]}]}
    if operation == "describe-transit-gateways":
        return {"TransitGateways": [{"TransitGatewayId": "tgw-1"}]}
    if operation == "describe-transit-gateway-attachments":
        return {"TransitGatewayAttachments": [{"TransitGatewayAttachmentId": "tgw-attach-1",
                                                  "ResourceId": "vpc-1"}]}
    if operation == "describe-transit-gateway-vpc-attachments":
        return {"TransitGatewayVpcAttachments": [{"TransitGatewayAttachmentId": "tgw-attach-1",
                                                     "VpcId": "vpc-1"}]}
    if operation == "describe-transit-gateway-route-tables":
        return {"TransitGatewayRouteTables": [{"TransitGatewayRouteTableId": "tgw-rtb-1",
                                                "TransitGatewayId": "tgw-1"}]}
    if operation == "get-transit-gateway-route-table-associations":
        return {"Associations": [{"TransitGatewayAttachmentId": "tgw-attach-1"}]}
    if operation == "get-transit-gateway-route-table-propagations":
        return {"TransitGatewayRouteTablePropagations": [{"TransitGatewayAttachmentId": "tgw-attach-1"}]}
    if operation == "search-transit-gateway-routes":
        return {"Routes": [{"DestinationCidrBlock": "10.0.0.0/8"}],
                "AdditionalRoutesAvailable": False}
    raise AssertionError(f"unexpected AWS operation {operation}")


class TopologyTest(unittest.TestCase):
    def test_complete_evidence_retains_routes_and_excludes_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory(root)
            with patch.object(topology, "aws_json", side_effect=aws_response):
                report = topology.collect(root, "PlatformIpamTopologyReadOnly", "")
        self.assertTrue(report["complete"])
        self.assertEqual(len(report["cells"]), 1)
        cell = report["cells"][0]
        self.assertEqual(cell["vpc_route_tables"][0]["Routes"][0]["TransitGatewayId"], "tgw-1")
        self.assertEqual(cell["tgw_route_tables"][0]["routes"][0]["DestinationCidrBlock"], "10.0.0.0/8")
        self.assertNotIn("test-secret", json.dumps(report))
        self.assertNotIn("test-session", json.dumps(report))

    def test_denied_stage_and_truncated_routes_are_incomplete(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory(root)

            def partial(env, *args):
                if args[1] == "describe-route-tables":
                    raise RuntimeError("denied")
                response = aws_response(env, *args)
                if args[1] == "search-transit-gateway-routes":
                    response["AdditionalRoutesAvailable"] = True
                return response

            with patch.object(topology, "aws_json", side_effect=partial):
                report = topology.collect(root, "PlatformIpamTopologyReadOnly", "")
        self.assertFalse(report["complete"])
        reasons = {(gap["stage"], gap["reason"]) for gap in report["cells"][0]["gaps"]}
        self.assertIn(("describe-route-tables", "read-unavailable"), reasons)
        self.assertIn(("search-transit-gateway-routes", "additional-routes-available"), reasons)

    def test_wrong_assumed_account_never_scans(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory(root)

            def wrong_identity(env, *args):
                if args[1] == "get-caller-identity":
                    return {"Account": "222222222222"}
                return aws_response(env, *args)

            with patch.object(topology, "aws_json", side_effect=wrong_identity):
                report = topology.collect(root, "PlatformIpamTopologyReadOnly", "")
        self.assertFalse(report["complete"])
        self.assertEqual(report["cells"], [])
        self.assertEqual(report["gaps"][0]["reason"], "credentials-unavailable")

    def test_management_account_identity_is_checked_before_scan(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory(root)

            def wrong_identity(env, *args):
                if args[1] == "get-caller-identity":
                    return {"Account": "222222222222"}
                raise AssertionError("a wrong account must not be scanned")

            with patch.object(topology, "aws_json", side_effect=wrong_identity):
                report = topology.collect(root, "PlatformIpamTopologyReadOnly", "111111111111")
        self.assertFalse(report["complete"])
        self.assertEqual(report["cells"], [])
        self.assertEqual(report["gaps"][0]["reason"], "credentials-unavailable")


if __name__ == "__main__":
    unittest.main()
