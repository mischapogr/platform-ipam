# AWS Open Source Credits: test budget estimate

Status: provisional planning budget, 2026-09-29. This is a public summary of
Platform-IPAM's proposed AWS test workload. It contains no applicant or AWS
account details and is not an AWS Pricing Calculator share link.

## Workload

Run two short-lived test cycles per month for 12 months in non-production
sandbox accounts in `eu-central-1` (24 cycles total). Each cycle exercises
cross-account read-role discovery, temporary VPCs and subnets, Transit Gateway
attachments and topology observations, allocation and binding checks, then
tears down the temporary resources. The plan excludes an always-on EKS cluster
and NAT gateway.

## Annual request

The proposed **USD 2,400** annual request is a planning ceiling, equivalent to
an average allowance of **USD 100 per cycle**. These category allowances are
not service prices calculated from configured resource quantities.

| Use | Per-cycle allowance | Annual allowance (24 cycles) |
| --- | ---: | ---: |
| Temporary networking, Transit Gateway and data transfer | $50 | $1,200 |
| Short-lived test runners and integration services | $25 | $600 |
| Logs, artifacts and small storage | $5 | $120 |
| Pricing variance and failed-run retries | $20 | $480 |
| **Total** | **$100** | **$2,400** |

[AWS Transit Gateway pricing](https://aws.amazon.com/transit-gateway/pricing/)
charges by attachment-hour and traffic processed. [Amazon VPC
pricing](https://aws.amazon.com/vpc/pricing/) can add public IPv4 and optional
network-service charges. Compute, logs, storage and data transfer depend on
the chosen instances, runtime and traffic. Billing alerts and a monthly spend
review are planned controls.

## Pricing gate before application

The resource counts, attachment hours, instance types and hours, traffic,
public IPv4 usage, log volume and storage duration have not been specified or
priced. Configure those inputs for `eu-central-1` in the [AWS Pricing
Calculator](https://calculator.aws/), then generate its public estimate URL via
**Share**. Replace the provisional request if the priced total differs. [AWS's
sharing guide](https://docs.aws.amazon.com/pricing-calculator/latest/userguide/save-share-estimate.html)
explains that a link is created when the estimate is saved and remains valid
for one year. This repository document is the public budget rationale; it
must not be presented as that calculator-generated URL.
