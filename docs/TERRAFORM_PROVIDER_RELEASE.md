# Terraform provider release

The provider source is `providers/terraform`. The maintainer selected the
public repository `mischapogr/terraform-provider-platformipam` and Registry
address `mischapogr/platformipam` on 2026-10-12.
[ADR 0018](decisions/0018-PUBLIC_TERRAFORM_PROVIDER_RELEASE_ROUTE.md)
defines source ownership and the first stable release gates.

## Independent versions and compatibility

The provider and Platform-IPAM product have independent release versions.
[`VERSION`](../providers/terraform/VERSION) names the planned provider version;
[`COMPATIBILITY.json`](../providers/terraform/COMPATIBILITY.json) declares its
API major and exact qualified product versions. The signed workflow rejects
a tag that differs from either file. See the
[version policy and compatibility matrix](../providers/terraform/COMPATIBILITY.md)
and [provider changelog](../providers/terraform/CHANGELOG.md).

The current RC requires API v1 but claims no qualified Platform-IPAM product
release. API-document, Terraform plugin-protocol and resource-state-schema
versions are distinct from either product's Semantic Version.

## Release channels

An RC such as `0.1.0-rc.1` may be qualified with local simulation. Its
attestation and published release notes must state:

**AWS API/AWS integration is not yet ready.**

