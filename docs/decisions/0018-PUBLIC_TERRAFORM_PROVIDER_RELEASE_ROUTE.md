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
