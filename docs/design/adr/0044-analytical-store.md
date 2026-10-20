# ADR-0044: Analytical store

**Status:** accepted (product owner, 2026-10-19, T55; amended 2026-10-19,
T55b: one pointer row per tenant, the leading ledger by default; T55c:
manifests named by cutoff and digest, `checked_at`, the scheduler; T55d:
`partners` in place of `vendors`, the reader; T55e: the semantic layer on
the store, the ledger filter, the reader's limits)

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
T55b, 000091: the column is added without a default, so no partition is
rewritten, and the default set after; entries posted before it keep NULL,
which a tenant's first run reads and Parquet stores as 0), and an extract's cutoff is the `xmin` of its own snapshot on the
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
and master data (accounts, partners) are re-extracted as snapshots of the
tenant's rows. **One run per tenant extracts every dataset in one
`REPEATABLE READ` transaction**, so the journal's append and the snapshots
(the open items `cleared` is derived from among them) come from the same
database state, and the run writes one manifest naming all of them. A run the standby cancels for a recovery
conflict (`max_standby_streaming_delay`) is retried by the sweep;
`hot_standby_feedback` stays off, so analytics never bloats the primary
(accepted by the product owner, 2026-10-19). The
outbox (ADR-0007) drives timing only: the relay marks a tenant changed
(`analytics.extract_due`) in the transaction that publishes a batch of its
events of a context a dataset reads (the `Relayed` hook), once per batch
and never in the posting transactions, where one row per tenant would be a
lock every writer of the tenant waits on. The scheduler claims a changed
tenant with `FOR UPDATE SKIP LOCKED` at most once per five minutes (the
claim is the run's lease, so a failed or crashed run is retried after it)
and records a finished run's claim time as done; a change marked after the
claim stays due. Marks and claims use the database's clock. A run that
leaves journal entries above its cutoff (an open transaction holds the
horizon) keeps the tenant due. An hourly sweep (T55f) extracts any tenant
whose `checked_at` is older than the freshness target, so a lost event
delays data and never loses it. A reconciliation per tenant and month (rows and the
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
`po_lines`, `ap_invoices`) and dimension datasets (`accounts`, `partners`,
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
`analytics/v1/tenant=<tenant-id>/<dataset>/month=YYYY-MM/part-<from>-<to>.parquet`,
each file written once through `blobstore` and never overwritten. A
per-tenant manifest (`analytics/v1/tenant=<id>/manifest-<seq>.json`)
lists, for each dataset, its files with size and SHA-256, the watermark,
the row count and `as_of`; an append dataset's manifest lists its earlier
runs' files too. A journal part is named by the interval it holds (the previous
watermark and the run's cutoff, each zero-padded to 20 digits), a snapshot
dataset's file by the SHA-256 of its content (`part-<sha256>.parquet`, one
per month of a fact dataset, one for a dimension), and the manifest by the
cutoff and the first 16 hex digits of the SHA-256 of what it lists, as_of
aside (`manifest-<cutoff>-<digest>.json`): two runs with one cutoff can see
different documents, since the snapshot datasets are read as of the whole
snapshot while the journal is read below its horizon. A run repeated with
the same interval and content (its publication lost) writes the same bytes
under the same keys and goes on, publishing the stored manifest's `as_of`;
other bytes under a key stop the run, never overwrite it. An unchanged
month or dimension keeps its file, which a run does not write again. A cutoff below the stored
watermark (the database restored or moved under the store) stops the run
with `ErrBehind` until the tenant's store is rebuilt: appending from the
old watermark would skip entries for good. Table `analytics.snapshot
(tenant_id, manifest_key, watermark, as_of, published_at, checked_at)` holds **one row
per tenant**, under RLS, pointing at the current manifest; a check
constraint ties the manifest's name to the tenant and the watermark. All
of a tenant's datasets are swapped together: a run publishes by updating
that one row, guarded by the `manifest_key` it read (ADR-0008), so a
reader sees an old or a new run, never half of one, and never a fact
dataset from one run with a dimension dataset from another. A run that
lost the race (`ErrMoved`) leaves its files unreferenced and the sweep's
next run starts from the newer pointer. A run that finds nothing new
(no journal line, every snapshot file unchanged) writes no file and no
manifest and records only its snapshot's time as the pointer's
`checked_at`, so running it twice leaves the store as one run did and a
quiet tenant is not taken for a stale one. `checked_at` is the snapshot
time of the last run that read every dataset and left no journal entry
above its watermark: a run with entries pending above its cutoff
publishes what it read but does not advance it. The pointer is derived bookkeeping, rebuilt from the
database, not a business state change: it writes no audit record
(ADR-0011), and the manifest it names is the record of what was published. The reader takes one manifest per tenant
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
rounded by `money.Scale`. **Metrics read the leading ledger by default,
and another ledger only when the question names it** (product owner,
2026-10-19). That is a semantic-layer change, made in both paths at once in
T55e; the datasets keep every ledger's lines and their `ledger_id`, so it
needs no re-extract. The freshness target is p95 of
fifteen minutes from posting to `checked_at`; freshness is judged on
`checked_at` and the watermark together (the store holds every journal
entry below the watermark and was found equal to the database at
`checked_at`), not on `as_of`, which a quiet tenant's store keeps from its
last change. When a tenant's snapshot is missing or its `checked_at` older
than the target, the semantic layer answers from PostgreSQL
instead, and every answer's lineage names its source (`store` or
`database`) and `as_of`.

**No new per-PR CI.** The contract, isolation and parity tests run in the
existing test job, which builds `erp-analytics` with cgo as it already
does for `-race`; from T55h the image job that already builds the binaries
builds its image too.

**Extractor (T55b).** `internal/analytics/extract` reads, as the
application role in a read-only `REPEATABLE READ` transaction scoped to
the tenant (`database.Scope.Isolation`), the pointer, the cutoff and the
lines, so all three come from one database state. It reads the ledger's,
the directory's and the partners' tables directly and read-only: it is the
store's copy of what the API returns, held to it by the parity test, not a
second owner of that data. Parquet is written with
`github.com/parquet-go/parquet-go` (pure Go: `erp-server` stays
`CGO_ENABLED=0`). Keys use Hive-style `name=value` segments, which
`blobstore` accepts (`=` is percent-encoded in the signed S3 path).

**Datasets and scheduler (T55c).** The run reads the five snapshot
datasets in the journal's `REPEATABLE READ` transaction, as the semantic
layer's `BuildFacts` derives them: order items valued with `money.Scale` of
the decimal quantity (a value out of range stops the run), invoices at
gross in any status, supplier open items as `/v1/open-items` returns them,
accounts per company assignment with `vendor_recon` for supplier
reconciliation accounts, and every business partner, as
`/v1/business-partners` lists them, with `supplier` and `customer` flags
(T55d: a journal line names a customer as its partner too, and
`BuildFacts` gives it the partner's name; contract v1 was unmerged, so
`vendors` became `partners` in place). A flag's role in the contract is
`attribute`: the reader filters and groups by it, and no semantic
dimension names it. Company
and dataset stay keys, so the reader can apply the principal's company
access and fact types. Bank accounts and tax identifiers, which the API
masks, are never read. Every dataset streams in month order, a month's
file written when the next month begins, so a first run over any backlog
holds one month in memory. The scheduler is `extract.Scheduler`
(`erp_worker` claims; the extract itself runs as `erp_app` under the
tenant's RLS); the server starts it, and sets the relay's hook, with the
reader in T55e. Until the run waits for the replica to replay the WAL
position recorded at its claim (`pg_current_wal_lsn()` on the primary
against `pg_last_wal_replay_lsn()`, T55f), the scheduler reads on the
primary: a lagging replica would find nothing new, record `checked_at` and
mark the change done, and the entry would wait for the tenant's next
change.

**Reader (T55d).** `internal/analytics/reader`, served by
`cmd/erp-analytics` on one mTLS listener (TLS 1.3, a client certificate
required, `RequirePeer` admitting `spiffe://<domain>/<cell>/erp-server`
alone; TLS material, trust domain and cell are required, there is no
plaintext mode). `POST /v1/query` takes the tenant, the manifest key
erp-server read from the tenant's pointer under RLS, the principal's scope
(every company or the ones listed, and the fact datasets of its fact types)
and a plan: a fact dataset, `sum` (of the amount, grouped by currency and
exponent) or `count_distinct` (of the document id), the columns to group by
and filters (`in`, `not_in` on any of them; `>=`, `<=`, `between` on the
date). Names resolve only to the contract's dimension and date columns of
the fact and the dimension and attribute columns of the datasets it joins
(the contract is embedded from `analytics/datasets`); values are
parameters. The guard accepts a manifest key only as
`analytics/v1/tenant=<id>/manifest-<cutoff>[-<digest>].json` and a file only
as `analytics/v1/tenant=<id>/<dataset>/[month=YYYY-MM/]part-<interval or
sha256>.parquet` (anchored patterns: no `..`, glob, scheme or second
tenant), and a manifest only when its `tenant_id` is the request's and its
datasets share one `as_of`. Each file is fetched into a local cache named by
its SHA-256 and used only when its size and digest match the manifest's.
Each query opens a fresh in-memory DuckDB with extension autoload and
autoinstall off, writes the tenant and the scope's companies to temporary
tables (parameters, not SQL text), sets `allowed_paths` to the plan's
files, `enable_external_access=false` and `lock_configuration=true`, and
only then creates one temporary view per dataset the plan reads, selecting
the contract's columns from those files where `tenant_id` is the scope's
and, for a dataset with a company, the company is in the scope's. A row
whose exponent is not its currency's (`currency.Seeded`), or a sum outside
int64, is refused. A refusal is 422 `analytics.refused`, which the
semantic layer answers from the database (T55e). The reader is in the main
module without a build tag (the test job already builds with cgo for
`-race`); `internal/layout`'s `TestStaticBinaries` fails when `erp-server`,
`erp-router` or `erp` would link DuckDB or the reader. `make build` builds
`bin/erp-analytics` with the host's toolchain; its image (distroless cc on
both platforms, through the ADR-0032 gates) is T55h, since ko does not
cross-compile cgo.

**Semantic layer on the store (T55e).** The plan, scope and rows are a
cgo-free package, `internal/analytics/wire`, which the reader aliases and
erp-server speaks without linking DuckDB. `internal/analytics/store.Source`
runs in a request's unit of work: it reads the tenant's pointer under RLS
(`ReadSnapshot`), with the leading ledger and the freshness rule in the
same statement, and generates one plan per base metric of the query's
measures: the metric's dataset and aggregate, grouped by the query's
breakdown, the metric's filter and the query's as `where`, the model's
fiscal year start month and the ledger. The reader answers each with
pre-aggregated groups, which become `Facts.Aggregated`; `analytics.Execute`
folds them in unfiltered (the reader applied the filters, the companies and
the ledger) and computes derived metrics, ratios, the currency guard,
ordering and limits exactly as it does over the database's facts. A
count's group is counted by the reader (`COUNT(DISTINCT document)` per
group), so a count is never added across groups that share a document.
The reader derives `fiscal_year`, `fiscal_quarter` and `fiscal_month` from
the dataset's date and the plan's start month (the semantic layer's
`fiscal`), takes `>=`, `<=` and `between` on them as integers, and
answers `cleared` as `vendor_recon` and no `open_items` row of the line.

