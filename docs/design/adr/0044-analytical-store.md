# ADR-0044: Analytical store

**Status:** accepted (product owner, 2026-10-19, T55)

## Context
The semantic layer (`internal/analytics`, `semantic.yaml`) answers every
report and dashboard from fact tables it builds in memory from the API on
each session (`analytics.BuildFacts`). That is right for a pilot tenant and
wrong at scale: every question re-reads every journal line, and a large
tenant's history does not fit one process. 02 §2.7 and 05 §5.6 step 7 named
the answer "a columnar store fed by CDC"; this ADR replaces "CDC" with
something that keeps tenant isolation (ADR-0003) and says what the store
is, how data reaches it, who may read it and how it is kept honest.

## Decision

**Engine.** DuckDB over Parquet files in the cell's object store
(ADR-0016). DuckDB is embedded, reads Parquet by column and by partition,
and needs no server of its own to operate or back up.

**Feed: per-tenant incremental extracts, not CDC.** An extractor reads one
tenant at a time on the cell's read replica, through the unit of work under
that tenant's RLS (`app.tenant_id` set, the application role, never a
`BYPASSRLS` role), and writes only that tenant's rows. Posted rows are
immutable (ADR-0006), so the journal is extracted by watermark and
appended. The watermark is a transaction-id horizon, not a clock: each journal entry
gets `posted_xid xid8 DEFAULT pg_current_xact_id()` (an expand migration in
T55b), and an extract's cutoff is the `xmin` of its own snapshot on the
replica (`pg_snapshot_xmin(pg_current_snapshot())`). Every transaction below
`xmin` has finished, so its rows are visible in that snapshot or were never
committed, however long the posting transaction ran: no timeout has to be
enforced on every writer (an integration import or an operator session
can run for an hour). Parquet stores it as UINT64, compared unsigned. The stored watermark is that cutoff, not the largest
`posted_xid` read, and each append reads the half-open interval
`posted_xid >= previous cutoff AND posted_xid < cutoff`. `posted_at` (the
transaction's start time) was rejected as the watermark: a transaction
that started before the cutoff and committed after it would be skipped for
good. Transaction ids belong to one cluster; the store is rebuilt after a
restore or a move, which resets the watermark (below).

Documents that change after they are written (orders, invoices, open items)
and master data (accounts, vendors) are re-extracted as snapshots of the
tenant's rows. **One run per tenant extracts every dataset in one
`REPEATABLE READ` transaction**, so the journal's append and the snapshots
(the open items `cleared` is derived from among them) come from the same
database state, and the run writes one manifest naming all of them. A run the standby cancels for a recovery
conflict (`max_standby_streaming_delay`) is retried by the sweep;
`hot_standby_feedback` stays off, so analytics never bloats the primary. The
outbox (ADR-0007) drives timing only: a consumer of the
tenant's domain events marks the tenant changed, and the scheduler runs its
next extract after a short debounce; an hourly sweep extracts any tenant
whose snapshot is older than the freshness target, so a lost event delays
data and never loses it. A reconciliation per tenant and month (rows and the
bigint sum of `amount_minor` against the database, like ADR-0030's
fingerprint) runs with the sweep and re-extracts a month that disagrees.

Logical decoding (CDC) was rejected, recorded here so it is not proposed
again:
- one replication slot sees every tenant's rows in the cell, outside RLS:
  the isolation ADR-0003 builds on the database would then rest on
  application code filtering a stream;
- the slot's role bypasses RLS by design (replication reads WAL, not
  tables);
- a stalled consumer holds WAL on the primary until the disk fills, and
  slots do not follow a failover to the replica or the DR region before
  PostgreSQL 17's failover slots are configured on every standby
  (ADR-0040);
- the outbox already carries every state change as a domain event in the
  same transaction (ADR-0007); a second change stream would be a second
  source of truth.

**Separate binary with a recorded cgo exception.** DuckDB is C++ and links
through cgo. It runs in a fourth binary, `erp-analytics`, built with cgo;
`erp-server`, `erp-router` and `erp` stay `CGO_ENABLED=0` and static
(ADR-0032, amended). `erp-analytics` has read-only access to the cell
bucket's `analytics/` prefix, no database credentials and no network path
to the database; it is called by the semantic layer over mTLS with its
SPIFFE identity `spiffe://<domain>/<cell-id>/erp-analytics` and accepts
only `erp-server` (ADR-0033). It receives a tenant's manifest key and the
query, never a tenant id from the request alone.

