# Preview candidate: 0.1.0-rc.1

**AWS API/AWS integration is not yet ready.** No API/AWS sandbox is available.
This candidate is for local development and simulation with fake cloud data.
Live AWS, customer pilot, real identity-provider and stage/production acceptance
remain open.

Initial preview audit: 2026-10-10. Provider publication audit: 2026-10-12.
Target: `linux_amd64`. **Qualified for local development simulation.**
The published provider source is immutable monorepo commit
`d1807f4fa6676d6323d7fdddf098b8a39b9fa873`, promoted to `main`.
[Provider release `v0.1.0-rc.1`](https://github.com/mischapogr/terraform-provider-platformipam/releases/tag/v0.1.0-rc.1)
is signed and available as `mischapogr/platformipam` in the public Registry.
Maintainer `mischapogr` reviewed the eleven required simulation checks.
The frozen-source qualification passed 42 provider tests, 82 release tooling
regressions and both Terraform lifecycle cases with zero skips, including an
applied replacement with persisted quarantine and old-key refusal. The
provider repository's 15 candidate-selection tests also passed.

Fresh public Registry installation, configuration validation and schema loading
passed with Terraform 1.14.9. Terraform verified developer signing key
`136F9D1E6B3B2BE8`; independent detached-signature verification matched full
fingerprint `F1EAE2D8ED6BF46AEEE8889E136F9D1E6B3B2BE8`. The installed binary
and downloaded archive match the exact simulation-qualified hashes.
The [publication record](qualification/PROVIDER_PUBLICATION_2026_10_12.json)
retains evidence digests and the consumer lock file. Provider publication closes
REL-1's RC distribution acceptance; stable live-AWS acceptance remains open.

On 2026-10-13 the maintainer confirmed that AWS qualification is the only
remaining stable-release qualification blocker. Customer pilot and deployment
gates have their own scope. Attribution review and signing backup custody
remain separately recorded follow-ups; this scope decision supplies no new
execution or review evidence for those records.

## Scope and audit fixes

The historical plan records 88 completed package rows, including design tasks,
with 87 distinct IDs because `N4` was reused. All four earlier audit fixes are
complete. Seven broader product, qualification and release items remain open.

The Terraform provider supports owned allocations, allocation/pool reads,
metadata updates, existing-ID import and durable release acceptance. Planning
and data sources do not allocate. Replacement requires a distinct logical key;
an ambiguous Read preserves identity. Consumers must run the shipped saved-plan
guard before applying the same saved plan, including manual `-replace` and
taint: Terraform core can force replacement independently of provider checks.
Destroy requests quarantine and does not establish immediate address reuse.

Independent agent review identified and corrected these defects:

| Area | Correction |
| --- | --- |
| Provider replacement and lifecycle | Optional immutable null transitions cannot bypass replacement checks; retired allocations cannot be provisioned again under the same key. |
| Provider recovery and release | Recovery checks immutable identity, long keys receive bounded idempotency headers, and release receipts must identify a durable quarantine operation. |
| Provider metadata and reads | Known ID/CIDR survive metadata updates; malformed reads produce diagnostics; pool lookup follows pagination. |
| Pilot evidence | Parsed UTC timestamps require collection start ≤ each successful observation ≤ finish ≤ review ≤ now. Invalid or future evidence remains UNKNOWN. |
| Saved pilot history | Every snapshot digest is checked before summaries or deltas. Corrupt history is labelled, and selected corrupt reports/downloads return HTTP 409. |
| OSS release | Export and mirror checks use immutable committed Git blobs, including Registry docs, the portable plan guard, LICENSE and NOTICE. RC qualification declares simulation; stable qualification requires authorized live AWS evidence. |
| Dependencies | Active builds use Go 1.26.9. Go dependency patches address the reachable advisories found during this audit. Final scan results are recorded below. |

The source review covered tenant authorization, reserve/replay ordering,
recovery/reclamation guards, workspace uploads and saved reports, provider
lifecycle, and the release pipeline. It is a bounded code review; no claim of a
complete penetration test or container/Python vulnerability certification is
made.

## Qualification evidence

The broader platform results below retain their 2026-10-10 execution scope.
The 2026-10-12 provider release adds immutable source, eleven-check
qualification, successful remote CI and fresh public-origin acceptance; it
does not reclassify historical platform checks as a fresh full-project run.

| Check | Current result |
| --- | --- |
| Patched platform Go suite | PASS: 1,592 test/subtest passes; 14 opt-in skips. |
| Isolated PostgreSQL ledger suite | PASS: 28 test/subtest passes; the optional statement-count measurement is skipped. Concurrency, reload, rollback and differential tests run against real PostgreSQL. |
| Provider tests, format and Registry packaging | PASS: 42 test/subtest passes, including timeout preservation and same-key recovery against a client transport fixture; format and provider checks pass. The exact unsigned RC archive passes filesystem-mirror initialization, schema loading, lock reuse and tamper rejection with Terraform 1.14.9. |
| Release and portable plan-policy regression tests | PASS: 37 tests, independently rerun: 27 release/export/assets/qualification and 10 portable policy tests. The final policy also accepts valid refresh-only saved plans that omit an empty resource change list. |
| Pilot review regression tests | PASS: 23 tests, independently rerun. |
| Stubbed AWS/assessment/topology/pilot checks | PASS: `scripts/ai/check-aws`; no AWS calls. |
| AWS SDK EC2/STS protocol simulation | PASS: the opt-in Moto test exercises role assumption, VPC/subnet observation and account-mismatch refusal. Emulator results do not prove AWS fidelity or IAM acceptance. |
| Documentation/OpenAPI, Compose and Helm | PASS: repository validators. Helm rendering is static evidence. |
| GitHub Actions and e2e shell lint | Local pinned lint passed in the initial audit. Monorepo [CI on qualified source `d1807f4`](https://github.com/mischapogr/platform-ipam/actions/runs/38263723774) and provider CI/signing on release commit `a143c62` passed remotely on 2026-10-12. |
| Go vulnerability scan | PASS: zero findings in both platform and provider. govulncheck 1.8.0, Go 1.26.9; vulnerability database last modified 2026-10-08T22:31:09Z. This does not scan Python or container packages. |
| Local API/CLI/Terraform/NetBox suite | PASS across combined runs: 124 unchanged cases passed in each full run, followed by both Terraform cases passing after the import-test correction, for 126 unique cases. This is not a single final full-suite pass. Import, refresh, replacement planning, the saved-plan guard and quarantine acceptance are covered. |
| Fresh NetBox workspace tests | PASS: 9 tests against the pinned NetBox 4.7.1 workspace image; ORM stand-ins exercise saved history without using the live inventory database. |
| Workspace browser flow | PASS: 1 Playwright test against the running stack covers extracted-file and ZIP drop, validation, immutable save, reload and downloaded report digest. |
| Operator browser flow | PASS: 1 Playwright test verifies the operator inventory displays the allocated prefix. |
| Ordinary API/worker restart | PASS: allocation ID, key, CIDR and NetBox prefix ID survive restart with volumes retained; allocation counts remain stable. This does not qualify disaster recovery. |
| Running image/source correspondence | PASS: 95 service/provider/workspace build inputs matched the 2026-10-10 working tree in that isolated qualification snapshot. |
| Linked Go licence inventory | [Published binary attribution packet](qualification/PROVIDER_ATTRIBUTION_2026_10_12/README.md) retains root licence/notice files for the exact 25 linked module versions and Go 1.26.9. Versions and copied-file hashes match the public binary; complete package-specific attribution/distribution review remains pending. The current monorepo NOTICE distinguishes linked libraries from deployed integrations. |
| Ordinary Registry installation and registered-key signature | PASS on 2026-10-12: fresh public-origin init, validate and schema load; developer signature, lock-file checksums and exact installed binary verified. See the publication record. |

## Notice packaging follow-up (2026-10-13)

Future release packaging requires an attribution packet bound to the exact
binary and version. The packager retains project LICENSE/NOTICE and all packet
files, verifies dependency/version and file-digest completeness, and embeds
`THIRD_PARTY_NOTICES/CONTENTS.json`. Archive validation rejects altered notices,
unsafe paths, duplicate entries, symlinks and executable notice files.
Historical binary-only packages remain verifiable. This tooling change prepares
a future candidate; published RC assets retain their original checksums.
Complete package-specific attribution review remains open.

The [tooling rehearsal](qualification/PROVIDER_NOTICE_PACKAGING_2026_10_13.json)
passed 90 release regressions and workflow lint. Terraform 1.14.9 installed the
notice-bearing archive, retained all 34 added files byte-for-byte, loaded its
schema, reused the lock and refused a tampered package. Repackaging the published
binary in the historical format reproduced its original archive checksum.
This rehearsal uses the existing binary; a future changed release requires its
own version, source freeze and qualification.

## Reproduce and publish

Use the pinned container commands in [Contributing](../CONTRIBUTING.md),
the [end-to-end suite](../tests/e2e/README.md) and the
[provider release runbook](TERRAFORM_PROVIDER_RELEASE.md).

The unsigned RC package is under
`dist/terraform-provider-platformipam/0.1.0-rc.1/`, with source and a linked
Go licence inventory. Generated packages and raw logs are ignored. The
[qualification record](qualification/PREVIEW_2026_10_10.json) binds check
results, local evidence logs and asset hashes; its
[source manifest](qualification/PREVIEW_2026_10_10_SOURCE.json) records the
working-tree file hashes. Raw logs are retained under
`.work-plan-runs/preview-20261010/` without generated credentials or customer
inventory.

Terraform 1.14.9 can return a false change exit code for a refresh-only plan
after import. The test inspects the saved plan, verifies the applied allocation
identity, and requires an empty normal plan. The recorded limitation matches
[upstream fix #37886](https://github.com/hashicorp/terraform/pull/37886), targeted
at Terraform 1.15. The earlier failed test runs are retained with the final
targeted pass; neither failure established an infrastructure change.

The isolated preview qualification used a local API at
`http://127.0.0.1:18080` and the workspace at
`http://127.0.0.1:18010/plugins/platform-ipam/`. Its generated development
credentials remain in the private snapshot's `deploy/compose/.env`.

The 2026-10-10 unsigned working-tree package remains historical preview
evidence. The published provider was rebuilt from the reviewed committed source;
its binary SHA-256 is
`21ae57f4303686f6b945d9f89f9c3e4d3f2e864df6b52bd086baf38cc6e0d255`,
and its signed archive SHA-256 is
`5dd713570714c027b764b6364acf84300d3720a1d6f3c9fe71d0cfbb132020fa`.
The publication record and final maintainer attestation preserve the later
source and evidence boundary. Configure `required_providers` with source
`mischapogr/platformipam` and exact version `0.1.0-rc.1`, run normal
`terraform init` with Terraform 1.14.9, and retain `.terraform.lock.hcl`.

Stable release, AWS-IPAM allocation/handoff, customer discovery/approval,
browser reservation identity, production access/retention/restore, Kubernetes
rollout and representative customer load retain the gates in
[the work plan](WORK_PLAN.md). A passing simulation does not close those gates.
