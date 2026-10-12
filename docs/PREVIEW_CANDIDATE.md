# Preview candidate: 0.1.0-rc.1

**AWS API/AWS integration is not yet ready.** No API/AWS sandbox is available.
This candidate is for local development and simulation with fake cloud data.
Live AWS, customer pilot, real identity-provider and stage/production acceptance
remain open.

Candidate date: 2026-10-10. Target: `linux_amd64`.
**Qualified for local development simulation.**
The source is the current uncommitted working tree on
`feat/close-non-aws-work-plan`, based on `e0ac3ad`. An unsigned local package is
built; no signed GitHub release or Terraform Registry version exists.

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
| GitHub Actions and e2e shell lint | PASS locally using pinned Actionlint and ShellCheck. Remote GitHub CI is not claimed. |
| Go vulnerability scan | PASS: zero findings in both platform and provider. govulncheck 1.8.0, Go 1.26.9; vulnerability database last modified 2026-10-08T22:31:09Z. This does not scan Python or container packages. |
| Local API/CLI/Terraform/NetBox suite | PASS across combined runs: 124 unchanged cases passed in each full run, followed by both Terraform cases passing after the import-test correction, for 126 unique cases. This is not a single final full-suite pass. Import, refresh, replacement planning, the saved-plan guard and quarantine acceptance are covered. |
| Fresh NetBox workspace tests | PASS: 9 tests against the pinned NetBox 4.7.1 workspace image; ORM stand-ins exercise saved history without using the live inventory database. |
| Workspace browser flow | PASS: 1 Playwright test against the running stack covers extracted-file and ZIP drop, validation, immutable save, reload and downloaded report digest. |
| Operator browser flow | PASS: 1 Playwright test verifies the operator inventory displays the allocated prefix. |
| Ordinary API/worker restart | PASS: allocation ID, key, CIDR and NetBox prefix ID survive restart with volumes retained; allocation counts remain stable. This does not qualify disaster recovery. |
| Running image/source correspondence | PASS: 95 service/provider/workspace build inputs in the isolated snapshot match the current working tree. |
| Linked Go licence inventory | Copied root licence/notice files for all 25 modules in the RC binary build metadata, plus the Go 1.26.9 licence, into the local candidate. This inventory requires maintainer attribution review before distribution. |
| Ordinary Registry installation and registered-key signature | NOT CHECKED: no provider namespace/signing/Registry setup has been selected. Local filesystem-mirror tests remain simulation evidence. |

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

The isolated preview stack remains available locally: API at
`http://127.0.0.1:18080` and the workspace at
`http://127.0.0.1:18010/plugins/platform-ipam/`. Its generated development
credentials remain in the private snapshot's `deploy/compose/.env`.

Before external publication, select the durable Registry namespace and public
repository owner, commit/review the final source, name the evidence reviewer,
configure the registered RSA/DSA signer and publish the RC through the gated
workflow. Install that exact published origin with normal `terraform init`.
The current working-tree package cannot substitute for that immutable-source
release check.

Stable release, AWS-IPAM allocation/handoff, customer discovery/approval,
browser reservation identity, production access/retention/restore, Kubernetes
rollout and representative customer load retain the gates in
[the work plan](WORK_PLAN.md). A passing simulation does not close those gates.
