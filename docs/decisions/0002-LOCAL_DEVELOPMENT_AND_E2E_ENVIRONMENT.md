# ADR 0002: Containerized local environment and a multi-client end-to-end suite

Status: accepted, 2026-09-08

## Context

ADR 0001 fixed the service architecture but left verification to unit tests and
static checks. `go test ./...`, `scripts/ai/check-compose`, and
`scripts/ai/check-helm` all pass without ever reserving a CIDR through the
running service. The allocation behaviour that matters to consumers — that
`chooseCIDR` returns aligned, non-overlapping blocks, that `capacity` reports
the fragmentation those reservations create, and that an interrupted client
retrying the same allocation key gets the same CIDR — is currently proven only
against in-process fakes.

The developer host is also not a reliable toolchain source. In the reference
checkout there is no Go toolchain on `PATH` at all, and the installed Terraform
is 0.11.10 while `providers/terraform` requires `>= 1.6.0`. Docker and Docker
Compose are present and working. Any verification procedure that assumes a
correctly provisioned host toolchain is not reproducible here and will not be
reproducible in CI either.

Consumers reach allocations through more than one surface: the Terraform
provider, direct REST, automation clients, and the NetBox operator UI. A test
that only exercises the Go service cannot show that these surfaces agree about
the same allocation, which is the property an IPAM system actually has to hold.

Address space for these tests must be simulated. Real AWS observation is not
available locally, and `internal/cloud.Fake` already reads an explicit fixture
(`deploy/compose/fixtures/cloud.json`) that must state `domain_id`,
`generation`, and `complete`, so an omitted field cannot be mistaken for
evidence of absence.

## Decision

Docker Compose is the only supported local runtime, and every toolchain the
tests need runs in a container. Go commands run in the pinned `golang` image
used by the `Dockerfile` build stage; Terraform runs in a pinned `hashicorp/terraform`
image. Neither the host Go version nor the host Terraform version is a
prerequisite, and neither is read. The stack keeps its existing shape: platform
API, worker, migration job, platform PostgreSQL, the fake cloud observer, and an
independent NetBox release with its own database and queues.

End-to-end tests live in `tests/e2e` and drive a running stack rather than
in-process fakes. They cover four client surfaces against one service instance:

| Surface | Driver | What it proves |
| --- | --- | --- |
| REST | direct HTTP | wire contract, idempotency, error codes |
| CLI | `platform-ipam client` (ADR 0003) | first-party client behaviour and exit codes |
| Terraform | `platformipam` provider under `dev_overrides` (ADR 0004) | plan/apply/destroy lifecycle and unknown-CIDR propagation |
| Operator UI | NetBox REST assertions in the default suite; an opt-in browser smoke test | inventory an operator can actually see |

The NetBox UI is asserted through its REST API in the default suite because that
is the same data the UI renders, and it is deterministic. A browser test is kept
as a separate opt-in target so that a UI rendering regression is still
detectable, without making the routine suite depend on a browser stack.

Address-space scenarios are expressed as fixtures, not as ad-hoc test setup. A
scenario names the pool, the pre-existing occupancy written to the fake cloud
observation fixture, and the expected allocation and capacity results. Because
the fake observer requires `complete` to be explicit, a scenario can state
"incomplete observation" as a first-class case and assert that the service
refuses to treat it as free space.

The suite is destructive with respect to its own data and must run against a
disposable project name and its own volumes. It never targets a stack that
shares volumes with an operator's working environment.

## Consequences

Verification stops depending on host toolchain provisioning, so the same
commands work on a developer machine and in CI. The cost is that every Go and
Terraform invocation pays container startup and needs a warm module cache;
module and provider caches are therefore named volumes rather than throwaway
layers.

Running the real stack makes previously invisible environment defects fail
loudly. The first execution of this decision already surfaced one: the
NetBox API token pepper default in `compose.netbox.yaml` was 33 characters,
below the 50 that NetBox 4.6.7 requires, so the container exited before serving
any request while the visible symptom was a database wait timeout.

Asserting the operator surface through NetBox's REST API means the default suite
cannot catch a purely visual regression; only the opt-in browser target can, and
it will be slower and less stable. That trade is deliberate.

Because scenarios are fixtures, adding a new address-space case is a data
change. Because the fake observer refuses implicit completeness, a scenario
author cannot accidentally assert that unobserved space is free — the property
ADR 0001 depends on for safe reuse.
