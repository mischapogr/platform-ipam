# Provider versioning and compatibility

The provider uses [Semantic Versioning 2.0.0](https://semver.org/) independently
of Platform-IPAM. `VERSION` is the planned provider release version;
`COMPATIBILITY.json` records its API requirement and release qualification
status. Neither file creates a release. A release tag is `v` followed by
`VERSION`, and the signed build embeds that version in provider metadata.

## Compatibility matrix

| Provider candidate | Required Platform-IPAM API | Qualified Platform-IPAM product releases | Terraform CLI / target | Status |
| --- | --- | --- | --- | --- |
| `0.1.0-rc.1` | v1 (`/v1`) | None yet | `1.14.9` / `linux_amd64` | Simulation-qualified source snapshot; exact source and evidence recorded in `RELEASE_QUALIFICATION.json` |

An API major requirement is necessary, but does not prove compatibility with
every service implementing that API. This checkout has no Platform-IPAM
product release tags from which to derive a supported product version range.
OpenAPI `info.version: 1.0.0-draft` describes the API document; it is not a
Platform-IPAM product version. `SOURCE_COMMIT` identifies the exact monorepo
source exported into the public provider repository.

Terraform 1.14.9 is the simulation-qualified CLI for this candidate. Publication
requires a maintainer-reviewed attestation for the exact exported source and
all eleven checks. Other Terraform versions and targets are unqualified. The Registry manifest's
protocol 6.0 identifies the Terraform plugin protocol, not a service API or
product version.

**AWS API/AWS integration is not yet ready.**

For each release, update the matrix and JSON together, list exact tested
Platform-IPAM product versions, and bind the named maintainer's qualification
to the provider version and immutable source commit. Publish a broader product
version range only when evidence and the API compatibility policy justify it.
An empty product-version list means no product release is claimed compatible.
This matrix documents compatibility; the provider does not currently negotiate
or reject server product versions at runtime.

## Version changes

The provider's public contract includes resource and data-source names,
configuration, attributes, import identifiers, state interpretation and
allocation lifecycle behavior. A change that invalidates existing configuration
or state, changes replacement/release behavior, or removes supported service
compatibility is breaking.

| Change | Before 1.0 | From 1.0 onward |
| --- | --- | --- |
| Compatible bug fix | Patch (`0.1.0` to `0.1.1`) | Patch |
| Compatible feature | Minor (`0.1.0` to `0.2.0`) | Minor |
| Breaking public contract or support requirement | Minor, with migration notes | Major, with migration notes |

Before 1.0 the contract is experimental; pin the exact candidate while testing.
Increment RC numbers for successive published candidates. Published versions
and tags are immutable. Do not synchronize versions merely because the service
and provider release together. A service fix that preserves the required API
does not require a provider release. A new provider capability may require a
newer service release; record that minimum only after testing the pairing.

Terraform resource schema versions are separate integers used for state
upgrades. A provider SemVer bump does not migrate state automatically; incompatible
state changes need an explicit migration strategy and upgrade tests. See
[HashiCorp's state upgrade documentation](https://developer.hashicorp.com/terraform/plugin/framework/resources/state-upgrade).

## Release checks

Run `python3 scripts/check-compatibility.py` in the provider repository. The
signed release workflow additionally supplies `--expected-version`, `--release`
and the source-bound `--qualification` record. A tag, `VERSION` or matrix
mismatch stops publication; pending metadata cannot be published, even as an
RC. Exact Terraform versions, target and tested Platform-IPAM product versions
must agree with the attestation (`tested_platform_versions` lists the product
versions; an RC snapshot may use an empty list). This check validates metadata;
the separate source ancestry, named qualification, signer and installation
checks still establish release eligibility. Stable publication requires live
qualification and explicit tested Platform-IPAM product versions.
