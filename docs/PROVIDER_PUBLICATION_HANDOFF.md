# Provider publication handoff

Updated 2026-10-12. The `0.1.0-rc.1` candidate remains an unsigned,
uncommitted-source development preview for `linux_amd64`.
**AWS API/AWS integration is not yet ready.**

## Proposed ownership

[ADR 0018](decisions/0018-PUBLIC_TERRAFORM_PROVIDER_RELEASE_ROUTE.md) is an
accepted release route. The following ownership proposal is **not approved or
verified** and does not replace the unreachable development provider address.

| Role | Proposed name or reference | Remaining input |
| --- | --- | --- |
| GitHub organization | `platform-ipam` | Confirm creation, ownership and durable control. |
| Terraform Registry namespace | `platform-ipam` | Confirm authorized namespace enrollment. |
| Provider repository | `platform-ipam/terraform-provider-platformipam` | Create under the confirmed organization. |
| Organization owner | Project founder | Supply the actual GitHub account. |
| Signing-key owner | Project founder | Supply the actual GitHub account and registered public-key fingerprint. |
| Signing-key storage | `platform-ipam/terraform-provider/signing-key` | Confirm this secret-store reference and custody; no secret values belong here. |
| Backup administrator | Second trusted GitHub account | Designate the actual account. |

Unauthenticated public API lookups on 2026-10-12 returned HTTP 404 for the
GitHub user, organization and proposed repository, and for the proposed
Registry provider. See the [lookup record](qualification/PUBLIC_NAMESPACE_2026_10_12.json).
These responses establish no public entry was returned; they do not establish
registrability, namespace control or authorization. GitHub CLI authentication
is also unavailable in this workspace. No organization, repository, signing
key or Registry enrollment has been created by this continuation.

## Portable review

The [October 10 audit](PREVIEW_CANDIDATE.md) and
[qualification record](qualification/PREVIEW_2026_10_10.json) are preserved.
The review bundle contains their digest-bound source, evidence, unsigned
assets and linked licence inventory. The two review tools are additions
bound by `BUNDLE_MANIFEST.json`; they are not part of the original execution
qualification. The bundle checksum and internal hashes check integrity; they
do not authenticate a reviewer or approve publication.

The October 10 snapshot includes local launcher work that is excluded from
the development closeout commit, and predates the latest CI changes. A fresh
clone of the pushed branch therefore cannot satisfy that historical source
manifest. Verify the frozen review bundle to check the original evidence;
GitHub CI on the pushed commit supplies separate evidence for the committed
source. The historical hashes and execution claims are not rewritten.

Build from the qualified checkout or a frozen copy whose referenced hashes
still match:

```sh
python3 scripts/release/build-preview-review-bundle.py \
  --root /path/to/qualified-snapshot \
  --out /tmp/platform-ipam-0.1.0-rc.1-review.tar.gz
```

The builder copies only explicitly digest-bound files and the review tools;
it does not scan ignored credentials or inventory. It checks integrity before
and after copying, writes a deterministic archive and refuses to overwrite
an existing archive or checksum.

Review on another machine:

The review tools require Python 3.11 or later.

The local bundle is at
`dist/terraform-provider-platformipam/0.1.0-rc.1/platform-ipam-0.1.0-rc.1-review.tar.gz`,
with a `.sha256` companion. The
[handoff evidence record](qualification/PROVIDER_REVIEW_HANDOFF_2026_10_12.json)
binds these artifacts, the public lookup and the review tools. Verification
passed 57 release/policy/preview tests, including 20 new preview integrity
tests. Two independent builds produced the same archive; extraction and
relocated verification passed, tampered source was refused, and an attempted
overwrite preserved the existing bundle.

```sh
sha256sum -c platform-ipam-0.1.0-rc.1-review.tar.gz.sha256
tar -xzf platform-ipam-0.1.0-rc.1-review.tar.gz
python3 platform-ipam-preview-review/scripts/release/check-preview-candidate.py \
  --root platform-ipam-preview-review
```

Use a trusted copy of the checker and qualification record when reviewing
untrusted input. The checker covers referenced files, not unrelated additions
to a checkout. It rejects malformed metadata, conflicting candidate identity,
digest mismatches and paths that escape their declared roots. A passing result
explicitly reports that publication acceptance is false.

## Publication sequence

1. Confirm the proposed organization, actual owner accounts and signing-key
   custody. Enroll the public key under the controlled Registry namespace.
2. Review and commit the provider source and release tooling; promote the
   reviewed commit through the project's main-branch review flow.
3. Rebuild and qualify the exact committed source. The working-tree preview
   record cannot stand in for that immutable-source qualification.
4. Export the source mirror and invoke the pinned reusable release workflow,
   following the [release runbook](TERRAFORM_PROVIDER_RELEASE.md).
5. Install the exact published provider through ordinary `terraform init`
   and retain its consumer lock file and acceptance evidence.

Keep the AWS readiness notice in the RC notes. Stable release still requires
authorized live API/AWS acceptance.
