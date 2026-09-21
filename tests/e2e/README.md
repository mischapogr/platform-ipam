# End-to-end suite

Drives a running Compose stack through every surface a consumer uses and
checks that they agree about the same allocation. The design and its
trade-offs are recorded in [ADR 0002](../../docs/decisions/0002-LOCAL_DEVELOPMENT_AND_E2E_ENVIRONMENT.md),
[ADR 0003](../../docs/decisions/0003-FIRST_PARTY_CONSUMER_CLI.md), and
[ADR 0004](../../docs/decisions/0004-LOCAL_TERRAFORM_PROVIDER_DISTRIBUTION.md).

```sh
./tests/e2e/run-e2e.sh
```

That builds the stack, waits for NetBox, bootstraps credentials and inventory,
resets allocation state, and runs the suite. No Go or Terraform installation is
needed on the host: both run from pinned images.

## What is covered

| File | Surface | Checks |
| --- | --- | --- |
| `test_e2e_allocation.py` | REST, CLI, NetBox inventory | alignment, containment, disjointness across sizes, idempotent replay, refused prefix lengths, capacity arithmetic, CLI/REST agreement, operator-visible markers, absence of unexplained inventory |
| `test_e2e_terraform.py` | Terraform provider | plan reserves nothing, CIDR unknown until apply, apply commits, second plan is empty, destroy quarantines |
| `test_e2e_netbox_roles.py` | NetBox REST, `platform-operators` group | the read-only operator group's `ObjectPermission` actually constrains a NetBox user (package A2) |
| `test_e2e_ui_proxy.py` | `ui-proxy`, `basic` mode | minimum proof: no credential, a forged header, a valid credential, API block (package A3, 5 tests) |
| `test_e2e_ui_auth.py` | `ui-proxy`, `basic` mode | full security matrix: wrong/unknown credential, both header spellings, forged group header, write-permission enforcement, `/api/`+`/graphql/` bypass attempts, healthcheck, NetBox port never published (package A6, 14 tests, 5 guard-removal mutations) |
| `test_e2e_import.py` | `platform-ipam onboard` | onboarding import writes NetBox occupancy the allocator treats as taken but never selects (package C7) |
| `test_e2e_cli_findings.py` | REST, CLI | `platform-ipam client findings` agrees with REST about the same list (package E1) |
| `test_e2e_second_identity.py` | REST, CLI | a second local identity (`ops-observer`, tenant `ops`, eligible for no pool) sees no allocations/findings, gets 404 (not 403) for the developer's allocation, cannot reserve, and gets a distinct CLI exit code from `findings --fail-if-open` (package G3c) |
| `test_e2e_operator_role.py` | REST, CLI | the operator identity (`ops-operator`, role `operator`, no tenant) starts from deny-everything: empty allocations/findings, `403 no_eligible_pool` at `/v1/pools`, `404` at capacity, `404` (not `403`) for the developer's allocation and operation, `403 forbidden` on reserve, `client findings --fail-if-open` exits `8`, and the start-up log warns about `ops-observer` but never about the operator (package G3b1) |
| `test_e2e_adopt.py` | `platform-ipam adopt plan\|apply\|abandon`, REST, NetBox inventory | the real `adopt` binary run inside the Compose network (never the service or the API directly), for a VPC with no subnets: plan/apply verdicts, adoption then promotion to `ACTIVE` once tagged, the owning team's first `POST` replaying, prefix-length conflict, a further apply replaying and writing nothing, the converse of `test_netbox_holds_no_managed_prefix_the_api_does_not_know`, release into `QUARANTINED` and key retirement, worker recovery of a seeded pending `ADOPT` operation, and the cheap refusals (bad prefix id, no observed resource, a second overlapping imported prefix, an unknown column, a missing `--operator`) (package F5). A second class, `AdoptVPCWithSubnetsE2ETest`, drives the two runs a VPC **with** a subnet takes (package F7): plan defers the subnet on its parent, the first `apply` adopts the VPC and reports the subnet `waiting_for_parent` with exit 3, tagging promotes the VPC to `ACTIVE`, the second `apply` replays the VPC and adopts the subnet, a third writes nothing; plus the refusals the child exemption does not cover, and an `onboard` import of a subnet created *after* its VPC was adopted. A third class, `AdoptAbandonE2ETest` (package H2c, [ADR 0012](../../docs/decisions/0012-ABANDONING_AN_ADOPTION_THAT_CANNOT_BE_FINISHED.md)), seeds a half-converted, genuinely stuck adoption (a reviewed NetBox prefix id that does not exist at the adopted CIDR, so `reviewedNetwork` itself refuses and the worker raises `adoption_stuck`) on a prefix carrying this allocation's and this operation's own markers, and drives `adopt abandon --dry-run` (writes nothing, reports the inventory claim) and a real `abandon` (clears the prefix's ownership fields while keeping its import tag/batch/source/AWS account/region, deletes the hold, resolves the finding, unfences the domain); a second abandon of the same id refuses; the freed key adopts the same network correctly under a fresh allocation id; abandoning that now-committed allocation refuses with `abandon_committed`; and a missing `--reason` exits `2` before any dependency is built |
| `test_e2e_reservation_stuck.py` | REST, CLI, NetBox inventory, worker recovery | `ReservationStuckE2ETest` (package H4): a pending `RESERVE` wedged by an out-of-band NetBox prefix at its own CIDR fences its overlap domain (`503 domain_busy`) and raises `reservation_stuck`; repairing the ground truth (removing the prefix) lets the next worker pass commit the hold and resolve the finding on its own. `ReservationCancelE2ETest` (package H8d, [ADR 0013](../../docs/decisions/0013-A_RESERVATION_THAT_CAN_NEVER_COMPLETE.md)): a second, independent stuck reservation refuses `DELETE /v1/operations/{id}` / `client cancel` with `409 reservation_not_stuck` before the finding opens and for another tenant or the operator (`404`, byte-identical to an unknown id); once `reservation_stuck` is `OPEN`, the owning tenant's cancel (through the CLI) deletes the hold and resolves the finding while leaving the out-of-band prefix that caused the refusal untouched; a repeat cancel converges (`already_fenced`, `deleted` true, `removed` false); the freed allocation key reserves again under a new allocation id; and a second, separate scenario manufactures ADR 0013's "fifth route" -- a prefix `Ensure` already created, carrying the allocation's own markers, stuck instead by a foreign untagged cloud resource added to the fake-cloud fixture at the same CIDR -- and shows the cancel deletes exactly that prefix |
| `browser_smoke.py` | NetBox web UI | opt-in; an allocation is findable and renders with its key and state, authenticated through `ui-proxy`'s HTTP Basic |

