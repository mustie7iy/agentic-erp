# ADR-0077: Metric watchers, alerts, forecasts and scenarios

**Status:** accepted (T96a, T97a, 2026-11-13; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T96a's and T96b's last commit). It
decides the analytics context's metric watchers and their alerts, the slices
T96a and T96b (`docs/design/plan-breadth-week3.md`, Lane B); T97a and T97b
(forecasts and scenarios of a governed metric) record what they build in it
under "As built". The defaults taken and the questions left are listed at the
end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01 (2026-11-10, breadth first). The tracker's area Analytics
and planning has the placeholders Metric watchers and Anomaly alerts in
workspaces; the plan details them as T96a (a watcher set and evaluated, an
alert raised) and T96b (watchers evaluated after each refresh of the
analytical store, a deviation condition, alerts listed and acknowledged).

What the design already settles:

- The semantic layer answers governed metrics as the caller, from the
  analytical store when it is current and else from the database through the
  API's own reads (ADR-0044); a query names metrics, dimensions and filters,
  never SQL, and its amounts are exact decimals in one currency each.
- Analytics never sums foreign currency (product owner 2026-11-13).
- 04 §4.3: no hard deletes and no `DELETE`; a state changes through a custom
  method. ADR-0007 (outbox), ADR-0008 (the caller's version), ADR-0010
  (permissions, agents), ADR-0011 (audit), ADR-0003 (RLS).
- Deferred work acts as the person who queued it, in a transaction of the
  unit of work (`uow.Job.As`, ADR-0057 §7), and a person no longer active is
  refused (`identity.principal_blocked`, product owner 2026-11-03); the
  ledger's recurring entries act as their template's owner this way, run
  after run (ADR-0056).
- The plan's "Later" for watchers puts a watcher proposed by the analytics
  agent after 1 December.

Nothing settles what a watcher holds, which period it judges, how a
percentage is compared, who sees an alert, or how the worker reads the
figures. A worker reading the API through the in-process loopback as a
person (T95b's `reqctx.WithActing`) is a question with the product owner
(2026-11-13) and is not used here.

## Decision

### 1. A watcher

A watcher (`analytics.metric_watcher`) is its creator's: they name it, and
it names one governed metric of the active model, filters on the metric's
dimensions in the query's form (`AnalyticsFilter`), one company (its code)
or none for every company the creator may read when it is evaluated, a
period grain (`month`: fiscal year and fiscal month; `year`: fiscal year)
and one condition:

- `above` or `below` a value: a decimal of at most 18 digits before the
  point and 6 after; for a money metric with the currency it is in, and
  only figures in that currency are compared with it; for a count or a ratio
  with no currency;
- `change`: the figure differs from the previous period's by more than a
  percentage of the previous period's (a decimal of at most 6 digits before
  the point and 4 after, `25` meaning 25 %), either way;
- `deviation` (T96b): the figure differs from the average of the previous
  `periods` periods (1 to 12) by more than a percentage of that average.

A watcher is checked when it is set, as the query it runs is (422
`analytics.invalid_query`). People set watchers: an agent does not (x-agent
false; a watcher proposed by the agent is "Later"), so the worker never acts
as an agent. A watcher is read, evaluated and removed by its creator alone;
another's is not found (404), as T95a's saved reports are their owner's
(ADR-0076 §2). Removing it (`:remove`, 04 §4.3) keeps it, `removed`, and
its alerts.

### 2. What an evaluation judges

An evaluation runs one semantic query as the creator: the metric, broken
down by company, by currency where the metric has that dimension, by fiscal
year and, for the month grain, fiscal month, under the watcher's filters,
over its company or the creator's companies, from fiscal year Y−2 (month)
or Y−14 (year), Y the database's year, so the previous periods a condition
needs are in the answer. Each company and currency is a series; amounts of
two currencies are never compared or averaged together. The period judged
is the latest period with a figure in the answer, one for every series. A
series without a figure in that period is not judged; `change` needs the
previous period's figure and `deviation` a figure in each of the `periods`
periods before it, and a base of zero judges nothing (no percentage of
zero). Percentages are computed and compared exactly, as rationals; the
percentage an alert shows is rounded half away from zero to two decimals,
and the average to the figure's digits. A result cut at 500 rows refuses
the evaluation (422 `analytics.watcher_too_many_series`).

### 3. An alert

A condition that holds in at least one series raises one alert per watcher
and period (`metric_alert_once`): the period, the companies of the series
that hold, and for each a finding naming the company, the currency, the
figure, what it was compared with (the value, the previous figure or the
average) and the percentage. A second evaluation of that period raises
none and answers the alert already raised. Raising one writes an audit
record and `MetricAlertRaised` in the same transaction (ADR-0007,
ADR-0011). An alert is `open` until a person acknowledges it (T96b).

A metric the active model no longer has fails the watcher at its next
evaluation: `failed` with the reason (`analytics.watcher_metric_removed`),
recorded with an audit record; a failed watcher is not evaluated again.
Any other refusal (a permission the creator lost, a busy server) leaves the
watcher as it was.

### 4. Evaluated after each refresh (T96b)

The extract scheduler, after a tenant's run that read every dataset,
evaluates the tenant's active watchers, each in a transaction of its own as
its creator (`uow.Job.As`), refused for a creator no longer active. It reads
the figures from the analytical store just refreshed, through the reader,
under the creator's company access and permissions, never through the
loopback: a store that cannot answer, or no reader configured, leaves the
watcher for the next refresh or an evaluation through the API. Running it
twice raises no second alert (§3).

### 5. Who sees an alert (T96b)

An alert is listed and read by a caller holding `analytics.alert.read`
whose company access covers every company of the alert and who holds the
permissions the metric's data is read with (kept on the alert when it is
raised), so a figure reaches no one the query would refuse. Acknowledging
(`analytics.alert.acknowledge`, a person) moves an open alert to
`acknowledged` once, at its version, with an audit record.