**Datasets are a contract.** Each dataset has a versioned schema file
(`analytics/datasets/v1/<dataset>.yaml`) shaped like the semantic layer's
facts: a fact dataset per `source` in `semantic.yaml` (`gl_lines`,
`po_lines`, `ap_invoices`) and dimension datasets (`accounts`, `vendors`,
`open_items`) joined at read time, so a rename or a reclassification of an
account needs no re-extract of posted lines. Money is integer minor units
plus currency plus exponent (ADR-0005); the reader refuses a row whose
exponent disagrees with its currency's. Fiscal year, quarter and month are
derived at read time from the dataset's date column and the semantic
layer's calendar, so changing the fiscal calendar needs no re-extract. A
contract test (`internal/analytics/datasets_test.go`) checks that every
semantic source metric's dimensions and filters resolve to a column and
that every dataset dimension maps to `semantic.yaml`. Changes are additive
(a new nullable column); a breaking change (a removed, renamed or retyped
column, a changed grain) is a new dataset version (`v2/`), written next to
`v1` until no reader uses `v1`.

**Layout and publication.**
`analytics/v1/tenant=<tenant-id>/<dataset>/month=YYYY-MM/part-<seq>.parquet`,
each file written once through `blobstore` and never overwritten. A
per-tenant manifest (`analytics/v1/tenant=<id>/manifest-<seq>.json`)
lists, for each dataset, its files with size and SHA-256, the watermark,
the row count and `as_of`. Table `analytics.snapshot (tenant_id, dataset,
manifest_key, watermark, as_of)`, keyed by `(tenant_id, dataset)` and
under RLS, points at the current manifest. A run publishes by updating all
of the tenant's rows to its manifest in one transaction, each guarded by
its previous `manifest_key` (ADR-0008), so a reader sees an old or a new
run, never half of one, and never a fact dataset from one run with a
dimension dataset from another. The reader takes one manifest per tenant
and refuses (falling back to the database) a manifest whose datasets do not
share one `as_of`.

**Storage class.** The files live in the cell bucket under its KMS key.
They are derived data: no object lock, not in the backup plan, superseded
files deleted seven days after their manifest is replaced (so a running
query never loses a file), rebuilt from the database after a restore or a
tenant move (ADR-0029, ADR-0040), and erased with the tenant on offboarding
(ADR-0021).

**Isolation in three layers.**
1. Each query opens a fresh DuckDB connection with
   `enable_external_access=false` after registering only the files it may
   read, and `lock_configuration=true`, so a query cannot open another
   path, load an extension or change a setting.
2. Views are created only over the files listed in the tenant's manifest,
   and only after a path guard accepts each key: it must be under
   `analytics/v1/tenant=<that tenant>/<dataset>/`, have no `..`, glob or
   scheme, and match the manifest's SHA-256.
3. Every view filters `tenant_id = <the tenant>`, so a file that holds
   another tenant's rows still returns none.
Tests run two tenants, a forged manifest pointing at the other tenant's
files, and a fuzzer over the path guard (T55d).

**Permissions.** The semantic layer's principal decides what is read: the
company filter and the fact types (datasets) the user may see are applied
in the view, not after. Fields that the masking policy hides from some
users are never extracted, so the store cannot leak what the API would
mask.

**Correctness and freshness.** A parity test runs every metric over the
same seeded tenant through the store and through the PostgreSQL path
(`BuildFacts`) and requires equal results. Parity holds the extractor to
`BuildFacts`' choices: journal lines of every ledger the API returns (a
`ledger_id` key keeps them apart), local-currency amounts, order values
rounded by `money.Scale`; whether metrics should read the leading ledger
only is a semantic-layer change, made in both paths at once. The freshness target is p95 of
fifteen minutes from posting to `as_of`; when a tenant's snapshot is
missing or older than that, the semantic layer answers from PostgreSQL
instead, and every answer's lineage names its source (`store` or
`database`) and `as_of`.

**No new per-PR CI.** The contract, isolation and parity tests run in the
existing test job; the cgo build of `erp-analytics` runs in the image job
that already builds the binaries.

**Steps.** T55a this ADR and the dataset contract; T55b the snapshot
migration, extractor and Parquet writer for journal lines; T55c the other
datasets and the scheduler; T55d `erp-analytics`, the guard and the
isolation tests; T55e the semantic layer on the store, the fallback,
lineage and parity; T55f erasure, compaction, lifecycle, rebuild and the
freshness alert; T55g metric changes through the configuration approval
loop.

## Alternatives
- **CDC from logical decoding into the store.** Rejected for the reasons
  above.
- **ClickHouse or another column server.** Faster at many concurrent
  tenants, but a stateful cluster to run, back up and isolate per tenant;
  DuckDB over files reuses the bucket, its keys and its erasure.
- **DuckDB inside `erp-server`.** One binary fewer, but cgo in the binary
  that holds database credentials and every tenant's session, and the end
  of the static, reproducible build for it.
- **Materialized views in PostgreSQL.** Keeps one engine, but analytics
  load stays on the cell's database, which is what step 7 exists to
  relieve.

## Consequences
- A fourth image, the only one with cgo; its build and scan follow ADR-0032
  on `gcr.io/distroless/cc-debian12` (glibc) instead of `static`.
- Analytics freshness becomes a service level with an alert (T55f), and
  answers say when they come from the fallback.
- A dataset change is a contract change reviewed like an API change.
- `semantic.yaml` and the dataset files must change together; the contract
  test fails otherwise.