*The ledger.* A plan over a dataset keyed by `ledger_id` must name one
ledger, or the reader refuses it: a sum never covers every ledger by
default. The semantic query's `ledger` is a ledger id in canonical form, empty for the
leading one; the store path takes the leading ledger's id from the
snapshot read, the database path from `leading_ledger`, a read-only field
added to the API's `JournalEntry` (additive), so both read the same lines.

*Freshness and fallback.* The store answers when the pointer exists,
`checked_at` is within the target (fifteen minutes) of the database's
clock, no journal entry at or above the watermark was posted longer ago
than the target (one a long transaction committed after the run that set
`checked_at`), and `checked_at` is not before the as-of the caller asks
for. Otherwise, and when a plan cannot be generated, the reader refuses
it (422), is at its cap (503 `analytics.busy`, `Retry-After: 1`), fails or
times out, the query is answered from the database (`analytics.Source`)
and the reason logged. The pointer read failing is an error, not a
fallback. The store path reads `analytics.snapshot`, `ledger.ledger` and
`ledger.journal_entry` read-only, as the extractor does (above). Every
`Result` names its `source` (`store` or `database`) and `as_of` (the
manifest's, or the time of the database read); `data_through` is the
latest date of the datasets the query read.

*Limits.* Each query's DuckDB is set to `threads` (default 2) and
`memory_limit` (default 1GB) before the configuration is locked, and the
reader runs at most `ERP_ANALYTICS_MAX_QUERIES` queries at once (default
4): one beyond the cap is refused at once with `ErrBusy` rather than
queued, so a burst never holds erp-server's requests behind it. Threads,
memory (DuckDB's notation, checked by pattern since it is written into a
`SET`) and the cap come from `ERP_ANALYTICS_THREADS`,
`ERP_ANALYTICS_MEMORY_LIMIT` and `ERP_ANALYTICS_MAX_QUERIES`; a bad value
fails startup.

*Parity.* `internal/analytics/store`'s `TestParity` seeds one tenant
through the API (revenue, expenses and supplier items in both companies,
an item cleared by a reversal, a parallel ledger's entry, a customer,
orders and invoices), extracts it, and requires equal columns, rows,
definitions and errors from both paths for every metric of
`semantic.yaml`, alone, by each dimension it allows and by all of them,
filtered by fiscal period, company and date, in the parallel ledger, for
fiscal years starting in January and in July, for every company and for
one.

*erp-server.* With an object store configured, erp-server starts
`extract.Scheduler` and sets the relay's `Relayed` hook to
`extract.MarkChanged`; the scheduler's extract reads on the primary until
T55f. The semantic layer itself still runs as an API client (`erp
analytics`); serving it from erp-server, where the store source can read
the pointer and call erp-analytics, is a separate step.

**Steps.** T55a this ADR and the dataset contract; T55b the snapshot
migration, extractor and Parquet writer for journal lines; T55c the other
datasets and the scheduler (migration 000092); T55d `erp-analytics`, the guard and the
isolation tests; T55h the `erp-analytics` image; T55e the semantic layer on the store, the fallback,
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
