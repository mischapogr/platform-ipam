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
| `test_e2e_netbox_roles.py` | NetBox REST, operator and inventory maintainer groups | the viewer can read but cannot create; the maintainer can create and change a prefix but cannot delete it (package A2; maintainer case requires `NETBOX_E2E_MAINTAINER_TOKEN`) |
| `test_e2e_ui_proxy.py` | `ui-proxy`, `basic` mode | minimum proof: no credential, a forged header, a valid credential, API block (package A3, 5 tests), plus package T2's quota canary as the module's sixth test |
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

## Quota budget

`pool_dev_euc1`'s `max_unreclaimed_allocations_per_tenant`
(`deploy/compose/fixtures/pools.yaml`; `domain.Pool.MaxAllocations` in
`internal/domain/config.go`) is **32**, enforced per tenant per pool by
`countTenant` (`internal/service/service.go`): every allocation whose state is
not `RELEASED` counts, regardless of scope (`vpc` or `subnet`) and regardless
of prefix length. A `release()` only quarantines a `RESERVED`/`ACTIVE`
allocation — `countTenant` still counts a `QUARANTINED` row, because the
suite never runs the worker's absence-scan reclaim path (`RequiredAbsenceScans`,
real wall-clock spacing) that would eventually free it. Only two things ever
return a slot outright: `adopt abandon` (its delete removes the allocation row
— `internal/service/abandon.go`, `delete(st.Allocations, a.ID)`) when the
freed key is never re-adopted, and `client cancel` (`internal/service/cancel.go`)
of a genuinely stuck reservation.

