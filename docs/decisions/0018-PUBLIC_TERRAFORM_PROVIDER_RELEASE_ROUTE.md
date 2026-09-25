# ADR 0018: Public Terraform provider release route

Status: accepted as a release route, 2026-09-23. Publication remains gated; the durable Registry namespace is not selected.

## Context

ADR 0004 intentionally uses `dev_overrides` for working-tree tests. A local filesystem-mirror rehearsal now proves normal `terraform init`, lock-file reuse, schema loading and package checksum refusal on `linux_amd64`. The current provider is a separate Go module in this monorepo, while its examples still use the unreachable `registry.example.com/platform/platformipam` source. ADR 0005 chooses a public Apache-2.0 release.

The [public Terraform Registry](https://developer.hashicorp.com/terraform/registry/providers/publishing) requires a public GitHub repository named `terraform-provider-platformipam`, release archives, a checksums file and a GPG signature. The current `platform-ipam` repository name is not a publishable provider repository. A [provider source address](https://developer.hashicorp.com/terraform/language/providers/requirements#source-addresses) is its global identity; changing it after use requires state migration.

## Decision

- Publish the first consumer provider through `registry.terraform.io/<durable-organization>/platformipam`. The organization must be selected and controlled by the maintainer before a consumer creates Terraform state. Do not replace the placeholder with a personal or temporary address by inference.
- Keep one authoritative provider source. Produce the required public `terraform-provider-platformipam` repository and release assets from the same reviewed source revision; do not maintain two hand-edited implementations. Version the provider independently of the platform API, starting at `0.1.0` with the API v1 contract.
- Start with the tested `linux_amd64` package. Add another OS/architecture only after a build, normal installation and acceptance check for that target. The package matrix is an explicit release artifact, not a claim inferred from Go cross-compilation.
- Sign the checksums as the Registry requires, publish the manifest and provider documentation, then install the published package without `dev_overrides`. Commit consumer lock files generated from the published origin. The local mirror rehearsal is a preparatory check, not release acceptance.
- Gate the first release on plan/apply/read/import/destroy and recovery tests against an authorized sandbox API and AWS account. In particular, a replacement must use a new allocation key, an ambiguous Read must retain identity, and Delete must reach durable quarantine acceptance. Keep `TF_ACC` opt-in and scoped to that sandbox.
- Use semantic versions for provider schema and behavior changes. State the minimum Terraform CLI version only after testing it; the example's current `>= 1.6.0, < 2.0.0` is not a tested support matrix merely because it parses.

## Unresolved release inputs

The organization namespace, signing-key custodian, release-repository ownership and sandbox access need named owners. No Registry publication, signed release, or real AWS acceptance has occurred. Until those inputs exist, examples retain the clearly unreachable placeholder and local development keeps ADR 0004's override.

## GitHub Actions implementation (2026-09-25)

The main repository CI now runs the provider unit/format checks and the normal
Terraform filesystem-mirror install/checksum rehearsal on GitHub-hosted
runners. The Compose validator explicitly uses the checked-in development-only
`.env.example`; it must not depend on a developer's ignored `.env` or create
credentials on a CI runner.

`.github/workflows/terraform-provider-release.yml` is a reusable workflow for
the eventual public repository named `terraform-provider-platformipam`. It
accepts a stable `vMAJOR.MINOR.PATCH` tag and a reviewed full commit SHA from
this repository's `main`, verifies that ancestry, builds the qualified
`linux_amd64` binary from that immutable source commit, creates the Registry
manifest and ZIP, checksums all published inputs, signs the checksum file with the caller's
`GPG_PRIVATE_KEY` and `GPG_PASSPHRASE` secrets, and creates a published GitHub
Release. The manifest declares Plugin Framework protocol 6.0. The workflow
does not publish from this monorepo: its repository name does not meet the
Terraform Registry's provider repository rule.

After creating the provider repository and registering its public signing key
with the Terraform Registry, its caller workflow can be (replace the workflow
reference with the reviewed commit SHA that contains the reusable workflow):

```yaml
name: Provider release
on:
  push:
    tags: ['v*.*.*']
permissions:
  contents: read
jobs:
  release:
    permissions:
      contents: write
    uses: mischapogr/platform-ipam/.github/workflows/terraform-provider-release.yml@<reviewed-commit-sha>
    with:
      source-ref: <reviewed-platform-ipam-commit-sha>
    secrets:
      GPG_PRIVATE_KEY: ${{ secrets.GPG_PRIVATE_KEY }}
      GPG_PASSPHRASE: ${{ secrets.GPG_PASSPHRASE }}
```

Pin the reusable workflow to a reviewed commit SHA. Keep the provider source
mirror in the provider repository aligned with the `source-ref` used for each
release. Configure the RSA or DSA signing key
in the Registry before the first publication; after initial registration, its
GitHub Release webhook discovers subsequent versions. The main repository
does not contain the cross-repository credential or make the namespace choice.
The called workflow builds only trusted source from this repository; it does
not execute code from the tagged provider mirror while the signing secret is
available.
