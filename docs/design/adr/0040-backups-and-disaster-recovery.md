# ADR-0040: Backups and disaster recovery

**Status:** accepted (product owner, 2026-10-19)

## Context
01 §1.3 sets an RPO of at most one minute and an RTO of at most 30 minutes
for a region failure of a cell (an RPO of 0 for posted financial data
comes only with synchronous replication, which this ADR does not enable:
the cross-region replica below is asynchronous), and 05 §5.8 asks for failover and restore
drills. ADR-0035 gave every cell an RDS for PostgreSQL 17 database with two
weeks of point-in-time recovery (a Multi-AZ DB cluster with two readable
standbys in the production shape, a single-AZ instance in dev and test), an
object bucket (ADR-0016) and, on its cluster, NATS JetStream and Temporal,
whose two databases live on the cell's RDS. Nothing yet covered losing the
region, restoring the cell or one tenant to an earlier point, or proving
that a restore works.

A cell's state is in four places: the database (every context, the
outboxes, Temporal), the object bucket (documents, exports), JetStream
(events in flight to consumers) and the global directory (ADR-0022), which
is not part of the cell and is not restored with it.

## Decision

**Retention.** Point-in-time recovery keeps 35 days in production and 14
in dev, test and staging (`backup_retention_days`, default 14). Copies in
the disaster-recovery region are kept 7 days. The object bucket is
versioned and keeps noncurrent versions 35 days, so an object a restored
row points at still exists for the whole recovery window. There is no
long-term archive: the records kept for years outside the database (the
audit trail itself is in it), audit anchors and
offboarding exports (ADR-0016, ADR-0025, ADR-0021), are objects written with a
compliance-mode retention, which lifecycle rules cannot shorten. The
bucket's object lock is therefore enabled (it was missing; T41b), and so
is the replica's, which S3 requires to replicate locked objects.

**Cross-region copies (T41b).** A cell with a disaster-recovery region
(`dr_region`: staging and production, which run the Multi-AZ DB cluster;
none in dev and test) copies its database's backups there, encrypted with
a KMS key of that region: AWS Backup snapshots the cluster every hour and
copies each snapshot to a vault in that region, kept 7 days. RDS's own
cross-region replication of automated backups serves DB instances only;
the copies are the fallback when the region and the replica are both
lost, so their RPO is the hour, not the minute. The bucket replicates to a
bucket in the disaster-recovery region, encrypted with that key, keeping versions and
delete markers, so a deletion in the home region is a noncurrent version in
both.

**Region failover: a pilot light (T41b2).** A production cell has, in its
disaster-recovery region, a cross-region read replica of its database and a
pilot-light stack: the VPC, the endpoints and an EKS control plane with no
nodes, plus the cell's KMS keys' counterparts there. Asynchronous
replication keeps the replica seconds behind; an alarm fires when its lag
passes 60 s, the RPO. On a region failure the operator (runbook below)
promotes the replica, scales the node group up and lets Argo CD (ADR-0035)
sync the cell's `release.json` there; the cell's directory entry is then
switched to the new API address. Promotion takes minutes, nodes about five,
the sync about ten: within the 30 minutes.

To be confirmed on AWS staging before the pilot: that a Multi-AZ DB
cluster accepts a cross-region read replica and that AWS Backup copies
its snapshots across regions. If it does not, the production shape becomes a Multi-AZ DB
instance (one standby, not readable; readers move to the replica), which
supports both, and this ADR is amended; Aurora PostgreSQL Global Database
is the other option, at the cost ADR-0035 gave for leaving it out.

**JetStream is not backed up.** The outboxes keep every published row (the
relay may only set `published_at`; nothing deletes them except a tenant's
erasure), streams keep their messages without an age limit and every
message carries the event id as `Nats-Msg-Id`, and consumers deduplicate
by event id in their inboxes (ADR-0007). After a restore or a failover the
streams are recreated empty and the relay republishes from the outbox:
`UPDATE <context>.outbox SET published_at = NULL WHERE occurred_at >=
<cut-off>` as the relay role, the cut-off the oldest event any consumer may
still lack (the restore point less the consumers' longest lag, or the
beginning for a rebuilt region). Events published after the restore point
are not in the restored outbox: consumers outside the cell may have seen
events the cell no longer has, and the runbook names them.

**Temporal restores with the cell.** Its two databases are on the cell's
RDS, so a point-in-time restore or a promoted replica carries the
workflows at the same point as the data their activities changed; the
activities are idempotent (ADR-0012), so a workflow that re-runs an
activity whose effect the restore kept changes nothing.

**Whole-cell point-in-time recovery only.** RDS restores a whole cluster to
a new one. A tenant is restored to an earlier point by runbook: a side
instance restored to that point, the tenant exported there (ADR-0021),
erased in the cell and imported again from the export (ADR-0029).

**Erasure survives a restore.** A tenant erased after the restore point
comes back with the restore. The restored cell's offboarding worker erases
again every tenant that was offboarded before the point and whose
`erase_after` has passed; a tenant offboarded after the point, or erased
early by an operator, it does not. The directory is the record outside the
cell, but today it cannot keep one: a cell's directory sync upserts every
tenant's status from the cell and keeps only `read_only`
(`directory.Upsert`), so a restored cell's first sync would write such a
tenant back as `active`. T41d therefore makes the directory record
`erased` (written by the cell when it erases) and keep `offboarded` and
`erased` against a cell's sync, as it keeps `read_only`; the runbook lists
the directory's offboarded and erased tenants before a restored cell's
workloads start, and the restore verification offboards and erases them
again.

