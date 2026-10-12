---
page_title: "Platform-IPAM Provider"
description: |-
  Reserve durable VPC and subnet address allocations through the Platform-IPAM API.
---

# Platform-IPAM Provider

## Versioning and compatibility

Provider versions follow Semantic Versioning independently of Platform-IPAM.
The unpublished `0.1.0-rc.1` candidate requires API v1; no Platform-IPAM product
release is qualified yet. See the provider repository's
[compatibility matrix and version policy](https://github.com/mischapogr/terraform-provider-platformipam/blob/main/COMPATIBILITY.md).

The `platformipam` provider reserves address space through the Platform-IPAM API v1. It provides one allocation resource and read-only allocation and pool data sources. A separate AWS provider manages cloud infrastructure.

**Preview limitation: AWS API/AWS integration is not yet ready.** Development verification uses the local API, simulated AWS observations and HTTP fixtures. There is no live AWS acceptance evidence. The approved public source address is `mischapogr/platformipam`; signed Registry distribution and consumer installation from that published origin remain pending. The example below is for the first published RC and cannot be installed from the Registry yet. Local development continues to use an explicit development override or test mirror.

## Example Usage

```terraform
terraform {
  required_providers {
    platformipam = {
      source  = "mischapogr/platformipam"
      version = "= 0.1.0-rc.1"
    }
  }
}

provider "platformipam" {
  endpoint = "https://ipam.example.com"
}

resource "platformipam_allocation" "orders" {
  allocation_key = "prod-eu-central-1-orders-v1"
  scope          = "vpc"
  environment    = "prod"
  region         = "eu-central-1"
  account_id     = "123456789012"
  prefix_length  = 20
  description    = "Orders VPC"
  labels         = { service = "orders" }
}
```

Supply the bearer token through `PLATFORM_IPAM_TOKEN`. Keep credentials out of source control. A local development endpoint may use loopback HTTP only when `PLATFORM_IPAM_ALLOW_LOCAL_HTTP=1`; other endpoints require HTTPS.

## Schema

### Required

- `endpoint` (String) Platform API origin. Use HTTPS except for explicitly enabled local loopback HTTP.

### Optional

- `token` (String, Sensitive) Bearer token. Defaults to `PLATFORM_IPAM_TOKEN`. Token configuration is provider-only and is not copied into allocation state.

## Allocation Safety and Recovery

Planning, validation and data sources never allocate. A new CIDR is unknown until apply obtains a committed `RESERVED` or `ACTIVE` allocation. Timeout retries use the same permanent allocation key; a provider timeout never cancels a pending reservation.

Use a new `allocation_key` generation for every replacement. The provider rejects configured immutable changes that keep the same key. Terraform taint and manual `-replace` can still propose a same-key replacement; the provider alone cannot prevent those Terraform-core decisions. Run the portable `scripts/check-replacement-plan.py` shipped in this provider repository before applying every saved plan. It rejects unchanged or unknown replacement keys, errored plans, deferred plans and unsupported plan formats.

```shell
terraform plan -out=tfplan
terraform show -json tfplan > tfplan.json
python3 /path/to/terraform-provider-platformipam/scripts/check-replacement-plan.py --plan-json tfplan.json
terraform apply tfplan
```

Run apply only when the check exits zero, and apply that same saved plan. Gate consumer CI on this command. The guard needs only Python 3 and can run from any directory; the JSON can contain credentials or sensitive values, so keep it with the protected saved plan. The monorepo's `scripts/ai/check-provider --plan-json PLAN.json` invokes the same policy.

An ambiguous authorization-related `404` retains managed state and reports a diagnostic. If Create records an allocation and then fails to update metadata, Terraform may taint it: stop applies, verify the API allocation and cloud identity, then explicitly untaint or re-import without release. Resume with an inspected plan.

Destroy cloud resources before releasing their allocations. Provider Delete completes on durable quarantine acceptance; it does not make the CIDR immediately reusable and never deletes AWS resources. Released keys remain retired permanently. `terraform state rm` does not release space. Keep each allocation in one Terraform state and serialize runs against that state's backend.

The current local toolchain uses Terraform 1.14 and the provider's declared protocol 6.0. Other CLI versions, operating systems and architectures need separate qualification. See the [project client contract](https://github.com/mischapogr/platform-ipam/blob/main/docs/CLIENTS.md) for development installation, AWS dependencies and recovery procedures.
