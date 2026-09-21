# Migration runbook

The chart owns one migration runner. The `Job` is annotated as a Helm
`pre-install,pre-upgrade` hook with a negative hook weight, so Helm waits for
the Job to complete before creating or updating the API and worker resources.
API and worker containers never run migrations on startup.

Before a release, verify that the new binary can read the existing schema and
that the referenced database Secret is present. Observe the Job and stop the
rollout on failure:

```sh
kubectl -n platform-ipam-stage get job -l app.kubernetes.io/component=migration
kubectl -n platform-ipam-stage logs job/<release>-platform-ipam-migrate
```

Migrations must remain compatible with the prior application version for the
rollback window. A Helm rollback restores manifests and images; it does not
reverse database changes.
