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
both. The replica keeps noncurrent versions 35 days too, so both
expire at the same age. Replication covers objects written after it is
enabled: the objects already in a bucket are copied once by an S3 Batch
Replication job when it is turned on. AWS Backup's snapshots count as
manual DB cluster snapshots, whose default quota (100 a region) a week of
hourly copies passes; the module raises it to 250 in both regions.

**Region failover: a pilot light (T41b2).** A production cell has, in its
disaster-recovery region, a cross-region read replica of its database and
a pilot-light stack: the VPC, the endpoints and an EKS control plane whose
node group is at zero, plus the cell's KMS keys' counterparts there (the
field key is a multi-Region key replicated there, so tenant keys wrapped
at home unwrap; the secrets are replicated), and the workloads' roles
there, trusting the DR cluster and using the DR bucket and keys (T41b3,
T72j; as built below). Asynchronous replication keeps the replica seconds
behind; an alarm fires when its lag passes 60 s, the RPO. On a region
failure the operator (runbook below) promotes the replica, scales the node
group up and lets Argo CD (ADR-0035) sync the cell's `release.json` there;
the cell's directory entry is then switched to the new API address.
Promotion takes minutes, nodes about five, the sync about ten: within the
30 minutes.

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
cell (T41f): it records `erased` (the cell's sync sends it for a tenant
with `erased_at`, and the control plane's erasure at once), and
`directory.Upsert` keeps `offboarded` (but for `erased`) and `erased`
against a cell's sync, as it keeps `read_only`, so a restored cell's first
sync cannot write such a tenant back as `active`. Before the restored
cell's workloads start, `erp-server dr reconcile` offboards again (through
the control plane, audited) every tenant the directory has offboarded or
erased and the cell does not, and erases again those the directory has
erased: against the export already in the object store, which it records
as the tenant's (`offboarding.Worker.Adopt`) rather than exporting erased
data a second time; a tenant with no export there is a problem for the
operator. A tenant the directory has only offboarded is offboarded again
and starts its grace period over: the worker exports it once more, a
second locked copy, which the erasure leaves as the first. Only the
entry's own cell writes it (ADR-0023), so a tenant re-onboarded into
another cell (an export and an import, ADR-0029) is first moved there
(`:move` takes an offboarded or erased entry, keeping its status, so the
router still refuses it) and the import then reinstates it (should the
import never run, the old cell's later erasure leaves the moved entry
offboarded, which the router refuses as well); an import back
into the same cell reinstates the entry
(`directory.Postgres.Reinstate`, run by `erp-server import`), the one way
past what a sync cannot undo. The reconciliation lives in
`internal/drverify` and uses the control plane (an audited offboarding),
the directory, the offboarding worker and the audit context's anchor
format: an operations package beside the contexts, allowed in
internal/layout.

**Every restore is verified (T41d).** `erp-server dr verify` checks a
restored cell before it serves, reading only (internal/drverify): the
schema at the migration version the binary was built for; every tenant's
audit hash chain recomputed against its chain head (ADR-0011); every
journal entry balanced in all three currencies and every legal entity's
ledger netting to zero (bigint sums, ADR-0005); every tenant's per-table
counts and chain head (the fingerprint of ADR-0030), compared with the
old database's where it is kept (`ERP_DR_OLD_DATABASE_URL`: a restore in
place, a promoted replica); and the drill's markers. A marker is an audit
event of the drill tenant (`erp-server dr mark`), so it is restored,
exported and verified with the chain and needs no table: the drill writes
one before the restore point and one after, and the restored cell must
hold exactly the first, with the tenant's fingerprint as the marker's
transaction left it. The report is JSON; a failed check exits 1. It runs in
`erp-server`, not the `erp` client: it reads the database directly, before
the cell's API is up. `erp-server dr reconcile` (T41f) follows it, with
the cell's whole configuration (its object store and directory): every
anchor the restored database records agrees with its event and has its
object (the objects are locked, so existing is enough), and every anchor
object it does not record agrees with the restored chain at its seq when
the restore kept that event (anchors of events after the point are
counted, not faulted); every tenant the directory places in the cell is in
it (one provisioned after the point is a problem to resolve); and the
re-offboarding and re-erasure above. It writes, unlike verify, so it is a
step of a real restore and not of a drill (a drill's side cluster has no
object store or directory of its own).

