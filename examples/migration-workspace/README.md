# 100- and 500-account migration workspace demos

`HUNDRED_ACCOUNT_DEMO/` and `FIVE_HUNDRED_ACCOUNT_DEMO/` contain **synthetic,
noncustomer** files shaped like
one completed read-only AWS Organization inventory and its reviewed pilot
inputs. It is ready to upload at **Migration workspace → Migration pilot
overview**. No AWS, NetBox or allocation API writes are performed by these
files. The timestamps are fixed at 2026-09-24 and must not be presented as
fresh customer observations or a passed discovery gate.
The workspace offers authenticated **Download 100-account ZIP** and **Download
500-account ZIP** links. Choose **Migration pilot overview** and upload one ZIP
in **Pilot ZIP**, or drop the ZIP into the marked area. You can also extract it
and drop all files together; the page assigns names using the table below.
Each upload reruns the engines rather than trusting a precomputed preview.
The `?sample=100` view is a read-only preview of the 100-account case.

| Workspace field | Upload |
| --- | --- |
| `networks.csv` | `networks.csv` |
| `accounts.json` | `accounts.json` |
| `failures.csv` | `failures.csv` |
| `run.json` | `run.json` |
| Connectivity matrix YAML | `matrix.yaml` |
| Ownership YAML | `ownership.yaml` |
| Protected ranges YAML or JSON | `fixed.yaml` (JSON syntax) |
| Approved pool plan JSON | `approved-plan.json` |
| Pilot owner and scope JSON | `pilot-scope.json` |
| `migration.yaml` | `migration.yaml` (JSON syntax) |
| Customer Terraform execution JSON (optional) | `execution.json` (synthetic `PLANNED` references) |

The base case contains **100 accounts, three regions, 300 VPCs, 25 subnets,
five secondary VPC CIDR associations and 300/300 synthetic successful scan
cells**. Seven pairs overlap: five block reviewed `must_communicate` intent;
two are permitted only while their `must_stay_isolated` intent remains true.
The remaining 286 VPCs have no overlap observed in this synthetic snapshot;
the UI's searchable VPC inventory shows them explicitly.
The plan selects five replacement VPCs and the planner produces five advisory
CIDRs from region-specific, Platform-IPAM-owned example pools. No allocation
evidence or authority verification is included; the allocation gate remains
`UNKNOWN` and network readiness remains `NOT_ASSESSED`.

The 500-account case has **1,500 VPCs across three regions, 125 subnets,
1,500/1,500 synthetic successful scan cells, 32 observed overlaps, 25 reviewed
connectivity blockers, seven isolated overlaps and 25 reviewed replacement
moves**. Its five secondary VPC CIDRs bring the association count to 1,505.
Neither case includes verified allocation evidence. The account IDs and
observations are invented; the fixed 2026-09-24 collection time becomes stale.

To exercise uncertainty, upload the base files but substitute the three files
under `UNKNOWN_VARIANT/` for `networks.csv`, `run.json` and `failures.csv`, plus
its `matrix.yaml`. One account-region cell is denied (299/300 successful),
and one overlapping pair has no reviewed connectivity intent. The planner
withholds candidates under incomplete coverage. Keep the base approved plan,
scope and migration plan. The UI should show the known five blockers and the
explicit missing evidence without calling the pilot ready.

`generate.py` recreates these files deterministically using the repository's
`platform-ipam onboard assess` command to put real conflict IDs into the
synthetic `migration.yaml`:

```sh
python3 examples/migration-workspace/generate.py --accounts 100 /path/to/platform-ipam
python3 examples/migration-workspace/generate.py --accounts 500 /path/to/platform-ipam
python3 examples/migration-workspace/generate.py --accounts 250 --output /tmp/platform-ipam-250-demo /path/to/platform-ipam
```

For local development, `bin/platform-ipam` is the default if present. Use a
binary built from the current checkout when regenerating; the checked-in
files are already ready for browser upload.

**Uploads and previews do not save NetBox records.** VPC and subnet CIDRs are
NetBox **Prefixes**, not `IP Addresses`; the demo has no individual host IP
observations to populate `/ipam/ip-addresses/`. If persisted Prefixes are
needed for a local exercise, use the supported `onboard parse` → `onboard plan`
→ `onboard apply` workflow in [Onboarding import](../../docs/ONBOARDING_IMPORT.md)
against a dedicated demo VRF/domain. The shared `local-development` VRF is
also the allocator's live occupancy source, so importing this invented estate
there would affect allocation decisions. Do not import the demo into a
customer NetBox deployment.
