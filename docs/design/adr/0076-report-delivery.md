# ADR-0076: Report delivery

**Status:** accepted (T95a, T95b, T95c, 2026-11-13; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on the step's last commit). It decides
how saved analytics reports are kept, run, scheduled and delivered, for the
slices T95a to T95d (`docs/design/plan-breadth-week3.md`, Lane B), each of
which records what it built under "As built". The defaults taken are listed
at the end for the product owner's digest.

## Context

The analytics agent (ADR-0044) answers a question with a dashboard
specification whose tiles, charts and tables name the results of governed
semantic queries; `erp ask` writes the specification and its queries to a
local file, and `erp report` reruns that file (`Session.Rerun`), removing
commentary its new figures no longer support. Nothing was kept on the
server: `analytics.report` (migration 000010) had no writer.

The product owner asked for a first usable version of every capability by
2026-12-01 (2026-11-10, breadth first). The plan details report delivery
as four slices: a report saved through the API and run again (T95a), its
scheduled runs kept with their results (T95b), sharing with each viewer's
own access (T95c) and an Excel workbook (T95d).

What the design already settles:

- The semantic layer answers as the caller: the companies they may read,
  the datasets their permissions read, from the analytical store or
  through the API's own reads (ADR-0044). A saved report must not read
  more than its runner may.
- 04 §4.3: no `PUT` and no `DELETE`; a state changes through a custom
  method (`POST {id}:verb`) with `If-Match`.
- Deferred work acts as the person who started it and is refused once
  that person is no longer active (product owner, 2026-11-03;
  `identity.Actors.MayAct`).
- Files kept for a tenant are written once to object storage
  (ADR-0016), named under the tenant and recorded with their SHA-256, as
  cash exports and e-invoices are; without object storage what needs it
  is refused 503.
- Week 3's rules (product owner, 2026-11-12): events go to the context's
  outbox and none is a webhook event until a consumer asks.

## Decision

### 1. A saved report

A saved report is a row of `analytics.report`: its specification (the
analytics agent's `Spec`: title, question, assumptions, tiles, charts,
tables, commentary, follow-ups) and its queries (governed semantic
queries without scope: measures, dimensions, filters, order, limit,
ledger) under the ids the specification names them by, its owner and its
status. The title and question are the specification's, kept beside it
for the list. A query carries no companies, `as_of` or configuration
version: a run is over the runner's companies, on current data, under the
active metric definitions.

A report is the analytics context's (`internal/analytics/reports`); its
queries run through the semantic layer's endpoint
(`endpoint.Endpoints.Runner`), so a report reads exactly what
`runAnalyticsQuery` would read for the same person. It reads principals'
names through a port of its own (`People`).

### 2. Saving, reading and running (T95a)

- `POST /v1/reports` saves a report as the caller's, `active`. It is
  checked as the agent's dashboard is built: every query is run once as
  the caller and the specification validated against the results
  (`Spec.Validate`, through `Session.Rerun`), so a tile, chart or table
  naming a query the report lacks, a column a result lacks, or commentary
  citing a figure no result supports is 422 `analytics.invalid_report`
  naming each problem; a query the semantic layer refuses is refused with
  its problem. Nothing of that run is kept.
- A query naming a metric the active model does not have is 422
  `analytics.report_metric_removed` naming the query and the metric, at
  save and at every run, before anything runs: a metric removed by a
  change request since the report was saved.
- A report is read, listed and run by its owner alone until sharing
  (T95c): another's is not found (404), as one that does not exist.
- `POST /v1/reports/{id}:run` runs an active report on current data as
  the caller (`Session.Rerun`) and answers the specification with every
  result; each result's `query_id` is its id in this run, which the
  specification answered names. Commentary the new results no longer
  support is left out with an assumption saying so, as `erp report` does;
  a specification the results no longer support is 422
  `analytics.invalid_report`. A run changes nothing (`x-autonomy: read`);
  `If-Match` is the report's ETag, which a run does not move.
- There is no `DELETE` (04 §4.3): `POST /v1/reports/{id}:remove` moves an
  active report to `removed` once (409 `analytics.report_removed` after),
  at its version. A removed report is kept and read, changes no more
  (trigger `report_removed_fixed`) and is not run.
- Saving and removing are audited (`analytics.report.create`, `.remove`);
  a report has no event: nothing outside the context reads it.

### 3. Permissions and agents

`analytics.report.read` reads and runs one's reports, with
`analytics.query.run` beside it to run them; `analytics.report.write`
saves and removes them, with `analytics.query.run` beside it to save. An
agent may hold them: its reports are its own and run with its own access;
saving and removing are writes (autonomy level 2), running a read.

### 4. Schedules (T95b)

A schedule is part of the report: a frequency (`daily`, `weekly` on an
ISO weekday, `monthly` on a day of the month from 1 to 28, so every month
has it), the company whose time zone dates the runs, and the day it was
set from, that company's date when it was set. `POST
/v1/reports/{id}:schedule` sets or replaces it and `POST
/v1/reports/{id}:unschedule` ends it, each at the report's version
(04 §4.3: no `PUT` or `DELETE` as the plan named them). Without object
storage a schedule is refused (503), as a cash export is: its runs could
not be kept. Removing a report ends its schedule.

A run is due on the company's date in its time zone on a day the
frequency names, from the day the schedule was set; a day the worker did
not run on is not made up (a run is for the day it runs, never a past
one).

### 5. Scheduled runs (T95b)

The worker's round finds the active reports with a schedule across
tenants (`erp_worker`), and runs each one due in a transaction of its
tenant as the report's owner (`uow.Act`), under a transaction-scoped
advisory lock of the report, so replicas never run one report at once:

- The run is authorized as the owner's own `:run` would be: an owner
  no longer active is refused as their request would be
  (`identity.Actors.MayAct`, 403 `identity.principal_blocked`), an
  agent owner only while the tenant's AI kill switch admits agents
  (`agents.Admit`), and the owner must hold `analytics.report.read` and
  `analytics.query.run` with no segregation-of-duties rule barring them
  (`authz.Require`, 403). A refusal is recorded as the run's failure
  with its reason.
- The queries run as the owner with the owner's access and permissions
  that day, as §2's run does. A query the semantic layer or the API
  refuses (a problem below 500) fails the run, recorded with the
  problem's code and detail; the schedule stays. Any other failure (the
  database, a busy server) leaves the day due for the next round.
- A run that succeeds keeps its specification and results as one JSON
  object written once to object storage under the tenant
  (`report-runs/<run id>.json`), recorded with its SHA-256, which every
  read compares (409 `analytics.report_run_changed` when the bytes differ).
- A run is recorded once per report and day (`report_run_once`): a second
  worker, or a second round, finds the day's run and makes none.
- The results are written before the run's row: a round that fails
  after the write (its transaction lost) leaves an object no row names,
  under the run's own id, which a later round never reuses; no row or
  event is ever without its object.
- Each run is audited (`analytics.report_run.complete`) and published as
  `ReportRunCompleted` with its outcome, from the analytics context's
  outbox; it is no webhook event.

The database path of the semantic layer reads the API in-process as its
caller (ADR-0044); a worker has no request. The loopback serves an
in-process read as a principal the worker acts as (`reqctx.WithActing`):
the unit of work resolves that principal by id, refuses one no longer
active or of a tenant no longer active, admits an agent through the AI
kill switch, and asks no token, session or second factor, as `uow.Act`
asks none of deferred work. Only code in the process can set it, and the
loopback serves reads alone.

### 6. Reading kept runs (T95b)

`GET /v1/report-runs` lists the runs of the caller's reports, the latest
day first, filtered by report and status; `GET /v1/report-runs/{id}`
answers a run with its specification and results, or one result as CSV
(`format=csv`, the result named by `query_id` when the run has more than
one). The recipients of a run are the report's owner until sharing
(T95c).

### 7. Sharing (T95c)

The owner shares a report with a person or a role
(`POST /v1/reports/{id}/shares`); a role's share reaches the people who
hold the role on the day they read. A share reaches people alone: a share
with an agent or a service principal is refused, and an agent holding a
shared role reads nothing through it.

- A viewer reads and runs the report and reads its kept runs; they do not
  change, schedule, remove or share it (403 `analytics.report_not_owner`),
  nor list its shares. A run by a viewer is §2's run as the viewer: their
  company access and dataset permissions, never the owner's.
- A kept run names the companies it covered: the owner's company access
  when it ran, by code (all the tenant's companies for access to all);
  and the permissions its queries read their data with
  (`endpoint.Permissions`, as the store asks of a reader: the datasets',
  `tenancy.org.read` and the names' it shows, `partner.supplier.read`
  among them); a failed run names none of either. A run is listed to a
  reader whose company access today covers every company and who holds
  every permission, and read by one who may also use each (no
  segregation-of-duties rule barring it; 403
  `analytics.report_run_access` otherwise), its owner included, so a kept
  run shows no viewer more than their own run would; a run kept before
  T95c names neither and is its owner's alone.
- A share with a role reaches the role's holders while the role is
  active, as authz grants a role's permissions only then.
- There is no `DELETE` (04 §4.3): `POST
  /v1/reports/{id}/shares/{share_id}:end` ends a share once, at the
  share's version, and from that moment it reaches no one; an ended share
  is kept. A report is shared with one person or role once at a time
  (`report_share_once`).
- Sharing and ending a share are human-only: they grant access, as
  identity administration does. Each is audited
  (`analytics.report_share.create`, `.end`); a share has no event.

### 8. Excel workbook (T95d)

A query's result (`POST /v1/analytics-queries?format=xlsx`) and a kept
run's result (`GET /v1/report-runs/{id}?format=xlsx`, the result named as
for CSV) are answered as an Excel workbook, the media type
`application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`;
`erp query -xlsx F` writes one too. The workbook is the minimal
SpreadsheetML package of `docs/design/specs/xlsx-minimal.md` (ECMA-376,
Transitional), written with the standard library's `archive/zip` alone
(no new dependency):

- one sheet (`Query`, or `Report` for a run); a header row of the
  columns' names, a money measure followed by its currency's column
  (`<name>_currency`), as the run's CSV lays it out (`analytics.Records`
  writes both, so the CSV and the workbook cannot differ), then a row per
  result row;
- a measure is a number cell whose value is its exact decimal, written
  from its units and scale with no float in between, styled with a number
  format of its scale's decimals (`#,##0`, `#,##0.00`, or a custom
  `#,##0.000…` from id 164); a measure that cannot be computed is no
  cell;
