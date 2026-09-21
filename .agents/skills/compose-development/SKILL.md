---
name: compose-development
description: Develop and diagnose the local platform-ipam Docker Compose stack, fixtures, readiness, and restart recovery.
---

Read [development deployment](../../../docs/DEPLOYMENT.md) and the actual Compose files before choosing commands. Local services use the same migrations/API/worker entry points as Kubernetes, with isolated development data and fake AWS observations by default.

Run `scripts/ai/check-compose` to validate the committed Compose file combinations. The helper does not start containers, seed data, or test runtime readiness. If files or Docker are missing, retain the explicit BLOCKED result.

For authorized runtime work, use the project's named Compose files and development project identity. Verify health and migration completion before seeding; make seeding repeatable. Preserve named volumes during ordinary restart tests. Do not reset volumes or point fixtures at stage/prod as an incidental troubleshooting step.

When testing allocation recovery, keep ledger and NetBox volumes and simulate a lost response or worker restart. Verify one logical allocation, not merely a healthy UI. Return bounded service-specific logs with their timestamps; do not dump interpolated secrets or full container environments.
