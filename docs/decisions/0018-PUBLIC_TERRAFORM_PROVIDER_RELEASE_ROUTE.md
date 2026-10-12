# ADR 0018: Public Terraform provider release route

Status: accepted as a release route, 2026-09-23. The maintainer explicitly selected the existing `mischapogr` account on 2026-10-12; publication remains gated.

## Context

ADR 0004 intentionally uses `dev_overrides` for working-tree tests. A local filesystem-mirror rehearsal now proves normal `terraform init`, lock-file reuse, schema loading and package checksum refusal on `linux_amd64`. The current provider is a separate Go module in this monorepo, while its examples still use the unreachable `registry.example.com/platform/platformipam` source. ADR 0005 chooses a public Apache-2.0 release.

The [public Terraform Registry](https://developer.hashicorp.com/terraform/registry/providers/publishing) requires a public GitHub repository named `terraform-provider-platformipam`, release archives, a checksums file and a GPG signature. The current `platform-ipam` repository name is not a publishable provider repository. A [provider source address](https://developer.hashicorp.com/terraform/language/providers/requirements#source-addresses) is its global identity; changing it after use requires state migration.

## Decision

- Publish the first consumer provider through `registry.terraform.io/mischapogr/platformipam`. The maintainer explicitly selected this existing account as the durable namespace on 2026-10-12, superseding the earlier organization-only proposal. Namespace control and Registry enrollment must be confirmed before consumer state is created; the account choice is not inferred from the monorepo owner.
- Keep one authoritative provider source. Produce the required public `terraform-provider-platformipam` repository and release assets from the same reviewed source revision; do not maintain two hand-edited implementations. Version the provider independently of the platform API, starting at `0.1.0` with the API v1 contract.
- Start with the tested `linux_amd64` package. Add another OS/architecture only after a build, normal installation and acceptance check for that target. The package matrix is an explicit release artifact, not a claim inferred from Go cross-compilation.
- Sign the checksums as the Registry requires, publish the manifest and provider documentation, then install the published package without `dev_overrides`. Commit consumer lock files generated from the published origin. The local mirror rehearsal is a preparatory check, not release acceptance.
- Gate the first stable release on plan/apply/read/import/destroy and recovery tests against an authorized sandbox API and AWS account. An explicitly selected `-rc.N` prerelease may use simulation with the AWS readiness notice and evidence boundary described below. In both channels, a replacement must use a new allocation key, an ambiguous Read must retain identity, and Delete must reach durable quarantine acceptance. Keep `TF_ACC` opt-in and scoped to that sandbox.
- Use semantic versions for provider schema and behavior changes. State the minimum Terraform CLI version only after testing it; the example's current `>= 1.6.0, < 2.0.0` is not a tested support matrix merely because it parses.
- Keep `providers/terraform/VERSION` and `COMPATIBILITY.json` in the authoritative source and export them with the provider. The release tag must match both. Record API-major requirements separately from exact qualified Platform-IPAM product releases; an API version alone does not prove product compatibility. See [the compatibility policy](../../providers/terraform/COMPATIBILITY.md). The first RC has no qualified product release pairing; stable publication requires explicit product versions and live qualification.

## Unresolved release inputs

The namespace and provider repository are selected as `mischapogr` and
`mischapogr/terraform-provider-platformipam`. Repository creation, authenticated
Registry enrollment, signing-key custody/registration and source promotion to
reviewed `main` remain open. No signed release or real AWS acceptance has
occurred. Local development keeps ADR 0004's override.

The supplied public RSA-4096 key has primary fingerprint
`F1EAE2D8ED6BF46AEEE8889E136F9D1E6B3B2BE8`. Parsing this public key establishes
format compatibility only; it does not establish registration, possession of
the private key, backup custody or release authorization.

## GitHub Actions implementation (2026-09-25)

The main repository CI now runs the provider unit/format checks and the normal
Terraform filesystem-mirror install/checksum rehearsal on GitHub-hosted
runners. The Compose validator explicitly uses the checked-in development-only
`.env.example`; it must not depend on a developer's ignored `.env` or create
credentials on a CI runner.

`.github/workflows/terraform-provider-release.yml` is a reusable workflow for
the eventual public repository named `terraform-provider-platformipam`. It
accepts a stable `vMAJOR.MINOR.PATCH` or `vMAJOR.MINOR.PATCH-rc.N` tag and a reviewed full commit SHA from
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
      signing-key-fingerprint: <registered-rsa-or-dsa-primary-fingerprint>
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

## Deterministic source mirror preparation (2026-09-29)

`scripts/release/export-provider-repository.py` exports the provider module
and root licence from one reviewed full commit SHA on local `main` into an
absent or empty directory. It adds a `SOURCE_COMMIT` marker and generated
README, refuses a nonempty destination, and reads committed Git objects rather
than the current working tree. A focused local test compares exported bytes
with `git show` for that exact commit. To prepare a repository candidate:

```sh
SOURCE_REF=$(git rev-parse main)
python3 scripts/release/export-provider-repository.py \
  --source-ref "$SOURCE_REF" --out /tmp/terraform-provider-platformipam-source
```

Review the exported tree and use the caller workflow above in the separate
provider repository. This command does not create that GitHub repository,
choose its namespace, sign a release or publish anything. The release workflow
checks that the caller's tagged mirror has the same `SOURCE_COMMIT`, licence
and provider source bytes before signing or publication, then builds from the
reviewed monorepo SHA. The mirror must be refreshed from that same SHA before
its tag is pushed. Provider `docs/` are immutable mirrored source and must
match the committed bytes. The comparison permits separate release metadata
in `.github/`, `README.md`, `.gitignore`, `RELEASE_QUALIFICATION.json` and
`release-evidence/`.

## Simulation RC qualification (2026-10-10)

The maintainer selected a simulation RC because no API/AWS sandbox is
currently available. RC attestations and release notes must state:
**AWS API/AWS integration is not yet ready.** This permits development preview
qualification; the stable release gate still requires authorized live AWS
acceptance.

The [release runbook](../TERRAFORM_PROVIDER_RELEASE.md) specifies the exact
digest-bound qualification record and evidence files. The reusable workflow
checks the committed source/mirror, required lifecycle/install/security
evidence, formatting and unit tests before packaging. It embeds the selected
provider-repository namespace, uses Go 1.26.9, validates Registry assets, and
verifies the registered RSA/DSA primary fingerprint in an isolated temporary
keyring. An RC is published as a GitHub prerelease; consumers must explicitly
select its version. A local unsigned package remains separate from that
signed, immutable-source release.

## Repository bootstrap (2026-10-12)

An explicit `--preview` source export may bootstrap the public repository from
an immutable development commit before promotion to `main`. It labels the
source as a preview in generated metadata and README. The default exporter
still requires `main` ancestry, and the signing workflow always checks that
ancestry and reviewed release qualification. Repository bootstrap creates no
version tag or Registry release and does not approve source promotion.
