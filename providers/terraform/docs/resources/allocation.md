---
page_title: "platformipam_allocation Resource - Platform-IPAM"
description: |-
  Reserve a durable address allocation with a permanent logical identity.
---

# platformipam_allocation (Resource)

Reserves address space through the Platform-IPAM API. The AWS API/AWS integration is not yet ready; preview verification is local and simulated. This resource does not create or delete AWS infrastructure.

## Example Usage

```terraform
resource "platformipam_allocation" "vpc" {
  allocation_key = "prod-eu-central-1-orders-v1"
  scope          = "vpc"
  environment    = "prod"
  region         = "eu-central-1"
  account_id     = "123456789012"
  prefix_length  = 20
}

resource "platformipam_allocation" "private" {
  allocation_key       = "prod-eu-central-1-orders-private-v1"
  scope                = "subnet"
  environment          = "prod"
  region               = "eu-central-1"
  account_id           = "123456789012"
  prefix_length        = 24
  parent_allocation_id = platformipam_allocation.vpc.id
  availability_zone_id = "euc1-az1"

  timeouts = {
    create = "10m"
    update = "5m"
    delete = "5m"
  }
}
```

Connect each allocation's `cidr` to its AWS resource so Terraform destroys that cloud resource before releasing the allocation. Replacement requires a new allocation key. The provider rejects same-key immutable configuration changes, but Terraform taint and manual `-replace` require the shipped consumer guard: run `python3 /path/to/terraform-provider-platformipam/scripts/check-replacement-plan.py --plan-json PLAN.json` on `terraform show -json` output before applying that same saved plan. A nonzero exit blocks apply. Lifecycle and binding observations are not cloud provisioning dependencies.

## Schema

### Required

| Attribute | Type | Meaning |
| --- | --- | --- |
| `allocation_key` | String | Permanent tenant-local identity, 1–128 ASCII letters, digits, `.`, `_`, `-` or `/`, beginning with a letter or digit. Changing it requires replacement. |
| `scope` | String | `vpc` or `subnet`; immutable. |
| `environment` | String | Authorized environment identifier; immutable. |
| `region` | String | Authorized AWS region; immutable. |
| `account_id` | String | Authorized 12-digit AWS account ID; immutable. |
| `prefix_length` | Number | IPv4 prefix length, 0–32 and subject to pool policy; immutable. |

### Optional

| Attribute | Type | Meaning |
| --- | --- | --- |
| `address_family` | String | Defaults to `ipv4`; other families are unsupported. Immutable. |
| `parent_allocation_id` | String | Required for a subnet and forbidden for a VPC. Immutable. |
| `availability_zone_id` | String | Stable AWS AZ ID for a subnet; forbidden for a VPC. Immutable. |
| `description` | String | Mutable description, defaults to empty, maximum 512 bytes. |
| `labels` | Map of String | Mutable labels, defaults to empty, maximum 20 entries. Keys are 1–64 bytes, values at most 256 bytes and non-null; the `platform-ipam/` key prefix is reserved. |
| `timeouts` | Object | Optional duration strings `create` (default `10m`), `update` (`5m`) and `delete` (`5m`). |

### Read-Only

| Attribute | Type | Meaning |
| --- | --- | --- |
| `id` | String | Committed platform allocation ID. |
| `cidr` | String | Committed allocated CIDR. |
| `pool_id` | String | Selected pool ID. |
| `allocation_state` | String | Observed API lifecycle: `RESERVED`, `ACTIVE`, `QUARANTINED` or `RELEASED`. Retired records retain identity; their CIDRs cannot be provisioned. |
| `bound_resource_id` | String | Verified AWS resource ID, when observed. |
| `inventory_url` | String | Authorized opaque inventory link, when available. |
| `etag` | String | Revision validator used for metadata updates. |
| `revision` | Number | Current API revision. |

## Import

Import an existing platform allocation ID. Import reads the allocation and never claims an arbitrary CIDR or creates AWS infrastructure. Configuration must match the existing immutable request, including the adopted CIDR's actual prefix length.

```shell
terraform import platformipam_allocation.vpc alloc_01
```

On a lost or ambiguous Read, restore authorization or inspect the API before retrying; the provider preserves state. On Delete, state is removed only after durable quarantine or release evidence. Quarantine still holds the CIDR until the API's observation and reclamation gates pass.
