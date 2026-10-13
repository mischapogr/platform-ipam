---
page_title: "platformipam_pool Data Source - Platform-IPAM"
description: |-
  Read an authorized allocation pool summary without allocating space.
---

# platformipam_pool (Data Source)

Reads one authorized pool by ID, following the API's opaque collection pagination. It does not allocate space or establish that capacity is currently available. The AWS API/AWS integration is not yet ready; preview evidence is local and simulated.

## Example Usage

```terraform
data "platformipam_pool" "production" {
  id = "pool_prod_euc1"
}

output "allowed_prefix_lengths" {
  value = data.platformipam_pool.production.allowed_prefix_lengths
}
```

## Schema

### Required

- `id` (String) Authorized platform pool ID.

### Read-Only

- `scopes` (Set of String) Allocation scopes supported by the pool.
- `environment` (String) Pool environment.
- `region` (String) Pool AWS region.
- `allowed_prefix_lengths` (Set of Number) Allowed IPv4 prefix lengths from pool policy.
- `policy_version` (String) Policy version associated with the pool summary.

Missing or unauthorized pools and backend errors return diagnostics. Reads are policy observations; only the allocation API can decide admission at apply time.