Assertions are invariants over what a run itself reserved, not fixed
addresses, so the suite is meaningful against a stack that already holds
allocations.

## The reset

Allocation keys are permanent and a release leaves a deliberate quarantine
hold, so a suite that only appended would exhaust the pool quota after a few
runs. `run-e2e.sh` therefore truncates the platform ledger and deletes the
NetBox prefixes carrying platform markers, while keeping NetBox's own database
— its migrations take minutes. The pool container prefix has no marker and is
preserved.

The reset is destructive and refuses to run unless `IPAM_ENVIRONMENT` is
`development` and the Compose project name looks like a development or test
project.

The reset never deletes an unmanaged (no `platform_allocation_id`, no
`platform-ipam-imported` tag) NetBox prefix, because that is exactly what a real
out-of-band resource looks like. `ReservationStuckE2ETest` and
`ReservationCancelE2ETest` each create one at a seeded stuck reservation's CIDR
to make the obstacle real. The first removes it as its own "ground truth repair"
step. The second asserts that a cancel leaves it exactly where it was (ADR 0013)
and therefore cannot remove it mid-test; it removes it in a class cleanup that
runs last, after the tests and after the safety net that cancels any hold still
pending. Both must, because the lent slots in `10.64.128.0/18` rotate with the
run id: an unmanaged prefix left behind would sit in some later run's adopt slot
on a long-lived development stack, and nothing but a person would ever clear it.

## Running parts of it