### 6. Permissions and events

`analytics.watcher.read`, `analytics.watcher.write` (T96a);
`analytics.alert.read`, `analytics.alert.acknowledge` (T96b). Evaluating
needs `analytics.watcher.write` and the query's own permissions. The
analytics context's outbox, which T95b's migration created, carries it; `MetricAlertRaised` names the watcher,
the metric, the period and the companies, never a figure's data beyond
the findings.

### 7. Forecasts of a governed metric (T97a)

`POST /v1/analytics-forecasts` forecasts one governed metric for the
periods after its history by a named method and answers history and
forecast together; nothing is stored, and agents of any autonomy level
may run it (`x-autonomy: read`).

- The history is the metric by fiscal period (`month`, `quarter` or
  `year` of the semantic model's fiscal calendar), filtered by `where`
  and `companies`, run as `runAnalyticsQuery` runs the caller's query
  (`endpoint.answer`): the caller's company access and dataset
  permissions, the active metric definitions. It runs from the first
  period with data through the last with data, which may be the current
  one not yet complete; a period between them without data counts as
  zero.
- `moving_average` forecasts each of `periods_ahead` periods (1 to 36) as
  the average of the history's last `window` periods (1 to 36);
  `same_period_last_year` as the same period a fiscal year before, for at
  most a year's periods ahead. Too little history is 422
  `analytics.forecast_history_short`, naming the periods the method
  needs and those the history has.
- Arithmetic is on minor units (ADR-0005): the window is summed exactly
  and divided once, rounded half away from zero to the metric's digits
  (`money.Divide`): an amount to its currency's, a count to a whole
  number. A ratio is refused (422 `analytics.forecast_ratio`): its
  average is not the ratio of its averages. A history in more than one
  currency is refused as the semantic layer refuses amounts of two
  currencies summed (422 `analytics.invalid_query`; product owner
  2026-11-13: analytics never sums foreign currency), and so is one of
  more periods than a query answers (500 rows), which would project from
  the earliest.
- The answer names the method, the window, the periods it used and says
  it is an arithmetic projection, not a statistical forecast; each
  figure is an exact decimal beside its currency, with the history's and
  the forecast's totals.

## Alternatives

- **The worker reads the database through the loopback as the creator.**
  It would let the walkthrough show the worker without erp-analytics, but it
  is the open question of T95b's scheduled runs; not taken until the product
  owner answers it.
- **The period of today's date.** Needs each company's fiscal calendar
  outside the semantic layer; the latest period with data is what the
  answer itself says.
- **One alert per series.** The plan asks for one alert per watcher and
  period; the series are its findings.

## Consequences

- A watcher evaluated through the API answers from the store or the
  database, as a query does; the worker answers from the store alone, so a
  cell without erp-analytics evaluates watchers only through the API.
- The alert keeps the permissions its data needed when raised; a later
  change of the model's metric does not widen who sees an old alert.

## Defaults chosen (for the product owner's digest)

1. `:remove` in place of `DELETE /v1/metric-watchers/{id}` (04 §4.3).
2. A watcher is its creator's; people alone set them (§1).
3. The period judged is the latest period with data (§2).
4. Percentages are of the base's absolute value, either direction (§1, §2).
5. An alert is seen only with the metric's data permissions besides company
   access (§5).
6. The worker reads the store only, never the loopback (§4).
7. (T97a) A forecast's history runs from the first period with data through
   the last, a period between without data counting as zero; the
   moving average is flat over the periods ahead; same period last year
   reaches at most a year ahead; a history in two currencies is refused.

## Questions

- Whether the worker may read the database as the creator when the store
  cannot answer is the product owner's T95b question (2026-11-13); the
  answer applies here too.

## As built

### T96a Metric watcher (migration 000322)

- Migration 000322: `analytics.metric_watcher` and `analytics.metric_alert` as
  §1 and §3 describe them; the analytics outbox and inbox are the ones
  migration 000320 (T95b, ADR-0076) creates, not created again (an alert keeps the permissions its metric's data is read
  with, for §5); the permissions `analytics.watcher.read` and `.write`.
  Schema tests in `db/metric_watcher_test.go`.
- `internal/analytics/watchers`: `Set`, `Remove` and `Evaluate` with the
  handlers of `POST /v1/metric-watchers`, `GET /v1/metric-watchers`,
  `GET /v1/metric-watchers/{id}`, `POST /v1/metric-watchers/{id}:remove`
  and `POST /v1/metric-watchers/{id}:evaluate`. The port `Queries` is the
  semantic layer's endpoint (`endpoint.Endpoints.ActiveModel`, `Answer`,
  which runs `runAnalyticsQuery`'s path as the caller), wired by
  `app.Watchers`.
- `MetricAlertRaised` in `api/asyncapi.yaml` (channel `analytics`).
- Refusals: a condition without exactly its settings or an unknown grain
  is 422 `analytics.invalid_watcher` naming the field; a watcher no longer
  active is 409 `analytics.watcher_inactive`; an answer cut at 500 rows is
  422 `analytics.watcher_too_many_series`.
- Walkthrough `step_metric_watch` (step 94): Ben watches Maple's spend by
  month in one currency above a value just below the latest month's
  figure; a watcher without the value's currency is refused; evaluated,
  it raises one alert naming the figure and the value; evaluated again, it
  answers the same alert; Olive does not see Ben's watcher.
- Decided by default while building: the defaults 1 to 6 above.

### T96b Alerts raised after each refresh, listed and acknowledged (migration 000323)

- Migration 000323: the condition `deviation` with its `periods` (1 to
  12); an alert's acknowledgement (`acknowledged_at`, `acknowledged_by`,
  `metric_alert_acknowledged`), fixed once acknowledged and changed at the
  next version alone (trigger `metric_alert_fixed`); the permissions
  `analytics.alert.read` and `.acknowledge`. Schema tests in
  `db/metric_watcher_test.go`.
