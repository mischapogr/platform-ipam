# ADR 0021: Keep AWS topology evidence separate from address assessment

Status: accepted for the local collector and workspace, 2026-09-23. Live AWS
behavior and a combined TGW readiness contract are not yet qualified.

## Context

`onboard assess` evaluates observed VPC CIDR relationships against a reviewed
intended connectivity matrix. Current TGW routes and attachments can explain
dependencies but cannot establish the target connectivity requirement or prove
traffic. The worker's cross-account role grants only the EC2 reads required
for VPC/subnet reconciliation.

## Decision

Collect route and TGW observations in a separate versioned `topology.json`
with account/region and per-stage gaps. Enable the eight additional EC2 read
actions only for a separately named operator role using the
`EnableTopologyDiscovery` parameter. An unread stage and a truncated route
search remain incomplete evidence. The NetBox workspace displays topology
beside, but does not join it into, the assessment or progress reports. No
address-readiness result may be presented as verified traffic connectivity.

## Consequences

- Existing worker permissions and assessment results remain unchanged.
- A future combined report needs an explicit join contract for source hashes,
  timestamps, account/region coverage, shared TGW visibility and intended
  routing domains. It must keep address blockers, configured routes and tested
  traffic as separate findings.
- The operator must run the collector with a reviewed AWS identity and check
  representative accounts before using the output in a customer pilot.
