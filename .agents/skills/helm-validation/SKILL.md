---
name: helm-validation
description: Change or validate platform-ipam Helm charts and stage/prod values, migration ordering, workload identity, and rollout behavior.
---

Read [Kubernetes deployment](../../../docs/DEPLOYMENT.md) and the chart/values affected by the task. Stage and prod promote the same application digest/chart version with explicitly different endpoints, roles, secrets, and policies.

Run `scripts/ai/check-helm` for strict chart lint and local rendering of each environment. Missing chart, values, or Helm is BLOCKED. Rendering cannot prove cluster API compatibility, workload identity, secret availability, migration ordering, or rollout readiness; test those separately when authorized.

- Keep API and worker Deployments independent while sharing ledger coordination. Replica count does not establish allocation safety.
- Keep NetBox as an independently managed release/service.
- Migration completion needs explicit release ordering; simply rendering a Job does not enforce it.
- Production configuration rejects development identity/fake AWS modes. Keep credentials in secret references.
- Rollback preserves durable holds and requires database compatibility with the old binary; Helm rollback does not rewind the database.

Use selected rendered resources and bounded events/logs for diagnosis. Treat deployment as a separate task from local chart validation.