- `internal/analytics/watchers`: the deviation in the evaluation;
  `GetAlert` and `Acknowledge` with the handlers of `GET /v1/metric-alerts`,
  `GET /v1/metric-alerts/{id}` and `POST /v1/metric-alerts/{id}:acknowledge`;
  `Worker.Refreshed`, which evaluates a tenant's active watchers, each in
  a `uow.Job` acting as its creator (`identity.Actors`, refused once not
  active). The ports `People` and `Companies` are wired by `app.Watchers`.
- `extract.Scheduler.Refreshed` is called after each run that published a
  manifest; erp-server sets it to `app.WatchEvaluator`'s worker when
  erp-analytics is configured. That worker's semantic layer has a
  transport that refuses every API read (`storeOnly`), so it answers from
  the store or not at all (§4).
- An alert shows its creator's and acknowledger's names
  (`raised_as_name`, `acknowledged_by_name`, read now): the consumer of the
  list needs them (ADR-0053 consumer classes).
- Budget: `TestAlertListBudget`, a page of 12,000 alerts of 200 watchers
  under 150 ms p95 (01-requirements' list with filters).
- Walkthrough `step_metric_watch` (step 94) extended: a deviation watcher
  on spend against the month before judges nothing, the month before
  having no spend; Carla lists the open alerts and acknowledges Ben's,
  once; Rita, granted the alerts' read but no invoice read, sees no alert
  on spend. The walkthrough runs no erp-analytics, so the worker's
  evaluation after a refresh, and a large invoice raising a deviation
  alert there, are shown by `TestWorker` and `TestWorkerFromTheStore`
  only (a gap reported for the product owner with the T95b question).
- Decided by default while building: an evaluation runs after a refresh
  only when the run published a manifest (its data changed); the alert
  list's visibility uses the caller's usable permissions
  (`authz.Effective`), so a segregation-of-duties bar hides an alert too.
- Review round 1 (T96b): the worker begins its transactions only while
  the tenant is active (`database.BeginActive`; a suspended tenant ends
  the round, logged) and requires the operation's own permissions
  (`analytics.watcher.write`, `analytics.query.run`) of the creator, as
  the API does at its door. Limit recorded: a month-grain query reads
  three fiscal years, about 36 rows a series, so a watcher over every
  company is refused (`analytics.watcher_too_many_series`) beyond about 14
  company-and-currency series; set it on one company then.

### T97a Forecast of a governed metric (no migration)

- `internal/analytics`: `ForecastSpec` (`Check`, `Query`), `NewForecast`
  and `Forecast`, pure arithmetic on a query's result
  (`forecast.go`); `endpoint.RunAnalyticsForecast` runs the history as
  the caller (`answer`) and answers `POST /v1/analytics-forecasts`.
- Walkthrough `step_metric_forecast` (step 93): Carla forecasts Maple's
  revenue for the next three months by the average of its months of
  history; the forecast equals that average, computed from the governed
  query's monthly figures, and the history's total equals theirs; a
  ratio and the same month a year before (with less than a year of
  history) are refused.
- Decided by default while building: §7 and default 7 above. Written in
  ADR-0076 on T97a's lane, whose base lacked ADR-0077, and moved here at
  integration (batch 73), where the plan puts forecasts.
