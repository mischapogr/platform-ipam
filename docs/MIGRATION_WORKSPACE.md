# NetBox migration workspace

Status: implemented as an optional local Compose overlay, 2026-09-24. This is
a browser view of the existing offline assessment and progress commands, not
a TGW routing simulator or a customer deployment qualification.

## Start locally

With the base development stack initialized as in [Compose](../deploy/compose/README.md):

```sh
cd deploy/compose
NETBOX_PORT=18001 docker compose --env-file .env \
  -f compose.yaml -f compose.netbox.yaml -f compose.netbox-workspace.yaml \
  up --build -d
```

Use another free `NETBOX_PORT` if 18001 is occupied. Open
`http://127.0.0.1:18001/plugins/platform-ipam/` and authenticate with the
local UI credentials in `.env`. The workspace accepts members of the
`platform-operators` group and NetBox superusers. The standalone NetBox Prefix
view remains at `/ipam/prefixes/`.

Run the plugin regression suite against the built image without resetting its
database or allocations:

```sh
cd deploy/compose
docker compose --env-file .env -f compose.yaml -f compose.netbox.yaml \
  -f compose.netbox-workspace.yaml exec netbox \
  /opt/netbox/venv/bin/python manage.py test platform_ipam_workspace

# Opt-in browser flow; requires the workspace overlay and Playwright image.
cd ../..
IPAM_E2E_BROWSER=1 python3 tests/e2e/workspace_browser_smoke.py
```

The initial page displays a clearly marked **synthetic sample pilot** so the
overview and blockers can be reviewed without first collecting AWS data. It
contains two invented VPCs, one conflict and one proposed move, with no
verified reservation. Its dated observations and gate results are illustrative,
not evidence of a live pilot. Expand **Upload pilot evidence or run another
report** to submit real files; the sample is replaced by that request's report.
For a larger walkthrough, use the authenticated 100- or 500-account ZIP links
above the form. Choose **Migration pilot overview**, then either select the ZIP
in **Pilot ZIP** or drop it into the marked area and click **Show report**.
Alternatively, extract it and drop all files into that area; the page assigns
recognized names to their fields and lists anything required but missing.
Dropping only `failures.csv` cannot produce a pilot report. The server also
validates required files and shows the missing names beside the form.
The [upload map](../examples/migration-workspace/README.md) lists every field.
After a successful overview, operators can name and save an immutable derived
pilot snapshot. Select an existing pilot for a later report with the same pilot
owner, or leave the selector at **Create a new pilot**. The **Migration pilots**
list opens the latest report and its earlier snapshots; it stores
report JSON, input hashes, creator and time, not the source upload files.
On every saved report view, the workspace compares authority evidence's
`valid_until` with the current time. An expired reservation remains in the
immutable snapshot as historical evidence and is labelled expired; it is not
presented as currently verified.
This is a local NetBox plugin migration under [ADR 0023](decisions/0023-IMMUTABLE_PILOT_SNAPSHOTS.md).
The sample banner also links to a precomputed 100-account preview, clearly
marked synthetic; uploading the files reruns the report from source inputs.
The pilot overview lists all observed CIDR relationships, lets the operator
filter by impact or search account/VPC/CIDR, and gives evidence-dependent next
steps. Its searchable VPC inventory also includes networks with no observed
overlap. A unique exact NetBox Prefix match becomes a direct object link;
the reviewed move detail shows the supplied approval, wave, dependencies,
plan constraints, rollback and customer check notes. These are claims from
`migration.yaml`, not authenticated approval or completed cutover evidence.
For a selected replacement, advisory planning uses that plan's target account,
region and prefix length; a proposal with a different target cannot pass the
planning gate. On large pilots, the discovery matrix initially shows 50
accounts and the VPC inventory 100 rows. Search covers all rows, and each
section has a **Show all** control.
missing prefixes are labelled **Not in NetBox** rather than linked to an empty
search. Multiple exact matches get a review link. A skipped or unavailable
lookup is labelled separately. The synthetic samples do not import Prefix
objects. These links are inventory navigation, not proof that the uploaded AWS
observation was imported or that a proposed allocation is committed.