- every other cell is an inline string, escaped as the standard's
  `ST_Xstring` before XML (a character XML cannot carry and a carriage
  return as `_xHHHH_`, text shaped like an escape with its underscore
  escaped); no cell has a formula, so text starting with `=`, `+`, `-` or
  `@` stays text (xlsx-minimal §8).
- a result cut at the rows a query answers (`truncated`) is refused as a
  workbook (422 `analytics.result_truncated`): the workbook holds the rows
  alone and could not say they were cut; its JSON says so.

A query is a POST with an `Idempotency-Key`: its workbook is kept with
the key as bytes (`platform.idempotency_key.response_bytes`, ADR-0008),
since only JSON answers were kept before, and a replay answers the same
bytes.

## Alternatives

- Keep reports as files the CLI writes (as before): nothing a schedule,
  a share or another client could use.
- Store the results of every `:run`: a run on demand is the caller's
  view now, and keeping it would keep data the caller's access may later
  no longer cover; only scheduled runs are kept, for their recipients.
- Run scheduled reports from the analytical store alone, as a queued
  configuration simulation does: without a current store every run would
  fail, and the store is optional (ADR-0044).
- Mint a token for the owner in the worker: tokens are an identity
  provider's, and a minted one would pass the session and second-factor
  checks a person's own token must.

