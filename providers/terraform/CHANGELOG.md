# Provider changelog

This provider is independently versioned using [Semantic Versioning](https://semver.org/).
Published entries must include their source commit, compatibility matrix and
migration notes when required. Installation requires signed Registry distribution.

## 0.1.0-rc.1 — simulation RC

- Allocation resource; read-only allocation and pool data sources; import,
  durable allocation identity and quarantine-aware deletion.
- Explicit replacement plan guard and diagnostics for ambiguous reads.
- Independent provider version metadata and an API/product compatibility matrix.
- Simulation qualification covers lifecycle, timeout recovery, unchanged identity
  on ambiguous reads, applied replacement with a new key, persisted quarantine,
  normal filesystem-mirror installation, checksum refusal and vulnerability scanning.
- API v1 required. No Platform-IPAM product release is qualified yet.
- Simulation only. **AWS API/AWS integration is not yet ready.**