## Run a report

Choose **Overlap assessment** and upload `networks.csv` from
[`org-inventory.sh`](AWS_ORGANIZATION_INVENTORY.md). Add its `accounts.json`,
`failures.csv` and `run.json` to establish coverage. Add a reviewed
`matrix.yaml` to classify impact against intended connectivity, and optional
ownership, fixed-range and decision files. The browser displays the same
conflicts, impacts, coverage status and summary the CLI returns. Missing
coverage remains visibly incomplete.

Choose **Migration progress** with the same inventory, a reviewed
`migration.yaml`, and optionally an allocation evidence JSON exported by
`platform-ipam client evidence`. The browser shows the separate subject,
target and conflict facts for each move. A missing evidence export remains
`unknown`, never a completed migration. All modes can return their exact
JSON report as a download. Inputs are limited to 16 MiB each and 64 MiB total;
assessment and progress have a 60-second deadline, and address planning has
a 180-second deadline for its embedded assessment. Uploaded bytes and results are not
stored after the request unless an operator explicitly saves the derived pilot report.

Choose **Address readiness and candidates** with the inventory,
`matrix.yaml`, protected ranges in JSON `--fixed` format, and an approved
pool plan JSON plus `pilot-scope.json` (owner, accounts and optional regions).
The browser shows `CIDR_BLOCKED`, `UNKNOWN` or `CIDR_READY`,
always alongside `NOT_ASSESSED` for network readiness, then one alternative
per VPC in confirmed conflicts. See [the pilot contract](ADDRESS_READINESS.md)
for the pool schema and why a candidate is not a reservation. The pool plan
belongs to the platform operator; the pilot customer supplies the matrix,
inventory, protected ranges and an owner/scope.

Choose **AWS topology evidence** and upload `topology.json` from the separate
[`org-topology.py`](AWS_TOPOLOGY_INVENTORY.md) collector. The browser shows
which account and region cells were read, counts of observed route tables,
TGWs and attachments, and collection gaps. It does not correlate this file
with the assessment or claim routed connectivity.

Choose **Migration pilot overview** to combine the address and progress
engines for one explicitly scoped pilot. Upload the inventory, `run.json`,
matrix, protected ranges JSON, approved pool plan, pilot scope JSON and a
reviewed `migration.yaml`; an allocation evidence export is optional. The
same read-only projection is available from
[`pilot-evidence.py`](../scripts/aws/pilot-evidence.py) for CLI/report use.
The overview shows requested and successfully scanned account/region cells,
scan duration and observation instants, observed VPC/CIDR counts, relevant
conflicts, reviewed replacement moves, advisory CIDRs and gate blockers.
Its five section links lead to scope inputs and SHA-256 hashes, a searchable
account/region coverage matrix with per-cell evidence, conflict review factors,
move lifecycle and allocation assertions. The review order uses only known
environment labels and observed subnet counts; it never chooses the VPC to
renumber or estimates workload migration cost. A move with Platform-IPAM
authority shows a command using its reviewed stable key and target fields;
the owning tenant still runs it with their own credential, then supplies a
fresh authority verification report.
Only `replace` moves count as planned migrations. The current address state
remains `CIDR_BLOCKED` while old conflicts are observed, even if a pilot
planning gate passes. `network_readiness` remains `NOT_ASSESSED`.

