---
page_title: "platformipam_allocation Data Source - Platform-IPAM"
description: |-
  Read one committed usable address allocation without reserving space.
---

# platformipam_allocation (Data Source)

Reads a committed `RESERVED` or `ACTIVE` allocation by exactly one of platform ID or permanent allocation key. It never reserves or releases space. `QUARANTINED`, `RELEASED` and pending reservations are rejected. The AWS API/AWS integration is not yet ready; preview evidence is local and simulated.

## Example Usage

```terraform
data "platformipam_allocation" "orders" {
  allocation_key = "prod-eu-central-1-orders-v1"
}

output "orders_cidr" {
  value = data.platformipam_allocation.orders.cidr
}
```

Alternatively configure `id = "alloc_01"` and omit `allocation_key`.

## Schema

### Optional

- `id` (String) Platform allocation ID. Exactly one of `id` or `allocation_key` must be configured. Also populated from the response.
- `allocation_key` (String) Permanent allocation key. Exactly one of `allocation_key` or `id` must be configured. Also populated from the response.

### Read-Only

| Attribute | Type | Meaning |
| --- | --- | --- |
| `scope` | String | `vpc` or `subnet`. |
| `environment` | String | Authorized environment. |
| `region` | String | AWS region. |
| `account_id` | String | AWS account ID. |
| `address_family` | String | Address family, currently `ipv4`. |
| `prefix_length` | Number | Allocated prefix length. |
| `parent_allocation_id` | String | Parent allocation ID for a subnet, when present. |
| `availability_zone_id` | String | Stable AZ ID, when present. |
| `description` | String | Current mutable description. |
| `labels` | Map of String | Current mutable labels. |
| `cidr` | String | Committed CIDR. |
| `pool_id` | String | Selected pool ID. |
| `allocation_state` | String | `RESERVED` or `ACTIVE`. |
| `bound_resource_id` | String | Verified AWS resource ID, when observed. |
| `inventory_url` | String | Authorized opaque inventory link, when available. |

Backend errors and ambiguous `404` responses return diagnostics. A pending allocation response names its operation for recovery; retry the read after it commits. Data source reads do not repair missing state or start reservations.
