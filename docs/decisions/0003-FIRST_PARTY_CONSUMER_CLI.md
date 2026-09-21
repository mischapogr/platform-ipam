# ADR 0003: A first-party consumer CLI shipped in the service binary

Status: accepted, 2026-09-08

## Context

`cmd/platform-ipam` currently accepts only `api`, `worker`, and `migrate`. Every
consumer path documented in [clients](../CLIENTS.md) — CI runners, AWS CLI/SDK
provisioners, ad-hoc operator work — is described as invoking "the provider,
CLI, or SDK", but no CLI exists. The only executable client is
`examples/python/reserve.py`, which its own header calls illustrative, which
covers reserve and operation polling only, and which the client document
explicitly says is not a replacement for a supported client.

That gap matters beyond convenience. The API's safety properties are mostly
client-side obligations: send a stable `allocation_key` rather than a
run-scoped one, send an `Idempotency-Key`, treat `202` as an unresolved
operation and poll it instead of retrying the reservation, and never read a
CIDR out of a pending response. A consumer that gets any of these wrong can
strand address space or acquire a second allocation for one logical identity.
Leaving each consumer to reimplement that in shell means the guarantees are
re-derived, incorrectly, per team.

The end-to-end suite of ADR 0002 also needs a client surface that is neither
raw `curl` nor the Terraform provider, so that the three surfaces can be shown
to agree about the same allocation. Testing `curl` would test the test harness,
not a client.

## Decision

Add a `client` subcommand to the existing `cmd/platform-ipam` binary:
`platform-ipam client <verb>`, with verbs covering `reserve`, `get`, `list`,
`capacity`, `bind`, and `release`. It ships in the release image that Compose
and Helm already build, so the end-to-end suite can execute it inside the stack
with no extra artifact, no extra module, and no separate release process.

The CLI is a transport client only. It resolves its origin from
`PLATFORM_IPAM_URL` and its bearer token from `PLATFORM_IPAM_TOKEN`, requires
HTTPS unless the origin is loopback and local HTTP is explicitly enabled, and
holds no allocation policy of its own. It does not select CIDRs, does not
choose pools, and does not infer tenant, account, or role — the API owns all of
that, exactly as it does for the Terraform provider.

It encodes the client-side obligations the API contract places on consumers.
`reserve` requires an explicit `--key` and never invents one from a timestamp,
hostname, or run number. It derives a stable `Idempotency-Key` from that
allocation key so that a retried invocation is a replay rather than a second
request. On `202` it polls the returned operation to a terminal state within a
deadline instead of re-posting the reservation, and it prints no `cidr` field
for an allocation that is not committed.

Output is JSON on stdout so it composes with `jq` and with CI. Diagnostics go
to stderr. Exit codes distinguish success, a usage or validation error, an
authorization failure, and an unresolved operation that reached its deadline,
so a pipeline can tell "this failed" from "this has not settled yet" — a
distinction that matters because the second case still owns address space.

## Consequences

Consumers get a supported client whose behaviour is versioned with the API that
defines it, and the correctness obligations above are implemented once. The
Python example stays in the repository as documentation of the raw protocol and
is explicitly not the supported path.

Shipping the CLI inside the service binary keeps distribution free but couples
the client's version to the server image. A consumer running an old image gets
an old client. This is acceptable while the API is v1 and additive; a separately
released client binary is the escape hatch if that stops being true.

Adding a client to the same binary widens what that image can do. The subcommand
performs no privileged local action and reads credentials only from the
environment, but the image is no longer purely a server, and the process
argument list is now part of the security review surface.

Because the CLI refuses to generate allocation keys, some callers that would
have shelled out with a generated identifier must now decide their key
deliberately. That friction is the point: an accidental key is the failure this
system exists to prevent.