## Consequences

- A report runs the same queries for whoever runs it; what it shows
  follows the runner's access, not the saver's.
- A change request that removes a metric breaks the reports naming it,
  which say so by name; they are saved again without it.
- The unit of work has a second way in, the in-process acting principal,
  which only reads and only workers set.

## Defaults chosen (for the product owner's digest)

1. A report's queries carry no company scope: a run is over every company
   its runner may read.
2. Saving runs the report once as the saver and refuses one its results
   do not support, commentary included.
3. Removal (`:remove`) instead of `DELETE`; a removed report is kept.
4. The schedule's frequencies: daily, weekly on a weekday, monthly on a
   day from 1 to 28; runs are dated in a company's time zone the schedule
   names; missed days are not made up.
5. Scheduled runs are kept in object storage, and only they.
6. Shares reach people alone, through a person or a role held on the day
   of reading; a viewer reads and runs but does not change or share; a
   kept run is read by whoever's company access covers the companies it
   names, its owner included; sharing is human-only.

## As built

### T95a Saved report through the API (migration 000319)

- Migration 000319: `status` and the checks of §1-§2 on
  `analytics.report`, the index of an owner's reports by title, the
  trigger `report_removed_fixed`, an update grant of the status and
  version alone, and the permissions `analytics.report.read` and
  `.write`. Schema tests in `db/reports_test.go`.
