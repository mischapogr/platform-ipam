# Licensing and small-business brainstorming

Status: discussion notes, updated 2026-09-20. This document records hypotheses
and corrections; it does not change the project's licence or commercial terms.
[ADR 0005](decisions/0005-PUBLIC_RELEASE_LICENCE_AND_DISTRIBUTION.md) remains the
licensing decision, and [LICENSE](../LICENSE) contains the applicable terms.

## Current framing and corrections

The current Apache-2.0 licence does not require companies to pay for using
platform-ipam. A paid offer must give customers something they choose to buy,
such as onboarding, integration, supported upgrades, or a defined maintenance
service. Downloads, commercial use, and the customer's company size do not
create a subscription obligation. Redistribution has conditions in the
[Apache-2.0 text](https://www.apache.org/licenses/LICENSE-2.0); those conditions
are not a general fee for internal use.

The historical notes below contain several claims that need qualification:

| Topic | Current interpretation for this discussion |
| --- | --- |
| AGPL plus a commercial option | A company can use AGPL software without paying when it complies with the applicable terms. A commercial option can offer different permissions; AGPL does not itself require a business subscription. Its network-source provision concerns modified versions and users interacting remotely. See [AGPL-3.0, including sections 2 and 13](https://www.gnu.org/licenses/agpl-3.0.en.html). |
| Elastic License 2.0 | ELv2 grants royalty-free use subject to its restrictions. Internal production use is not generally a paid-use trigger. Restrictions include certain hosted or managed services, bypassing licence-key controls, and removing notices. See the [ELv2 text](https://www.elastic.co/licensing/elastic-license). |
| Business Source License 1.1 | Production permissions depend on the specific Additional Use Grant, version, and Change Date/Change License. The label alone does not establish that every business must pay. See the [BSL 1.1 text](https://mariadb.com/bsl11/). |
| Ownership and prior distribution | An uncommitted local checkout does not prove that no copy was previously distributed, nor that one person owns all relevant copyrights. Review contributions, dependencies, employment or assignment obligations, and prior grants before considering another licence. |
| Future licensing changes | A local file edit does not withdraw rights already granted to recipients. Any future change needs a separate decision and a review of the rights available for the affected code. No change is proposed by moving these notes. |

Commercial audit rights, usage reporting, and support obligations depend on an
actual agreement; they do not arise automatically from publishing software.
Licence keys, registries, or telemetry cannot turn current Apache-2.0 users
into customers who owe payment. Trademark rights also do not create a general
right to charge for permitted use of the code.

## Revenue hypothesis to validate

The maintainer confirmed these constraints on 2026-09-20:

- Target: **EUR 500–1,500/month recurring revenue**, before tax and business
  costs. One-time assessment/onboarding fees do not count toward that target.
- Paid onboarding and scheduled support are acceptable **with a strict time
  limit**. The actual per-engagement, per-customer and total time budgets remain
  to be agreed; product maintenance, sales and administration need separate time.

The discussed scenario of 2–6 customers at EUR 250/month (EUR 3,000/year each)
would yield EUR 500–1,500/month equivalent. Neither that price nor the suggested
one hour of monthly assistance is an accepted commercial term or validated
customer willingness to pay. A paid pilot should measure effort before those
limits are offered as a repeatable package.

For a solo developer seeking modest recurring income, test a bounded service
around an existing NetBox, AWS, and Terraform workflow:

1. A paid onboarding engagement: assess the existing address estate, import
   reviewed occupancy, establish allocation policy, and demonstrate the
   customer's supported reservation and release workflow.
2. Optional annual maintenance: agreed compatibility checks, supported
   upgrades, and support with explicit scope, working hours, and limits.
3. Customer-owned operation with clear recovery instructions and a tested
   handover, so delivery does not imply an unlimited managed-service or
   availability commitment.

This is a business hypothesis, not validated demand, an available support
contract, or a revenue forecast. Prices, support effort, renewal willingness,
and the number of customers a solo maintainer can serve remain unknown. A
customer who only needs a few static CIDRs may be adequately served by existing
tools and a documented manual process.

An existing overlap is a different problem from preventing the next overlap.
The concrete scenario, current capability boundaries, possible paid assessment
scope, and unanswered questions are recorded in
[IP overlap and migration](IP_OVERLAP_MIGRATION.md). A migration assessment can
be a bounded engagement; recurring maintenance needs a recurring customer
benefit of its own.

Questions to resolve before investing in a paid edition:

- Which buyer already has NetBox, AWS, Terraform, and a repeated allocation or
  migration problem with an identifiable cost?
- What did their current scripts or process fail to do, and why would they pay
  for a supported workflow rather than repair that process?
- Would they pay for an initial assessment, for continuing maintenance, or for
  both? What specific deliverables and response expectations would they buy?
- Can onboarding and upgrades follow one repeatable process within a solo
  maintainer's capacity, without extensive custom work for every installation?
- What measured pilot results would justify further product work or a future
  licensing discussion?

## Alternative product and pricing proposals

Two additional proposals are being considered: a simplified SMB/MSP IPAM with
many discovery sources, and an AWS connectivity-change assessment/migration
workflow. Their scope, competitors and architecture implications are recorded
in [product directions under consideration](IP_OVERLAP_MIGRATION.md#9-product-directions-under-consideration).
Neither is an accepted pivot or a commitment to build a paid edition.

Compare the support implications of low-priced subscriptions with fewer
bounded engagements. As arithmetic scenarios, 40 customers at EUR 25/month and
four customers at EUR 250/month both produce EUR 1,000/month before costs and
tax. At just 15 minutes per customer monthly, the first scenario already needs
ten support hours, before connector maintenance, onboarding or sales. The
second has greater revenue concentration and may bring more demanding customers.
Neither support estimate nor price is market evidence.

The suggested Community/Pro feature split also needs a separate decision.
Scheduled reconciliation already exists in the current code: a new paid offering
cannot assume that this functionality is absent from the Apache-2.0 version.
Do not infer an entitlement or a licence change from these pricing sketches.

For the confirmed income/time constraints, test a paid AWS assessment and a
bounded maintenance offer first. A one-time migration may end successfully and
leave no recurring need. Renewal must be justified by changing accounts and
connectivity, repeated reviews, maintained compatibility or another demonstrated
ongoing benefit. Avoid expanding connector coverage before repeatability and
support effort are measured with an actual customer.

## Historical source notes

The following text is preserved from `_tmp/BRAINSTORMING-LICENSING.md` for
context. It is not an accepted licensing decision. Its statements about sole
authorship, publication history, mandatory payment, and the equivalence of
different licences must be read with the corrections above.

<!-- Historical source text starts below; preserved without edits. -->
how to make sure the companies, which are downloading and using the current project are really paying according to the current license?

------------------------------------------------------------------------------------------------------------------------
With the license this project has now, they do not have to pay anything. You chose Apache-2.0, which lets anyone, companies included, download, use, modify and redistribute the project commercially for free. Their only obligations are to keep the license and NOTICE text, state changes they make to
files, and leave the patent terms intact. There is no payment to enforce. What you can enforce is attribution, and your trademark if you register one.

If you want companies to pay, the license or the business model has to change. Timing matters here. main has zero commits and nothing is public, and you are the sole author. So you can still pick any license freely today. Once the repo is public under Apache-2.0, every published version stays
Apache-2.0 for ever. You could only change the license for later versions.

Common models that make companies pay

┌────────────────────────────────────────────┬──────────────────────────────────────────────────────────────────────────────────────────────────────────┬───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│                   Model                    │                                               How it works                                               │                                                                   Trade-off                                                                   │
├────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ Dual licensing (AGPL-3.0 plus commercial)  │ The code is free under AGPL. A company that does not want AGPL's obligations buys a commercial license   │ Many companies ban AGPL outright, which is what drives sales, but it also reduces adoption. You must own all the copyright, so outside        │
│                                            │ from you.                                                                                                │ contributors would need to sign a contributor license agreement (CLA).                                                                        │
├────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ Source-available (BSL 1.1, Elastic License │ The code is visible, and production or commercial use above some limit needs a paid license. BSL         │ It is not open source by the OSI definition, so you could not call the project that. Some communities react badly to it.                      │
│  v2, PolyForm)                             │ converts to open source after a set number of years.                                                     │                                                                                                                                               │
├────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ Open core                                  │ The core stays Apache-2.0. Paid features such as SSO, audit export or multi-region live in a separate    │ You need features worth paying for that are not in the core.                                                                                  │
│                                            │ proprietary repo or binary.                                                                              │                                                                                                                                               │
├────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────────────────────────────┼───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ Paid support, hosting or SLA               │ The code stays free, and companies pay for support contracts, a hosted service, or certified builds.     │ Revenue depends on service quality, not on a license. It is the easiest model to combine with Apache-2.0.                                     │
└────────────────────────────────────────────┴──────────────────────────────────────────────────────────────────────────────────────────────────────────┴───────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘

How compliance is actually checked, once you have a license that requires payment:

- The contract. The commercial license grants you the right to audit the company and requires them to report usage. With larger companies this matters most, because their legal and procurement teams want to stay clean.
- License keys or signed entitlements for the paid binaries or features. Determined users can bypass these, but they deter casual non-payment and they show intent if a dispute arises.
- Distribution control. Paid builds come only from an authenticated registry or download portal, so you know who has them.
- Opt-in telemetry or a version check. This shows roughly who runs the software. It must be disclosed and easy to disable, and it must respect GDPR. Hidden phone-home behaviour destroys trust, and infrastructure teams often block it anyway.
- Public signals such as job postings, conference talks, GitHub issues and forks, or container image pulls. In practice this is how violators usually get found.
- A registered trademark stops others from selling "platform-ipam" under that name, even where the code license allows reuse.

Two caveats:

- No technical measure truly forces payment for self-hosted software. Enforcement in the end is legal (copyright or contract), and it only works if you own all the copyright and the license terms are clear.
- This is general information, not legal advice. If revenue depends on the license, have a lawyer review the license choice and the commercial terms before the first public push.

If you decide Apache-2.0 is not what you want, say so in the main conversation. The LICENSE, NOTICE, README and CONTRIBUTING files would need to change before the initial commit, and a CLA would need to be added.