The table below is a per-module accounting of every allocation the suite
creates in `pool_dev_euc1` and never truly reclaims (i.e. every slot still
counted by `countTenant` once the module's own tests are done), read from the
current test source, in the order `run-e2e.sh`'s two-half split actually runs
them (files sorted alphabetically within each half; `unittest`'s
`TestLoader` also sorts classes and test methods within a file
alphabetically by name, not by definition order).

### First half (`test_e2e_[a-i]*.py`)

| Module | What spends a slot | Net slots held at suite end | Releases? |
| --- | --- | --- | --- |
| `test_e2e_adopt.py` | `AdoptAbandonE2ETest`: seeds a stuck adoption, abandons it (row deleted, slot returned), then adopts the same network again under the same key — the class's own `_release_everything` cleanup releases that re-adoption too | 1 | Quarantined — still counts (H2c's own precedent later reused by H8d: the abandon-then-re-adopt round trip is a net +1 either way, released or not) |
| `test_e2e_adopt.py` | `AdoptE2ETest`: adopts one VPC, tags it to `ACTIVE`, releases it in `test_08`; `test_09` separately SQL-seeds a second, pending `ADOPT` (`recovery_cidr`/`alloc_id`) that the worker commits, also released in the class's `_release_everything` cleanup | 2 | Both quarantined — still count |
| `test_e2e_adopt.py` | `AdoptVPCWithSubnetsE2ETest`: adopts one VPC and its one subnet; both are registered in the class's `_release_everything` cleanup | 2 | Both quarantined — still count |
| `test_e2e_allocation.py` | Seven tests reserving eleven distinct keys, none released: `test_reserved_cidr_is_aligned_and_inside_its_pool` (1), `test_distinct_keys_receive_disjoint_ranges` (4), `test_mixed_sizes_never_overlap` (2 — one `/22`, one `/20`, the suite's only non-`/22` `vpc` allocation), `test_replaying_one_allocation_key_returns_one_allocation` (1, replay of the same key), `test_capacity_accounts_for_each_new_reservation` (1), `test_cli_and_rest_agree_about_one_allocation` (1, reserved once through the CLI then replayed through REST), `test_reservation_appears_in_netbox_with_its_platform_markers` (1) | 11 | No |
| `test_e2e_cli_findings.py` | Reads findings and allocations; reserves nothing | 0 | n/a |
| `test_e2e_import.py` | Three tests reserve real allocations beside imported occupancy to prove they never overlap it: `test_01_imported_network_is_never_allocated` (3×`/22` + 2×`/20`), `test_07_row_overlapping_a_live_allocation_is_refused` (1×`/22`), `test_11_range_inside_pool_is_avoided_a_range_spanning_the_pool_is_refused` (3×`/22`). None released. (Every ADR 0016 test, `test_13`–`test_23`, including M9b4's removal demonstration, writes only NetBox occupancy — never a ledger allocation — so it spends nothing here.) | 9 | No |

**First-half subtotal: 25 of 32.**

### Second half (`test_e2e_[j-z]*.py`)

| Module | What spends a slot | Net slots held at suite end | Releases? |
| --- | --- | --- | --- |
| `test_e2e_netbox_roles.py` | No allocations | 0 | n/a |
| `test_e2e_operator_gate.py` | No allocations (reads only) | 0 | n/a |
| `test_e2e_operator_role.py` | One class-wide allocation, released in `addClassCleanup` | 1 | Quarantined — still counts |
| `test_e2e_reservation_stuck.py` | `ReservationStuckE2ETest` (H4): one seeded stuck `RESERVE`, quarantined by the module's own cleanup rather than freed | 1 | Quarantined — still counts |
| `test_e2e_reservation_stuck.py` | `ReservationCancelE2ETest` (H8d): two seeded stuck holds, both cancelled outright (slots returned in full — a cancel deletes the row, unlike release); `test_05`'s fresh re-reservation under the freed key is left committed rather than released | 1 | The net +1 is the re-reservation; the two cancels cost nothing lasting |
| `test_e2e_second_identity.py` | One class-wide allocation, released in `addClassCleanup` | 1 | Quarantined — still counts |
| `test_e2e_terraform.py` | `test_plan_reserves_nothing_and_apply_is_stable` (one allocation, never destroyed) + `test_destroy_releases_the_allocation_without_freeing_it_immediately` (one allocation, destroyed) | 2 | One committed, one quarantined — both still count |
| `test_e2e_ui_auth.py` | No allocations | 0 | n/a |
| `test_e2e_ui_proxy.py` | No allocations of its own | 0 | n/a |

**Second-half subtotal: 6 of 32.**

**Suite total from the current source: 31 of 32** (25 + 6), which matches
`docs/WORK_PLAN.md`'s H8d and M9b4 review notes ("31 of 32 slots after a full
run") exactly. `test_e2e_reservation_stuck.py`'s own module docstring instead
puts the running total at "about 32 of 32" after `ReservationCancelE2ETest`;
that number is explicitly hedged ("about") and was never checked by a runtime
assertion against a real ledger, so it is left as written rather than edited
to agree — this table's own count is one below it. Whichever of the two is
exactly right at any given moment, both leave at most one free slot, which is
exactly the situation this package's assertion (below) exists to catch: it
reads the true count from the running stack rather than trusting any static
count on this page, so a future package that adds or removes an allocation is
told at once if the real number drifts, instead of the next module simply
failing with a mysterious `quota_exceeded`.

The lent, rotating slots in `10.64.128.0/18` (`test_e2e_adopt.py`'s
`_QUARTER_BASE_OCTET`/`_SLOT_COUNT`, indices `SLOT_RESERVATION_STUCK` /
`SLOT_RESERVATION_STUCK_PROBE` lent to `test_e2e_reservation_stuck.py`, and
`SLOT_REMOVAL` lent to `test_e2e_import.py`) are address-space slot
*indices*, not allocation-quota slots: borrowing one only fixes which CIDR a
module's own reservation/adoption lands on, so it is already counted above
under whichever module actually reserves or adopts that CIDR.

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

For the exact NetBox 4.6.10 candidate, prefix each phased runner invocation
with `IPAM_NETBOX_CANDIDATE=1`. This uses separate `platform-ipam-a4-4610`
and `platform-ipam-a5-4610` Compose projects and ports 18104/18105. For
example, run the Entra phases `init`, `up`, `wait-netbox`, `bootstrap`,
`wait-issuer`, `test`, `stop`; run the LDAP phases `up`, `wait`, `bootstrap`,
`test`, `stop`. The `stop` phase retains volumes for inspection. The ordinary
runner defaults and its `down` cleanup remain unchanged.

The additional local simulation gates are opt-in and use separate projects:

```sh
./tests/e2e/run-ui-samba-ad.sh up
./tests/e2e/run-ui-samba-ad.sh wait
./tests/e2e/run-ui-samba-ad.sh bootstrap
./tests/e2e/run-ui-samba-ad.sh test
./tests/e2e/run-ui-samba-ad.sh stop

./tests/e2e/run-aws-moto.sh up
./tests/e2e/run-aws-moto.sh test
./tests/e2e/run-aws-moto.sh stop
```

These `stop` phases retain volumes. The Samba AD test requires a privileged
container; its directory port is not published. Moto proves SDK EC2/STS
request compatibility, not real AWS policy or inventory authority. See
[local simulation](../../docs/LOCAL_SIMULATION.md).

For the separate provider installation path, run
`sh tests/e2e/run-provider-mirror.sh`. It packages a temporary local mirror
artifact, runs normal Terraform initialization, checks the lock file and
provider schema, then verifies rejection of a changed package. It needs
Docker, `zip`, `rg` and Python 3 on the host, and does not publish a provider.

To probe the next NetBox 4.6 patch image without changing the default pin, run
`sh tests/e2e/run-netbox-compat.sh up`, then repeat `wait` until it succeeds,
followed by `bootstrap`, `test` and `stop`. A fresh image's migrations can outlast
one health window. The runner uses a separate Compose project and host ports,
generates a fresh allocation run ID for each test invocation, and retains its
volumes on stop. It targets `v4.6.10-5.0.2` only.

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
