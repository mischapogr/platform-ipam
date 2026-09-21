# ADR 0004: Development builds of the Terraform provider install through a CLI
development override

Status: accepted, 2026-09-08

## Context

The `platformipam` provider in `providers/terraform` is not published. The
examples declare `source = "registry.example.com/platform/platformipam"`, an
address that does not resolve, and [clients](../CLIENTS.md) records private
distribution — registry or approved mirror, checksums, signatures, supported
platforms, compatibility policy — as unresolved Phase 0 work. The provider is
also a separate Go module from the service, so its binary is not produced by
the release image build.

The end-to-end suite of ADR 0002 has to run `terraform plan` and
`terraform apply` against a provider built from the working tree, on a host
whose Terraform is 0.11.10 while the provider requires `>= 1.6.0`. Terraform
will not install a provider from an unreachable registry, and a locally built
binary has no registry checksum, so the normal `terraform init` path cannot be
used for a development build no matter which distribution decision is taken
later.

Two mechanisms can install an unpublished build: a filesystem mirror, which
still requires `terraform init` and a lock file entry, and a CLI configuration
`dev_overrides` block, which points a provider source address at a directory
and bypasses installation entirely.

## Decision

Development and end-to-end runs install the provider through a `dev_overrides`
block in a generated Terraform CLI configuration, pointing the
`registry.example.com/platform/platformipam` source address at a directory
holding a provider binary built from the working tree. Terraform runs in a
pinned container image, as ADR 0002 requires, with that configuration supplied
through `TF_CLI_CONFIG_FILE`.

The provider binary is built by the same pinned Go image used elsewhere, from
`providers/terraform`, into a workspace directory that is not committed. Test
runs rebuild it rather than reusing a stale artifact, because a provider binary
that silently lags the source it is supposed to be testing is worse than no
test.

This is explicitly a development mechanism and not the distribution decision.
The published path — registry or mirror address, signing, checksums, supported
OS/architecture matrix, and the version compatibility policy — remains open and
is still owned by Phase 0. Nothing here should be read as selecting it.

Because `dev_overrides` is in force, end-to-end Terraform runs do not execute
`terraform init` for this provider and do not consult or write a dependency
lock entry for it. Terraform emits a warning on every command under an
override; the harness keeps that warning visible rather than filtering it, so
that no one mistakes an override run for a normal one.

## Consequences

The provider can be exercised end to end today, against the working tree,
without publishing anything and without depending on the host's Terraform
version. This closes the gap between "provider unit tests pass" and "the
provider actually reserves a CIDR from the running service".

The mechanism deliberately does not exercise installation. Registry resolution,
checksum verification, signature validation, and lock-file behaviour are all
skipped, so an end-to-end pass says nothing about whether the provider is
installable by a consumer. Those remain untested until distribution is decided,
and the acceptance criteria for that decision must include them.

`dev_overrides` also changes provider behaviour subtly: because there is no
lock file entry and no installation step, a run cannot detect a version
mismatch between the configuration's `version` constraint and the binary in
use. End-to-end results therefore describe the working tree's provider only,
never a versioned release.

Keeping the unreachable `registry.example.com` address rather than inventing a
plausible one preserves the fact that the address is still unassigned. A
realistic-looking placeholder would risk being copied into a consumer
configuration as though it were real.
