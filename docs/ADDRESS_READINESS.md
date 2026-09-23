# Pilot address readiness and replacement options

Status: local offline implementation, 2026-09-23. The command and NetBox
workspace have run against synthetic inventory only. They have not been
validated in a customer AWS Organization.

## The four customer artifacts

The first pilot needs a reviewed [connectivity matrix](OVERLAP_ASSESSMENT.md)
(`must_communicate`, `must_stay_isolated`, and unresolved relationships), a
read-only organization inventory with explicit account/region coverage, a
protected-range list, and a named pilot owner with a 5–10 account scope. The
platform operator also supplies an **approved pool plan** from its existing
address policy; that is platform configuration, not a fifth discovery request
to the customer.

The protected list is a JSON array in the same `--fixed` format assessment
already accepts. Include on-premises, CATO/VPN, FortiGate, Azure, partner and
other reserved CIDRs that replacement addresses must avoid:

```json
[{"cidr":"10.64.0.0/16","description":"corporate VPN","owner":"network-team"}]
```

The pilot owner records the reviewed account scope in `pilot-scope.json`.
The inventory may cover more accounts: all observed space remains blocked
for candidate selection, while only conflicts touching the pilot accounts
receive replacement options.

```json
{"version":1,"owner":"network-team","accounts":["111111111111","222222222222"],"regions":["eu-central-1"]}
```

The approved plan names a reviewed pool, region, allowed sizes, optional
eligible accounts and exclusions, and exactly one authority. Pools may not
overlap, even if they name different authorities:

```json
{
  "version": 1,
  "pools": [{
    "id": "pilot-euc1",
    "cidr": "10.64.0.0/14",
    "authority": "platform-ipam",
    "region": "eu-central-1",
    "allowed_prefix_lengths": [16, 20, 22],
    "eligible_accounts": ["111111111111", "222222222222"],
    "excluded_cidrs": ["10.67.0.0/16"]
  }]
}
```

Use `"authority":"aws-ipam"` for a pool owned by AWS IPAM. The planner
then identifies the pool and required prefix size but does **not** choose a
local CIDR. Provisioning must request one from the authoritative AWS IPAM
pool. Do not run Platform-IPAM's allocator over the same pool.
This local version trusts the operator-approved pool file; it does not read
AWS IPAM pool inventory, available capacity or allocations. Verify the pool
against AWS IPAM before a customer pilot uses that authority mode.

## Run

```sh
python3 scripts/aws/address-plan.py \
  --inventory /path/to/org-inventory \
  --matrix /path/to/matrix.yaml \
  --protected /path/to/protected.json \
  --approved-plan /path/to/approved-plan.json \
  --pilot-scope /path/to/pilot-scope.json \
  --migration-plan /path/to/migration.yaml \
  --binary /path/to/platform-ipam \
  --out /path/to/address-report.json
```

The command runs `platform-ipam onboard assess` over those exact inventory,
matrix and protected bytes, then reads the original `networks.csv`. Exit 0
means `CIDR_READY` for the reviewed intent; exit 3 means a report exists but
address blockers or unknowns remain; exit 4 means no trustworthy report could
be produced. Repeated runs over unchanged inputs produce the same JSON. The
report contains input hashes and preserves the underlying confirmed conflicts.

`CIDR_BLOCKED` means at least one observed overlap blocks a `must_communicate`
relationship touching the pilot scope. `UNKNOWN` means coverage is incomplete, input evidence is
limited, or an overlapping pair's connectivity intent is unresolved.
`CIDR_READY` means no observed `must_communicate` overlap remains in a
completely covered scope with resolved intent. An overlap explicitly marked
`must_stay_isolated` does not block that address state; the routing and
security controls that enforce isolation are separate evidence.

`network_readiness` is always `NOT_ASSESSED`. No TGW route, firewall, DNS or
traffic result can be inferred from address readiness.

For every VPC in a confirmed conflict, the report offers an **alternative**
replacement option only for a pilot-scoped VPC. It does not choose which VPC should move. Without a reviewed plan, the size comes
from exactly one observed primary VPC CIDR; if that is absent or ambiguous,
the option needs owner review. A platform-owned pool receives a deterministic
first-fit candidate that avoids every observed VPC/subnet CIDR, every
protected range, pool exclusions and earlier candidates in the same report.
When `--migration-plan migration.yaml` is supplied, a selected replacement
uses its reviewed target account, region and prefix length. Selected moves
claim advisory candidate space before unselected alternatives. Without a
reviewed target, the alternative uses the observed primary VPC size. The
pilot overview requires a target-matching proposal before its planning gate
can pass; a /16 old VPC may therefore have a reviewed /22 replacement.
Incomplete coverage withholds candidates. A candidate is free only relative
to those supplied snapshots: it is not a reservation, does not include
unobserved allocations or ledger holds, and may differ from the CIDR a later
authorized reservation returns. The owner must review and reserve through
the sole authority before provisioning.

The same report is available in the optional
[NetBox workspace](MIGRATION_WORKSPACE.md) at `/plugins/platform-ipam/` under
**Address readiness and candidates**. This view processes uploads for one
request and stores neither a plan nor a reservation.

## Pilot acceptance

The four explicit pass/fail gates and required evidence are in
[First customer pilot gates](PILOT_GATES.md). In particular, an offline
candidate does not pass the allocation gate. A completed reservation from the
sole authority, followed by validation of its returned CIDR, is required.

For the selected 5–10 accounts, the inventory names every in-scope account
and region or explicitly lists why it was unreadable; every observed
`must_communicate` CIDR conflict is stable across reruns; protected external
ranges and pool exclusions appear in the candidate check; no proposed CIDR
collides with a known input; and the owner confirms which alternative to
execute. A customer pilot still needs a live read-only collection and
representative hand checks. Application migration and working TGW traffic
are outside this address-readiness result.