- `internal/analytics/reports`: `Save`, `Get`, `Remove`, `Run` and `List`
  with the handlers of `POST /v1/reports`, `GET /v1/reports`,
  `GET /v1/reports/{id}`, `POST /v1/reports/{id}:remove` and
  `POST /v1/reports/{id}:run`; wired by `app.Reports` with the port
  `People` on `identity.People`.
- `endpoint.Endpoints.Runner` runs a query as `runAnalyticsQuery` does
  (`answer`); `analytics.SpecError` and `analytics.RemovedMetric` name
  the problems.
- `erp report -save` saves a report file in erp-server; `erp report -id`
  with erp-server's id (a UUID) runs it there and writes its dashboard;
  `-id` with `erp ask`'s id reruns the local file as before.
- Walkthrough `step_saved_report` (step 92): Carla saves step 14's
  revenue by company as a report; one naming a query it lacks and one
  naming an unknown metric are refused; she runs it, the billing system
  posts a sale of CAD 100.00, and the run shows revenue 100.00 higher;
  Ben neither lists nor finds it; the CLI saves and runs it, and a removed
  report is not run.
- Decided by default while building: §2-§3 and defaults 1-3 above.

### T95b Scheduled runs of a saved report (migration 000320)

- Migration 000320: the analytics context's outbox and inbox; the
  schedule's columns on `analytics.report` (`report_schedule_whole`,
  `report_removed_unscheduled` and a check per column), their update
  grant, and `erp_worker`'s read of the scheduled reports;
  `analytics.report_run` as §5 describes it (`report_run_outcome`,
  `report_run_once`, insert-only). Schema tests in `db/reports_test.go`.
- `internal/analytics/reports`: `SetSchedule` and `Unschedule` with the
  handlers of `POST /v1/reports/{id}:schedule` and `:unschedule`;
  `Scheduler`, the worker's round, wired by `app.ReportScheduler` and
  started by erp-server beside the analytical store's extract when it has
  object storage; `Runs` and `GetRun` with the handlers of
  `GET /v1/report-runs` and `GET /v1/report-runs/{id}` (`format=csv`).
  Removing a report ends its schedule. The report's row is locked
  `FOR NO KEY UPDATE` for a change, so a run being kept, whose foreign key
  takes `FOR KEY SHARE`, is not held back by it.
- `reqctx.WithActing`, read by `httpapi.Loopback` and the unit of work
  (`uow.Middleware`: `callerOf`, `resolvePrincipal`), as §5 describes;
  `app.Config.Loopback` shares the API's loopback with the worker.
- `ReportRunCompleted` in `api/asyncapi.yaml` (channel `analytics`);
  `analytics` in `outbox.Contexts`; `blobstore.KindReportRuns`.
- The CSV of a run names a money measure's currency in a column of its
  own (`<name>_currency`), so no amount is without it.
- Walkthrough `step_saved_report` (step 92) extended: Carla gives the
  report a daily schedule dated in her company's time zone; a weekly one
  without its weekday is refused; the worker's round runs it once, and
  the run is listed as succeeded and read as CSV with the revenue she saw
  after the sale.
- Decided by default while building: §4-§6 and defaults 4-5 above; the
  in-process acting principal of §5 (security posture, listed for the
  product owner: it asks no token of a worker acting as a report's owner,
  as `uow.Act` asks none of deferred work, and refuses an owner no longer
  active).
