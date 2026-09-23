# AWS topology inventory

Status: local collector and stubbed tests, 2026-09-23. No live AWS Organization
has been scanned. This is separate route evidence for a prior
[organization inventory](AWS_ORGANIZATION_INVENTORY.md); it does not establish
TGW reachability.

## Collect

Deploy the opt-in `PlatformIpamTopologyReadOnly` role described in
[`deploy/aws`](../deploy/aws/README.md) to the reviewed member accounts and
regions. The caller needs `sts:AssumeRole` on that role. Keep the normal
`PlatformIpamReadOnly` worker role narrow. Use an AWS CLI v2 profile with
permission to assume the topology role.

```sh
python3 scripts/aws/org-topology.py \
  --inventory /path/to/org-inventory \
  --out /path/to/topology.json
```

The input directory must contain the original `accounts.json` and `run.json`
from the organization collector. For a management account scanned directly,
add `--management-account-id 111122223333`; the script verifies the current
caller identity before reading that account. Exit 0 means every attempted
topology read returned a complete collection; exit 3 writes a report with
explicit gaps; exit 4 means no report could be written. A report with exit 3
is still useful but must never be described as complete.

The versioned `topology.json` contains source hashes, per-account and region
observations, VPC route tables, transit gateways, attachments, TGW route
tables, associations, propagations and searched routes. Failed reads are
recorded as gaps. `AdditionalRoutesAvailable=true` also makes a cell
incomplete. The collector uses AWS CLI pagination for list operations and
does not include temporary credentials in the report. Treat the report as
sensitive network architecture data.

## Interpretation boundary

The existing `onboard assess` command evaluates CIDR relationships against a
reviewed *intended connectivity* matrix. This topology file observes current
route configuration. It must be analyzed separately: a current TGW attachment
or route does not prove that traffic can pass security groups, NACLs, firewalls
or application controls. A missing attachment can describe an estate that is
still planning TGW adoption. Cross-account and shared TGW visibility must be
validated with the actual operator permissions. Compare collection timestamps
and account/region coverage before correlating the two reports. The current
CLI and UI do not yet join this file to the overlap assessment or produce a
TGW readiness verdict.

## Validation

`python3 tests/aws/test_org_topology.py` covers complete responses, denied
reads, truncated route searches and an identity mismatch with a stubbed AWS
CLI interface. No live AWS, 100-account runtime or customer route model has
been verified.
