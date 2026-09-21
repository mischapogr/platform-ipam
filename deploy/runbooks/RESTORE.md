# Restore runbook

Restore the platform PostgreSQL database using the managed database service's
point-in-time or snapshot procedure. Keep the allocation ledger and its audit
history together; do not restore NetBox over it as a substitute. NetBox is an
independent inventory release and should be restored from its own backup.

After restore, pause new allocations and reclamation, verify database
connectivity with the migration Job and run a read-only reconciliation. Check
that allocation keys, durable holds, NetBox identity markers, and AWS scan
coverage agree before resuming admission.