**Drills (T41e).** Staging's drill cell restores itself to a point in time
and restores from its copies in the disaster-recovery region (staging has
the copies, not the replica and pilot light, which production alone pays
for), with `scripts/restore-drill.sh` (`RESTORE_PROVIDER=aws`,
`RESTORE_KIND=point-in-time` or `dr-copies`): it writes the drill
tenant's "before" marker, takes the restore point (for the copies, the
creation of the first copy made after the marker), writes the "after"
marker, restores a side cluster, runs `erp-server dr verify` as a one-off
pod of the cell's server image against it, measures the RPO (the point
less the "before" marker) and the RTO (the restore's start to the
verification's pass; past 30 minutes the drill fails), and removes the
side cluster. Its pods are labelled `erp-restore-drill`, which the
erp-cell chart's NetworkPolicy of that name lets reach the databases in
the VPC past the cell's default deny. The RPO so measured shows that the
restore kept what it should up to its point; the data a real incident
loses is the replica's lag (region failover) or the age of the newest
copy (restore from copies), which the runbook reads at the time. Each run is appended to
`deploy/environments/staging/stg1/drills/restore.json` (kind, provider,
who, when, restore point, RPO and RTO, result, the run's log), which only
grows, merged by a pull request of its own. The promotion checks (ADR-0035)
refuse a production promotion when the last passed restore drill on AWS
started more than 90 days before; a rollback is exempt, being the way out
of an incident (product owner, 2026-10-19). Until the first AWS drill has
passed on staging, every production promotion is refused: there is no
pilot before one (a pre-pilot item). The same drill runs in Docker (`RESTORE_PROVIDER=local`,
`make restore-drill`, weekly.yml's `dr` on demand): a base backup and the
WAL archive restored with `recovery_target_time`; its runs do not count.
A copy is restored in the disaster-recovery region, where staging has no
Kubernetes cluster and the home VPC is not peered (product owner,
2026-10-19; T41e2). The cell's Terraform keeps there, for staging's drill
cell (`restore_drill_dr`), a VPC with one NAT gateway, the database subnet
group and security group the copy is restored into, a Fargate cluster, a
security group that reaches only PostgreSQL in the VPC and HTTPS, an
execution role that pulls the image and reads the drill's side clusters'
RDS-managed secrets only, and a log group under the DR key (the
`restore_drill` output). The drill registers a task definition of the
cell's server image (from the registry's replica in that region) running
`erp-server dr verify` with the markers in its environment, the side
cluster's master password from its secret (`PGPASSWORD`) and the region's
RDS authorities as PEM (`ERP_DR_DATABASE_CA`, written to a temporary file
and named as `sslrootcert`, the TLS check unchanged); runs it once, reads
its report from the log group, and removes the task definition with the
side cluster. Nothing runs between drills.

**The region failover is rehearsed** (product owner, 2026-10-19) on the
production pilot cell before it takes live traffic, as a gate of the
pilot, by the runbook below end to end, then once a year in a maintenance
window. Before T41b is applied to a cell that already holds objects, an S3
Batch Replication job copies them once, and the plan is checked to create
the field key as a multi-Region key (a key cannot become one later).

**Failover inside a region (T41c).** A Multi-AZ failover moves the writer
within about 35 s; until PgBouncer reconnects, a statement can reach a
former writer that is now read-only. The platform answers SQLSTATE 25006
(`read_only_sql_transaction`) like a lost connection: 503
`platform.unavailable` with `Retry-After`, the transaction rolled back, so
a client's retry with its idempotency key posts once. Only a read-write
transaction is mapped so: 25006 in a read-only scope is a write the code
should not attempt, a bug that stays a 500 (the unit of work marks a
safe method's transaction read-only, `problem.FromReadOnly`). A
resilience test asserts exactly that, weekly (`make failover`,
`scripts/failover.sh`): a primary, a streaming replica and PgBouncer in
transaction mode in Docker; under load the primary restarts as a standby
(a fence by `default_transaction_read_only` does not hold: PgBouncer
restores the client's value on every server connection), writes that
reach it are refused with 25006, the replica is promoted once it has
replayed the shutdown checkpoint, and PgBouncer is repointed (PAUSE,
RELOAD, RESUME). Only 503s, every retried post once, recovery within 60 s.

## Runbook

**Region failover** (production cell `<cell>`, disaster-recovery region
`<dr>`). The values named below are the environment root's
`cell.pilot_light` output, read and kept before any incident: Terraform
cannot plan the root while the home region is down. Every command runs
in `<dr>`. The steps marked *(staging)* are confirmed on AWS staging
before the rehearsal (T41g). Until the global tier and the subordinate
CA are recovered from a home-region failure (see "Not covered" below),
steps 5, 7 and 8 cannot complete for the environment's home cell, the
production pilot cell among them: its `cell.json` names the directory,
whose password secret (`erp/global/directory-cell-password`) has no
replica in `<dr>` and which the DR roles may not read, so erp-server does
not start there, and step 8 needs the directory itself. No interim
procedure is decided; T41g waits on that recovery.
1. Declare the incident; freeze promotions to the cell.
2. Read the replica's lag (`ReplicaLag`); note it as the data lost.
3. Promote the replica: `aws rds promote-read-replica
   --db-instance-identifier <database_instance>`. Then have RDS manage
   its master password in `<dr>` under the DR key *(staging)*:
   `aws rds modify-db-instance --db-instance-identifier <database_instance>
   --manage-master-user-password --master-user-secret-kms-key-id
   <database_master_secret_kms_key_arn> --apply-immediately`; the new secret's ARN
   is the cell's master secret there.
4. Scale the node group to the cell's size (`node_scaling`): `aws eks
   update-nodegroup-config --cluster-name <cluster_name> --nodegroup-name
   <node_group> --scaling-config minSize=<min>,maxSize=<max>,desiredSize=<desired>`.
   Once the nodes are ready, add the EBS CSI driver as its role *(staging)*:
   `aws eks create-addon --cluster-name <cluster_name> --addon-name
   aws-ebs-csi-driver --service-account-role-arn <workload_role_arns.ebs_csi>`
   (with `--addon-version <ebs_csi_addon_version>` when it is set).
5. Point the cell at `<dr>`, by one pull request: the cell's `cell.json`
   takes the pilot light's values (the region, `cidr`, `vpc_id`, the
   workload roles, the bucket's replica, the field key's replica, the DR
   cluster key, the log group, the AWS endpoints, the promoted replica's
   endpoint and master secret, and the registry's replica in `<dr>`), and
   its `release.json` the same digests from the registry's replica in
   `<dr>`.
6. Bootstrap the cell's delivery on the DR cluster as ADR-0035's bootstrap
   step 6 (`aws eks update-kubeconfig --region <dr> --name <cluster_name>`;
   the deploy key from the replica of `erp/<cell>/argocd-repository` in
   `<dr>`; Argo CD from its pinned archive; the project and the root
   Application) *(staging)*.
7. When Argo CD reports the cell synced: recreate the streams
   (`erp-server` creates them at start) and republish from the outboxes
   from the beginning, as above.
8. The cell's directory sync rewrites its entries' API address when the
   cell starts with its new `api_url`; check it with `erp directory
   locate`, then run the restore verification (T41d).
9. Record the RTO (incident to verified) and RPO in the incident; rebuild
   the home region as the new disaster-recovery region later.

**Whole-cell restore to a point in time** (corruption, a bad migration):
1. Freeze the cell's tenants in the directory (ADR-0023); stop the cell's
   workloads (scale to zero) so nothing writes. The directory keeps the
   cell's offboarded and erased tenants against any sync (T41f).
2. Restore the database to the point (`aws rds
   restore-db-cluster-to-point-in-time`, or `restore-db-instance-to-point-in-time`
   in dev and test) as a new database with the cell's parameter group,
   subnet group, security group and key.
3. Point the cell at it (the database endpoint in the cell's values, the
   master secret of the new database), start the workloads, recreate the
   streams and republish from the restore point less the consumers' lag.
4. Run the restore verification (`erp-server dr verify`), then
   `erp-server dr reconcile` as a one-off pod with the cell's server
   configuration: it offboards and erases again what the directory says,
   and names the anchors that disagree and the tenants the directory places
   in the cell and the cell lacks (provisioned after the point), which the
   operator resolves before unfreezing.
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
- **Backups only in the second region, without a replica or pilot light.** Cheaper, but a
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
  Staging's costs a VPC with one NAT gateway for the restore drill
  (T41e2); its Fargate task runs only during a drill.
- The directory gains an `erased` status and keeps `offboarded` and
  `erased` against a sync from the entry's own cell (T41f); only an import
  reinstates such an entry.
- The pre-pilot confirmation on staging can change the production database
  shape; T41b2 is written so that the replica is one resource to replace.

## As built (2026-10-29, T72j): the pilot light made runnable

`deploy/terraform/modules/cell` (`pilot_light.tf`,
`pilot_light_workloads.tf`), tested with mocked providers in
`tests/cell.tftest.hcl` and `modules/environment`'s tests:
- **Nodes.** A managed node group in the DR cluster at zero (minimum and
  desired), its maximum the cell's (`node_scaling.max`), with the home
  nodes' role, instance types and image and a launch template of its own
  whose volumes are encrypted with the DR cluster key (whose policy now
  admits Auto Scaling as the home cluster key's does) and whose instance
  metadata is closed to pods. The runbook scales it with the EKS API and
  Terraform ignores the count afterwards. The VPC CNI is an add-on of the
  DR cluster with its network policy agent, as at home; the EBS CSI driver
  is added by the runbook after the scale-up, since its controller cannot
  become ready without nodes and an add-on created at zero would leave the
  apply failing on a degraded add-on.
- **Roles.** The DR cluster's issuer is an OIDC provider of its own, and
  every service account with a role at home (irsa.tf) has a DR role,
  `erp-<cell>-dr-<workload>`, trusting that namespace and service account
  of the DR cluster through that provider alone; no role trusts both
  clusters, so a token of one cluster assumes nothing in the other region
  (irsa.tf's rule: each role trusts one service account of one cluster).
  Each may do what its home counterpart does (the tests hold the actions
  equal) on the DR region's counterparts: the bucket's replica, the DR key
  (replica bucket, secrets' replicas, the database replica), the DR
  cluster key, the secrets' replicas, a containers' log group in the DR
  region, target groups of the DR region (none exists there: the router
  and its target groups stay at home). The field key's replica names
  the DR server's role where the key names the home server's. External
  Secrets also reads the master secret RDS makes for the promoted replica
  (`rds!db-*` tagged with the replica's instance ARN) and, as at home,
  neither the deploy key nor the monitoring add-on's secrets; the global
  tier's secrets are not read there. cert-manager's issuer issues from the
  environment's one subordinate CA, at home (ACM Private CA is regional).
- **Values.** The cell module's `pilot_light` output (the environment's
  `cell.pilot_light`): the DR cluster, its node group and the size to
  scale it to, the replica's identifier and endpoint, the key of its
  master secret, the bucket's and the field key's replicas, the DR cluster
  key, the log group, the DR roles and the DR region's endpoints: the
  cell's values there (runbook step 5).
- **Argo CD.** Bootstrapped on the DR cluster by the runbook, as ADR-0035's
  bootstrap installs every cell's: from its pinned archive, the deploy key
  from its secret's replica, the project and the root Application. Not a
  Terraform Helm or Kubernetes provider: ADR-0035 gives Terraform no access
  to a cluster's API (private, reached by operators and the cell's own
  Argo CD), the roots cannot plan while the home region is down, and the
  DR cluster has no nodes to run Argo CD on until the failover.

Confirmed on AWS staging before the rehearsal, the steps marked in the
runbook and these assumptions: a replica secret's ARN is its primary's
with the region changed; RDS tags a DB instance's managed secret
`aws:rds:primaryDBInstanceArn`; a node group may be created at zero; the
VPC CNI add-on becomes active without nodes.

Not covered: the environment's global tier (the router's and the web
workspace's load balancers, the directory) is in the home region and VPC,
which the DR VPC is not peered with, so a region failure of the
environment's home cell also takes the global tier, which no decision yet
recovers; and the subordinate CA issues from the home region only.
Raised to the product owner on 2026-10-29 and tracked as its own item
(review:T72j), on which the rehearsal (T41g) waits; until then the
runbook's steps 5, 7 and 8 cannot complete for the home cell (above).
On 2026-10-31 the product owner deferred the decision, the global tier's
recovery and a subordinate CA in the DR region (about USD 400 a month,
ADR-0035 Consequences) among it, to the pre-production checkpoint; the
item and the rehearsal (T41g) wait on it.