Simulation covers provider lifecycle, recovery, normal installation and
checksum refusal. It does not establish live AWS collection, IAM, real VPC
binding, safe reclamation after AWS deletion, or stage/production readiness.
The release workflow marks RC versions as GitHub prereleases. Registry
prereleases require an explicit consumer version selection, according to
[HashiCorp's publishing requirements](https://developer.hashicorp.com/terraform/registry/providers/publishing).

A stable release requires the same checks against an authorized API/AWS
sandbox. A maintainer attestation binds declared results and evidence digests;
the validator cannot authenticate execution or independently prove AWS access.

## Prepare reviewed source

Commit and review the source, Registry docs and reusable workflow first. Use
that immutable SHA for the workflow reference, `source-ref` and source export:

```sh
python3 scripts/release/export-provider-repository.py \
  --source-ref REVIEWED_FULL_COMMIT_SHA \
  --out /tmp/terraform-provider-platformipam-source
```

The exporter accepts commits on local `main` and refuses a nonempty target.
It exports committed Git blobs, the root `LICENSE` and `NOTICE`, all four
Registry docs and the portable replacement-plan guard at
`scripts/check-replacement-plan.py`.
The mirror checker compares Git blobs at that SHA, including documentation;
working-tree edits cannot substitute different provider source, licence or
attribution. Conflicting module-level `LICENSE` or `NOTICE` files are refused.
Only release metadata in `.github/`, `.gitignore`, generated `README.md`,
`RELEASE_QUALIFICATION.json` and `release-evidence/` may differ.

For repository bootstrap before source promotion, `--preview` exports a
committed development revision with explicit unpromoted/unsigned metadata.
This exception is only for source review: the signing workflow still requires
reviewed `main` ancestry and version/source-bound release qualification.
Do not tag this bootstrap as a release.

## Qualification record

Add `RELEASE_QUALIFICATION.json` and reviewed, sanitized evidence under
`release-evidence/` to the tagged provider repository. Required common fields:

| Field | Required value |
| --- | --- |
| `schema_version` | `1` |
| `source_commit` | Exact reviewed monorepo commit SHA |
| `provider_version` | Exact release version without `v` |
| `platform` | `linux_amd64` |
| `reviewer` | Named maintainer who reviewed the evidence |
| `authorization_reference` | Local simulation authorization or approved sandbox reference |
| `tested_terraform_versions` | List of exact tested CLI patch versions |
| `checks` | Every check listed below, with evidence |

Each check has `status: "passed"`, `evidence_file: "release-evidence/..."` and
the file's lowercase SHA-256 `sha256`. Required checks are `plan`, `apply`,
`read`, `import`, `destroy`, `create_timeout_recovery`,
`ambiguous_read_retains_identity`, `replacement_uses_new_key`,
`durable_quarantine_acceptance`, `normal_install_and_checksum_refusal` and
`vulnerability_scan`.
Absent, failed, altered or unreviewed evidence does not qualify a release.

For a local RC, retain the exact unsigned release binary/package hashes and
Terraform patch version. Run the committed Terraform lifecycle assertions
against the isolated API using that binary; apply the inspected saved new-key
replacement plan and verify the old allocation's persisted quarantine and
retired-key refusal. Provider fixture tests supply deterministic timeout and
ambiguous-read cases. They must assert unchanged identity, not merely nonempty
state. Retain each command's exit status and reject skipped required tests.

The normal-install rehearsal can test the public identity without publishing:

```sh
IPAM_PROVIDER_VERSION=0.1.0-rc.1 \
IPAM_PROVIDER_SOURCE=registry.terraform.io/mischapogr/platformipam \
IPAM_PROVIDER_PACKAGE=/tmp/rc-assets/terraform-provider-platformipam_0.1.0-rc.1_linux_amd64.zip \
IPAM_PROVIDER_MIRROR_EVIDENCE=/tmp/rc-mirror-evidence \
  tests/e2e/run-provider-mirror.sh
```

The evidence directory must be absent or empty. The script retains installation,
schema, readonly-lock and tamper-refusal results and reports the exact CLI
version and original package hash. This is local mirror acceptance, not an
installation from the published Registry. Keep raw credentials, plans and
state out of public evidence; publish sanitized logs and selected assertions.

RC records additionally require `release_channel: "rc"`,
`evidence_boundary: "simulation"`, `live_aws: "NOT_READY"` and
`aws_readiness_notice: "AWS API/AWS integration is not yet ready"`.
Stable records require `release_channel: "stable"`,
`evidence_boundary: "authorized_aws_sandbox"` and `aws_mode: "live"`.

```sh
python3 scripts/release/check-provider-qualification.py \
  --mirror /tmp/terraform-provider-platformipam-source \
  --source-ref REVIEWED_FULL_COMMIT_SHA --version 0.1.0-rc.1
```

A local artifact built from uncommitted edits must disclose that boundary in
its preview report. It is not an immutable-source release qualification.

## Consumer replacement guard

Require the portable guard in consumer CI before applying every saved plan,
including plans from manual `-replace` or taint. Pin its source to the reviewed
provider release tag or commit and copy it into the consumer pipeline. A
replacement requires a known, distinct `allocation_key`; the guard rejects
replacement plans that reuse the previous key or cannot establish a new one.

```sh
set -eu
terraform plan -out=tfplan
terraform show -json tfplan > tfplan.json
python3 scripts/check-replacement-plan.py --plan-json tfplan.json
terraform apply tfplan
```

Configure the pipeline to stop on a failed guard and apply the same saved plan
that passed the check. The helper ships in the exported source repository;
the provider binary alone cannot enforce the complete saved-plan policy.

## Prepare unsigned preview assets

Build the binary with the pinned `golang:1.26.9-bookworm` image. Until a durable
namespace is selected, preserve the unreachable development source address. Then:

```sh
python3 scripts/release/package-provider.py \
  --binary /tmp/terraform-provider-platformipam_v0.1.0-rc.1 \
  --manifest providers/terraform/terraform-registry-manifest.json \
  --version 0.1.0-rc.1 --out /tmp/platformipam-rc-assets
python3 scripts/release/check-provider-assets.py \
  --directory /tmp/platformipam-rc-assets --version 0.1.0-rc.1
```

The deterministic ZIP contains one executable `linux_amd64` binary. The
manifest declares protocol 6.0, and checksums cover the ZIP and manifest. These
assets are unsigned; they cannot be ingested as a signed Registry release.
Registry doc filenames follow
[HashiCorp's required structure](https://developer.hashicorp.com/terraform/registry/providers/docs).

## Publish from the selected provider repository

Pin its caller workflow to the reviewed reusable-workflow SHA. Supply
`source-ref` and `signing-key-fingerprint`; configure `GPG_PRIVATE_KEY` and
`GPG_PASSPHRASE` secrets. The fingerprint must identify the RSA or DSA public
key registered under that namespace in Terraform Registry.

The reusable workflow verifies immutable source/mirror integrity, qualification,
formatting and unit tests before signing. It embeds the selected repository
owner's provider address, validates the package, signs checksums in a temporary
keyring, verifies the detached binary signature and creates the GitHub Release.
It never replaces assets of an existing release. The first Registry enrollment
requires the maintainer's GitHub/Registry authorization and public-key setup.
After publication, run `terraform init` from the published origin without a
development override and retain the generated consumer lock file.

Namespace ownership, signer custody, authenticated release-repository access,
Registry registration, live AWS acceptance for stable releases and published
installation evidence remain explicit release inputs.

The local unsigned RC includes `linked-licenses/INVENTORY.json`, root licence
and notice files for its 25 linked Go modules and the Go 1.26.9 licence. Review
the complete linked-package attribution requirements before distribution and
retain the notices with the released binary/source. The copied root files
and project `LICENSE`/`NOTICE` are an inventory, not a legal compliance finding.