**Every restore is verified (T41d).** A verification command checks a
restored cell: the migration version, every audit hash chain against its
anchors (ADR-0025), a balanced journal and the trial balances, every tenant the directory
places in the cell present in it, the fingerprints of ADR-0030 compared
with the old database's where it is kept (a restore in place), and the RPO and
RTO from markers written before the incident and read after the restore.

**Drills (T41e).** Staging's drill cell restores itself to a point in time
and fails over to its disaster-recovery region; each run is appended to
`deploy/environments/staging/stg1/drills/restore.json` (who, when, restore
point, RPO and RTO measured, verification result), which only grows. The
promotion checks (ADR-0035) refuse a production promotion when the last
passed restore drill is older than 90 days.

**Failover inside a region (T41c).** A Multi-AZ failover moves the writer
within about 35 s; until PgBouncer reconnects, a statement can reach a
former writer that is now read-only. The platform answers SQLSTATE 25006
(`read_only_sql_transaction`) like a lost connection: 503
`platform.unavailable` with `Retry-After`, the transaction rolled back, so
a client's retry with its idempotency key posts once. Only a read-write
transaction is mapped so: 25006 in a read-only scope is a write the code
should not attempt, a bug that stays a 500. A resilience test
promotes a streaming replica behind PgBouncer under load and asserts
exactly that, weekly (`make failover`).

## Runbook

**Region failover** (production cell `<cell>`, disaster-recovery region
`<dr>`):
1. Declare the incident; freeze promotions to the cell.
2. Read the replica's lag (`ReplicaLag`); note it as the data lost.
3. Promote the replica (`aws rds promote-read-replica` in `<dr>`).
4. Scale the pilot-light node group to the cell's size; install the
   add-ons' Argo CD (ADR-0035) pointing at the cell's `release.json`, with
   the database endpoint of the promoted replica in the cell's values.
5. When Argo CD reports the cell synced: recreate the streams
   (`erp-server` creates them at start) and republish from the outboxes
   from the beginning, as above.
6. The cell's directory sync rewrites its entries' API address when the
   cell starts with its new `api_url`; check it with `erp directory
   locate`, then run the restore verification (T41d).
7. Record the RTO (incident to verified) and RPO in the incident; rebuild
   the home region as the new disaster-recovery region later.

**Whole-cell restore to a point in time** (corruption, a bad migration):
1. Freeze the cell's tenants in the directory (ADR-0023); stop the cell's
   workloads (scale to zero) so nothing writes. List the directory's
   offboarded and erased tenants of the cell (until T41d makes the
   directory keep them against a sync, this list is the record).
2. Restore the database to the point (`aws rds
   restore-db-cluster-to-point-in-time`, or `restore-db-instance-to-point-in-time`
   in dev and test) as a new database with the cell's parameter group,
   subnet group, security group and key.
3. Point the cell at it (the database endpoint in the cell's values, the
   master secret of the new database), start the workloads, recreate the
   streams and republish from the restore point less the consumers' lag.
4. Run the restore verification; offboard and erase again the tenants of
   step 1's list; resolve the tenants the directory places in the cell and
   the cell lacks (provisioned after the point).
5. Unfreeze; keep the old database (deletion protection on) until the
   verification has passed and the incident is closed.

**One tenant to a point in time:**
1. Restore a side database to the point as above. Run `erp-server`
   against it with the cell's object store only: no directory URL, no
   NATS, no Temporal, no workers, so it neither rewrites the
   directory's entries nor publishes to live consumers. Offboard the tenant
   there (only an offboarded tenant is exported) and export it through its control plane (ADR-0021).
2. Freeze the tenant; offboard and erase it in the cell
   (`:erase`, ADR-0021); import it from the side export (`erp-server
   import`, ADR-0029), which verifies the parts and the audit chain head.
3. Unfreeze; delete the side database. The tenant's events after the point
   are not replayed (ADR-0029 imports no outbox).

## Alternatives
- **Region option (b): backups only in the second region.** Cheaper, but a
  restore from copied snapshots and a cluster built from nothing exceeds
  30 minutes, and the RPO is the copy interval, not one minute.
- **Active-active cells in two regions.** Meets both targets with margin;
  doubles the cell's cost and needs multi-writer conflict handling the
  ledger cannot accept (ADR-0006).
- **Per-tenant point-in-time recovery from logical change streams.** Exact
  per tenant, but a second backup system to run and verify; the export and
  import already move tenants and are verified.
- **Backing up JetStream.** Its contents are derived from the outbox, which
  is backed up with the data that produced it; a separate backup would be
  restored to a different point than the database.

## Consequences
- The outboxes grow without bound; replay depends on it. Pruning them later
  needs a retention at least as long as the recovery window plus the
  consumers' lag.
- The DR region costs a replica instance, a VPC with NAT gateways, an EKS
  control plane and storage for the copies; dev and test have none.
- The directory gains an `erased` status and keeps `offboarded` and
  `erased` against a cell's sync (T41d).
- The pre-pilot confirmation on staging can change the production database
  shape; T41b2 is written so that the replica is one resource to replace.
