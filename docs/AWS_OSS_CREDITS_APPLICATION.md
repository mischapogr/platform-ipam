# AWS Cloud Credits for Open Source application draft

Prepared 2026-09-26. Copy the answers into the [AWS application form](https://aws.amazon.com/blogs/opensource/aws-cloud-credits-for-open-source-projects-affirming-our-commitment/) and email the completed form to `awsopensourcecredits@amazon.com`. The linked spreadsheet returned HTTP 403 during preparation, so its exact fields could not be checked. If it remains inaccessible, ask that address for a working copy of the form. A [plain-text email draft](AWS_OSS_CREDITS_APPLICATION.txt) is also available.

## Applicant details to complete

- Maintainer/contact name, role, and email: **confirm with applicant**.
- Organization or project legal entity, if any: **confirm with applicant**.
- Current employer, if the applicant wants to disclose it: **confirm with applicant**. State whether the application is personal or on the employer's behalf; do not imply employer sponsorship or endorsement without authorization.
- Personal AWS background: **confirm exact wording and certification status**. Use the official name *AWS Certified Cloud Practitioner* only if the credential is current.
- Whether the project is independent of a single vendor and VC funding: **confirm with applicant**. AWS lists these as eligibility criteria; do not infer an answer from the repository.
- AWS account ID and account owner for receiving credits: **confirm with applicant**. Do not publish the ID in this repository.
- Existing credits or AWS sponsorship, if any: **confirm with applicant**.

## Project and benefit to AWS users

**Project:** Platform-IPAM — https://github.com/mischapogr/platform-ipam

**License:** Apache License 2.0 — https://github.com/mischapogr/platform-ipam/blob/main/LICENSE

**Suggested application text:**

> Platform-IPAM is an open source, pre-release service for policy-controlled IPv4 VPC and subnet allocation on AWS. It reserves non-overlapping CIDRs through a REST API, CLI, and Terraform provider, records allocation identity in a durable ledger, and presents network inventory in NetBox. Its read-only AWS reconciliation design checks whether VPCs and subnets exist and treats incomplete account or Region observations as unknown rather than proof that address space is free. The project also offers organization inventory, overlap assessment, and reviewed migration-planning workflows. These capabilities address problems AWS customers face when independently managed VPCs later need to connect through Transit Gateway or other network paths.
>
> The local Docker Compose stack has end-to-end tests across REST, CLI, Terraform, and the operator inventory. Live AWS Organizations, cross-account IAM, VPC/subnet observations, and Transit Gateway topology have not yet been validated. AWS credits would let us run repeatable integration tests in dedicated sandbox accounts and publish reproducible test results and fixes for the community.

**Maintenance and community evidence:** The repository has a `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, changelog, CI workflow, and recent commits. Provide links to actual external issues, discussions, users, or contributions if available. Do not claim community adoption or multiple independent maintainers without evidence.

**Optional maintainer background:** "I have approximately ten years of hands-on AWS experience and hold the AWS Certified Cloud Practitioner credential [if current]. I currently work at [company, if disclosed]. I am applying as the maintainer of Platform-IPAM in a personal capacity [if true]; my employer is not a sponsor of this application [if true]." Keep this brief and separate from the project's eligibility and community evidence.

## Proposed use of credits

Run two short-lived AWS test cycles per month for 12 months in dedicated, non-production sandbox accounts. Each cycle would:

1. Exercise Organizations account discovery and least-privilege cross-account read roles across two Regions.
2. Create temporary VPCs, primary and secondary CIDR associations, and subnets; compare collected inventory with AWS state, including partial and denied observations.
3. Exercise read-only Transit Gateway attachment and route observations for overlapping and isolated network examples. Address assessment and connectivity evidence remain separate.
4. Test reservation, binding verification, deletion observation, and reuse refusal after incomplete scans; tear down temporary infrastructure and publish test outcomes.

The proposed annual **credit request is USD 2,400**. This is a planning ceiling, not an AWS price quote or an assumed award:

| Use | Annual allowance |
| --- | ---: |
| Temporary VPC/Transit Gateway networking and data transfer | $1,200 |
| Short-lived compute for test runners and integration services | $600 |
| Logs, artifacts, and small storage | $120 |
| Pricing variance and failed-run retries | $480 |
| **Total** | **$2,400** |

Assumptions: 24 time-boxed test cycles per year; resources are destroyed after each run; no always-on EKS cluster or NAT gateway; account billing alerts and a monthly spend review. Recalculate with the [AWS Pricing Calculator](https://calculator.aws/) for the actual Regions, resources, run durations, and expected data transfer before submission. Adjust the requested amount to match that estimate and the maintainer's approved test plan.

## Draft cover email

**To:** awsopensourcecredits@amazon.com
**Subject:** AWS Cloud Credits for Open Source application — Platform-IPAM

Hello AWS Open Source team,

Please find attached the completed AWS Open Source Credits application for Platform-IPAM, an Apache 2.0 licensed project for AWS VPC and subnet address allocation and inventory. We are requesting credits for repeatable, short-lived sandbox integration tests of AWS Organizations discovery, cross-account read access, VPC/subnet reconciliation, and Transit Gateway topology observation. These tests will help us validate and improve behavior that currently has only local or synthetic coverage.

Project: https://github.com/mischapogr/platform-ipam
Requested budget and period: USD 2,400 over 12 months, subject to the attached application's final estimate.

Thank you for considering the project.

[Applicant name and role]

## Submission checklist

- Confirm applicant, funding/independence, AWS account, and any existing credits.
- Open the current AWS form, enter the project text and recalculated budget, and attach the completed form.
- Add evidence of real community engagement if available; keep unsupported claims out.
- Send the email from the applicant's account. Submission and any AWS account changes require the applicant's authorization.

## Source and project evidence

- [AWS program criteria and application instructions](https://aws.amazon.com/blogs/opensource/aws-cloud-credits-for-open-source-projects-affirming-our-commitment/)
- [Project README](../README.md), [license](../LICENSE), [contributing guide](../CONTRIBUTING.md), [AWS integration contract](AWS_INTEGRATION.md), and [validation status](README.md#validation-status)