- The product owner accepted the in-process acting principal of §5 as
  built: a scheduled run reads the API in-process as the report's owner,
  with no token, and is refused once the owner is no longer active (src
  po:2026-11-13).

### T95c Report shared, each viewer with their own access (migration 000321)

- Migration 000321: `analytics.report_share` as §7 describes it
  (`report_share_grantee`, `report_share_once`, the trigger
  `report_share_ended_fixed`, an update grant of the status, version and
  `updated_by` alone, optimistic locking by `platform.touch`), and
  `analytics.report_run.companies` and `.permissions`
  (`report_run_companies_check`, `report_run_permissions_check`, both or
  neither: `report_run_access_whole`). Schema tests in
  `db/reports_test.go`.
- `internal/analytics/reports`: `CreateShare`, `EndShare` and `Shares`
  with the handlers of `POST /v1/reports/{id}/shares`,
  `POST /v1/reports/{id}/shares/{share_id}:end` (no `DELETE`, 04 §4.3)
  and `GET /v1/reports/{id}/shares`; reads go through `readable` (the
  owner, or a person with an active share to them or a role they hold
  today); `access` (`owner`, `viewer`) on a report; `shared` among
  `listReports`' filters; `companies` on a run. The ports `Grantees`
  (principals' kinds, roles held and roles, on `identity.People` and
  `identity.Directory`, whose `Roles` is new) and `Companies` (the codes
  an access reaches, on `tenancy.Directory.Entities`), wired by
  `app.Reports`.
- Locks: a share is made under the report's row lock (`FOR NO KEY
  UPDATE`), so two shares of one report serialize and the second is
  refused by `report_share_once`; ending a share locks the share's row
  alone, so it never waits for a change of the report
  (`TestShareLocks`).
- Walkthrough `step_saved_report` (step 92) extended: Carla shares her
  scheduled report and one of every company's revenue with Vera, who may
  act in Maple alone; Vera's run shows Maple's rows only, Carla's kept
  run over every company is neither listed nor read by her (403), she may
  not remove or share the report, a share with an agent is refused, and
  once Carla ends the share Vera no longer finds the report.
- A run names every company of its owner's access, even when each of its
  queries filters on one company: stricter than needed, never looser
  (review round 1; narrowing to a shared company filter is left for later).
- Decided by default while building: §7 and default 6 above;
  `:schedule` checks the caller owns the report before it answers 503
  without object storage.

### T95d Excel workbook (migration 000331)

- Contract: `format` (`json`, `xlsx`) on `runAnalyticsQuery` and `xlsx`
  among `getReportRun`'s formats, each with the workbook media type
  beside JSON (and CSV); a format not listed is 422
  `request.validation_failed`; a truncated result is 422
  `analytics.result_truncated` as a workbook (review round 1).
- `internal/analytics`: `WriteXLSX` (§8), `Records` (the layout the
  run's CSV and the workbook share) and `DecodeResult` (a result as the
  endpoint writes it, now read by the run's CSV and workbook too);
  `internal/analytics/endpoint` and `internal/analytics/reports` answer
  the format; `erp query -xlsx`.
- Migration 000331: `platform.idempotency_key.response_bytes` with
  `idempotency_key_one_body` (a stored answer is JSON or bytes, never
  both; `db/idempotency_bytes_test.go`); the unit of work keeps an
  answer whose declared media type is not JSON as bytes and replays them.
- Walkthrough `step_reports` (step 14) extended: Carla downloads the
  months' expenses as a workbook through the API, whose sheet holds the
  CSV's months and amounts, each amount a number beside its currency,
  with no number in a text column and no formula; `step_saved_report`
  (step 92) reads the kept run's result as a workbook too.
- Decided by default while building: the workbook carries each money
  measure's currency column as the run's CSV does, though `erp query
  -csv` (`analytics.WriteCSV`) has none, since a result may mix
  currencies in one column; a ratio is shown as a decimal of four digits,
  not as a percentage; the sheet names are fixed, so no user text needs
  the sheet-name rules; the deflate method `archive/zip` uses by default.
- Not checked: whether Excel, LibreOffice or Numbers opens the file. No
  spreadsheet program is available to the build; the format follows the
  standard's text (xlsx-minimal §11), and the tests read the workbook back
  with their own reader and compare it with the CSV.