```sh
# One file, against an already-running stack.
python3 -m unittest discover -s tests/e2e -p 'test_e2e_terraform.py' -v

# The opt-in browser smoke test; not part of the default suite.
IPAM_E2E_BROWSER=1 python3 tests/e2e/browser_smoke.py

# Keep the built provider and generated Terraform config for inspection.
IPAM_E2E_KEEP_WORKSPACE=1 ./tests/e2e/run-e2e.sh
```

`IPAM_E2E_RUN_ID` prefixes every allocation key the run creates; `run-e2e.sh`
sets it to a UTC timestamp.

## Splitting a full run across two invocations

A full run needs most of the ten minutes an agent's foreground shell command
is allowed (image build, NetBox wait, reset, then the tests themselves), so
`run-e2e.sh` takes two opt-in switches that let one verification be done in
two calls instead of being made slower. Run with neither, as CI does, the
script behaves exactly as it always has.

| Variable | Effect |
| --- | --- |
| `IPAM_E2E_SKIP_PREPARE=1` | Skip the build, the NetBox health wait, the bootstrap and the destructive reset; run only the tests against the stack an earlier call already prepared. |
| `IPAM_E2E_PATTERN=<glob>` | The `unittest discover` pattern, default `test_e2e_*.py`. Extra arguments are still passed through to `unittest discover`. |

```sh
# First call: prepare the stack and run the first half.
export IPAM_E2E_RUN_ID=$(date -u +%Y%m%d%H%M%S)
IPAM_E2E_PATTERN='test_e2e_[a-i]*.py' ./tests/e2e/run-e2e.sh

# Second call: same run id, no preparation, the rest of the modules.
IPAM_E2E_SKIP_PREPARE=1 IPAM_E2E_PATTERN='test_e2e_[j-z]*.py' ./tests/e2e/run-e2e.sh
```

Two rules make this safe. Export the **same** `IPAM_E2E_RUN_ID` for both
halves: it prefixes every allocation key and seeds each module's
address-slot rotation, so halves under different run ids can pick the same
CIDRs. And make the two patterns disjoint, so no module runs twice — the
second call does not reset the ledger, allocation keys are permanent, and
the development pool allows 32 unreclaimed allocations per tenant.

## `entra` and `ldap` mode: opt-in, isolated runners

`ui_mode_entra.py` and `ui_mode_ldap.py` (package A4/A5, [GUI
authentication](../../docs/GUI_AUTHENTICATION.md)) are named the way
`browser_smoke.py` already is, deliberately outside this suite's
`test_e2e_*.py` discovery glob: each needs its own throw-away Compose
project and a container attached to that project's network, neither of
which exists when `run-e2e.sh` runs the default suite against the shared
`platform-ipam-dev` project in `basic` mode. Run them instead through their
own scripts:

```sh
./tests/e2e/run-ui-entra.sh   # entra mode, against a mock OIDC issuer
./tests/e2e/run-ui-ldap.sh    # ldap mode, against an OpenLDAP test directory
```

Both are **phased** (`init`/`up`, a repeatable bounded health-wait,
`bootstrap`, `test`, `down`, or `all`/no argument for every phase in one
call) rather than one long blocking script: a from-scratch NetBox's
first-run migration commonly takes 5-15 minutes, longer than some
environments allow a single foreground command to block for, so each health
check returns after its own bounded budget and is simply re-invoked until it
reports healthy. Both tear their project down, pass or fail. Neither has
been run against a real Microsoft Entra ID tenant or a real Active
Directory -- see [GUI authentication](../../docs/GUI_AUTHENTICATION.md) for
what that still needs.

## Transports

The harness talks to the API over the published loopback port when that is
reachable, and otherwise issues the same requests from inside the stack's
network. Tests do not know which is in use, so the suite works with a remote or
sandboxed Docker daemon.

Terraform runs inside the API container's network namespace. That is what makes
the provider's loopback-HTTP exemption apply — `127.0.0.1:8080` there is the
platform API — so no bearer token crosses a network in plaintext.

## Skips

The suite skips, rather than fails, when the stack is not running or a required
image is unavailable. A skip means "not covered here", not "passed": in
`run-e2e.sh` the stack is started first, so a skip there points at the harness
or the environment, not at the service.