Without a separate authority check, the overview deliberately reports zero
**verified** reservations: `onboard progress` can establish a target
allocation fact, but its export does not prove the returned CIDR and pool.
In the move table, **Suggested CIDR, not reserved** (`ADVISORY_CANDIDATE`)
means the CIDR fits the approved pool against the uploaded inventory,
protected ranges and other suggestions. It is an advisory proposal and can
become unavailable before reservation. **Reservation check missing**
(`VERIFICATION_NOT_SUPPLIED`) means no current authority verification report
was uploaded; it does not establish whether a reservation exists.
For Platform-IPAM-owned pools, run the authenticated
[reservation verifier](RESERVATION_VERIFICATION.md) and upload its JSON as
**Authenticated reservation check JSON**. The workspace checks that the
report belongs to the recomputed pilot snapshot and shows its result per
move. It does not receive a Platform-IPAM API token or perform the authority
read itself. Input and discovery gates remain `UNKNOWN` without independent
approval and live collection validation.

For CLI use, first write an address report with `address-plan.py`, a progress
report with `platform-ipam onboard progress`, and a full `platform-ipam
onboard assess --format json` report over the same inventory, matrix,
protected ranges and optional ownership file. Then derive the identical
overview JSON:

```sh
python3 scripts/aws/pilot-evidence.py \
  --address address-report.json --progress progress-report.json \
  --assessment full-assessment.json \
  --run inventory/run.json --networks inventory/networks.csv \
  --out pilot-evidence.json
```

After an authority check, add `--verification verification.json` to derive
the same per-move result and allocation gate in the CLI overview.

The command refuses reports whose assessment input hashes differ. The scope
must name explicit regions so requested account/region coverage has a known
denominator.

Optional `execution.json` may report customer Terraform runs. It has
`version: 1`, an `input_sha256` mapping identical to the pilot overview, and a
`moves` array of stable `allocation_key`, `status` (`PLANNED`, `APPLIED` or
`FAILED`), optional `commit`, `run` and `observed_after`. Pass it to
`pilot-evidence.py --execution execution.json` or upload it in the workspace.
The report joins only known, unique keys and never treats this customer-supplied
status as authority verification or network readiness.

The workspace does not run AWS discovery, import occupancy, reserve space or
perform a migration. The network team still reviews the connectivity matrix,
replacement alternatives and target plan. The allocation API,
CLI or Terraform provider commits replacement space only when the owner
requests it. The assessment and progress reports do not read TGW route tables,
attachments or propagation. The topology report displays those observations
separately. No mode reads security controls or traffic or asserts reachability.

## Validation and remaining gates

The local 4.7.1 overlay built and started with the existing development
volumes. An authenticated GET of the workspace returned HTTP 200. A POST of
a synthetic two-account equal-CIDR inventory plus a reviewed matrix rendered
one `confirmed` relationship; a POST of a synthetic reviewed plan rendered
one move with incomplete allocation evidence. A POST of a synthetic topology
report showed one account/region cell and its missing-route-table gap. A
two-account address-planning POST returned `CIDR_BLOCKED`, `NOT_ASSESSED`,
the named pilot owner, and two distinct advisory candidates that skipped a
protected range. None of these runs contacted AWS.

The pilot overview was subsequently built in the local NetBox 4.7.1 image
and exercised through an authenticated HTTP POST with a synthetic two-account
inventory, reviewed matrix, approved pool and migration plan. It rendered
HTTP 200 with 2/2 requested cells, `CIDR_BLOCKED`, `NOT_ASSESSED` and a
pending reservation-verification state. This proves the local UI path for
that fixture only; no live AWS or AWS IPAM authority was involved.

An authenticated POST of the same synthetic pilot with an uploaded
verification-shaped artifact also rendered the claimed allocation gate,
allocation ID and returned CIDR. That fixture used a fabricated verification
artifact solely to exercise the upload and template path; the separate CLI
verifier has read the development API with an operator credential and
received CIDR/pool fields, but no customer reservation was verified here.

Before a customer pilot, run the collector against an authorized read-only
AWS Organization and hand-check representative account/region rows; measure
runtime with representative inventory sizes; qualify real UI identity and
the plugin on the chosen NetBox release; and separately collect/validate route,
TGW and traffic evidence if the product is to claim TGW readiness rather than
address conflicts against intended connectivity.
