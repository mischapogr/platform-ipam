## What this changes

<!-- One paragraph. Why, not just what. -->

Refs: #

## Verification

<!-- State what you actually ran, and paste the outcome. -->

- [ ] `go test ./...` (in the pinned image)
- [ ] `./tests/e2e/run-e2e.sh`
- [ ] `scripts/ai/check-contract`
- [ ] `scripts/ai/check-compose`
- [ ] `scripts/ai/check-provider`
- [ ] `scripts/ai/check-helm`

**Not verified:** <!-- Be explicit. Live AWS, Kubernetes rollout, and provider
distribution are covered by nothing in this repository. Say so rather than
leaving it implied. -->

## Allocation safety

- [ ] Does not let a client choose its own tenant, pool, or CIDR
- [ ] Does not let held address space be reused without complete observation evidence
- [ ] Preserves one allocation per allocation key across retries and restarts
- [ ] No error is swallowed, and no generic failure discards its cause

<!-- If any box is unchecked, explain why here. -->

## Contract and documentation

- [ ] `api/openapi.yaml` updated, or unchanged by this PR
- [ ] Affected documents under `docs/` updated in this change
- [ ] A decision record added or amended if this constrains later work
- [ ] Commit messages follow Conventional Commits

## Breaking changes

<!-- Wire contract, CLI flags, provider schema, Helm values, config schema.
     Write "none" if there are none. -->
