# ADR-0043: Observability

**Status:** accepted (2026-10-19)

## Context
01-requirements §1.3 asks for every request to be traceable and for metrics
on every API operation and every agent, and sets latency and availability
objectives. Cells already expose request counts and latency per operation,
backlog and lag gauges and traces with OTLP export
(`internal/platform/telemetry`), but nothing stores, alerts on or shows them,
the latency buckets miss two of the objectives' thresholds, no business or
agent metric exists, and nothing stops a metric from carrying a tenant id,
which would make a cell's series grow with its customers. T54 closes this.

## Decision
- **Stack.** Each cell runs its own Prometheus, Alertmanager and Grafana,
  self-hosted with the kube-prometheus-stack chart as a cell add-on
  (ADR-0035). Nothing leaves the cell's account but the alerts. Alert
  receivers are decided later; until then the routes end at a placeholder
  receiver. As built (T54d, `deploy/helm/addons/values/kube-prometheus-stack.yaml`):
  Prometheus is an HA pair keeping 15 days (45 GB at most) on 50 GiB gp3
  volumes encrypted with the cluster key, Alertmanager two clustered
  replicas; the add-on asks about 2.3 vCPU and 9.5 GiB of a cell. It
  scrapes every ServiceMonitor of the cell; the erp-cell chart's scrapes
  the server's admin `/metrics` over TLS verified against the
  environment's CA, and a NetworkPolicy admits the add-on's Prometheus to
  that port alone. Alertmanager's configuration is written by an
  ExternalSecret: the routes in the repository, every alert to
  `erp-alerts`, whose definition is the receivers list in
  `erp/<cell>/alertmanager-receivers` (seeded `- name: erp-alerts` and
  `- name: erp-heartbeat`, which notify no one; T54e). A cell
  bootstrapped before T54e adds `- name: erp-heartbeat` to that secret
  before it syncs the add-on: Alertmanager refuses a route to a receiver
  the list does not define. Grafana signs in through the operators' identity
  provider alone (OIDC, its client secret from `erp/<cell>/grafana-oidc`,
  roles from the provider's claims, strictly): no initial admin, no login
  form, no basic or anonymous access, no password in values. The add-on
  reads those two secrets through a SecretStore of its own as its own IRSA
  role (`monitoring`), which reads nothing else and which the platform's
  store and role cannot read.
- **Labels are bounded enumerations.** A metric's labels are only
  `context`, `operation`, `class`, `event`, `status`, `decision`, `kind`,
  `agent_kind`, `alias`, `currency` and the operational ones already there
  (`code`, `gauge`, `store`, `encryption`), each with a ceiling on its
  values. No tenant, company, principal or document id is ever a label:
  per-tenant figures stay in the database, where tenants' reports and the
  operators' tools read them under RLS (ADR-0003).
- **The catalog is enforced.** Every metric is registered through
  `telemetry.Metrics`, which records it. `Metrics.Check` holds them to the
  catalog: names and help pass promlint, every label name is in
  `telemetry.Labels`, and the worst case (every label at its ceiling,
  histograms with every bucket) stays under `telemetry.MaxSeries` (50,000
  series per process). The cell's test runs it over every metric the
  server registers and checks that the operation, class and context
  enumerations fit their ceilings. It runs in the existing test suite, not
  as a CI job of its own.
- **Latency per class.** The request histogram has a bucket at every p95
  and p99 threshold of §1.3 (0.05, 0.15, 0.2, 0.4, 0.5, 1 and 3 s), so a
  latency SLO counts requests within its threshold exactly. Every public
  operation has a latency class: `read` (a GET of one resource), `list`
  (any other GET), `write` (any other method), or `approval` or `report` as
  `x-slo-class` declares in the contract. `erp_operation_info{operation,
  class}` exports the map, and the SLO rules join the histogram on it.

  | Class | Objective (§1.3) | p95 | p99 |
  |---|---|---|---|
  | read | Read a single resource | 50 ms | 150 ms |
  | list | List with filters (one page) | 150 ms | 400 ms |
  | write | Post a journal entry or invoice | 200 ms | 500 ms |
  | approval | Approval decision | 150 ms | 400 ms |
  | report | Trial balance | 1 s | 3 s |

- **Business events.** `erp_events_published_total{context,event}` counts
  each business event once, when the relay's batch is committed: a batch
  rolled back after publishing and relayed again counts once, matching the
  bus's de-duplication (ADR-0007).
- **Business gauges** (T54b) are read on scrape from the database, as the
  backlog gauges are, and aggregate across tenants, unlabelled: supplier
  invoices on hold, and entered over 7 days ago and still parked; payment
  runs proposed and not decided, and failed in the last 24 hours; bank
  files transmitted and not acknowledged (count and oldest age), and
  rejected in the last 24 hours; approval requests pending (count and
  oldest age); normal periods past their end and still open. Terminal
  states count over a day so a gauge falls once failures stop. The worker
  role reads them through column grants (000091), each from a partial
  index of the rows it watches (000092–000095). Bank-file gauges for T44's
  worker statuses and webhook delivery gauges (T50) follow those steps;
  `status` and `kind` are held at 4 values for the model call (T54c), so
  those gauges name labels of their own within the series ceiling.
- **Agent telemetry** (T54c). `agent_kind` is the agent's type
  (`autonomy.Types`), `untyped` for an agent whose profile names none it
  knows, or `none` for a person or service: never a principal id
  (`autonomy.Kinds`, read through `identity.AgentKind`). The model
  gateway counts each call once its ledger row commits:
  `erp_model_tokens_total{agent_kind,alias,kind}` (kind: input, output,
  cache_read, cache_write), `erp_model_cost_micro_total{agent_kind,currency}`
  in millionths of the currency, and
  `erp_model_call_duration_seconds{agent_kind,alias,status}` (succeeded,
  failed, refused; buckets from 0.5 s to 10 min). The cost series is a
  trend, a float that drifts: exact cost stays in the call ledger and
  `getAiUsage` (ADR-0036). Calls refused before the provider are not
  calls and are not counted.
  `erp_agent_proposals_decided_total{agent_kind,decision}` counts the
  approval requests an agent raised that a person approved, rejected, or
  approved after changing its object (`modified`), once the decision
  commits (`reqctx.OnCommit`, run by the unit of work after its commit);
  a withdrawn request is not decided. An owning context whose objects a
  person may change while their request waits says so by implementing
  `approvals.Modifier`; none does yet, because no such change exists
  today. The **override rate** is the share of agent-raised proposals
  that a person rejected, or modified before approving; approved
  unchanged is the only outcome that is not an override. T54e records it
  as:

  ```yaml
  - record: erp:agent_override_rate:ratio_rate1d
    expr: |
      sum by (agent_kind) (rate(erp_agent_proposals_decided_total{decision=~"rejected|modified"}[1d]))
      / sum by (agent_kind) (rate(erp_agent_proposals_decided_total[1d]))
  ```
- **SLOs and alerts** (T54e) are `deploy/helm/erp-cell/rules/erp.rules.yaml`,
  a plain rule file the chart ships as the `erp-slo` PrometheusRule and
  promtool checks and unit-tests (`erp.rules.test.yaml`) in
  `scripts/helm-checks.sh`, at the add-on's Prometheus version, pinned by
  SHA-256 in the deploy workflow. Availability is the share of 5xx and 429
  among requests that count (any other 4xx is the caller's and counts
  neither way); latency is, per class, the share of requests over the
  class's p95 threshold, a 5% budget. Both are recorded over 5m, 30m, 1h,
  2h, 6h, 1d and 3d, and alerted on the multi-window burn rates of the
  99.95% monthly budget: page at 14.4× (1h and 5m) or 6× (6h and 30m),
  ticket at 3× (1d and 2h) or 1× (3d and 6h). Symptom alerts cover what an
  SLO does not see: outbox lag over 5 minutes (page), projector lag over
  15 minutes (ticket: reports add unprojected entries), a partition horizon
  short for 12 hours (ticket) or, for audit, a month from failing
  readiness and, for the ledger, failing it (page; the
  server exports `erp_partition_horizon_audit_months` and
  `_ledger_years`, what readiness checks), a bank file unacknowledged a
  day (ticket), a failed payment run (ticket), a tampered audit chain (page
  at once), a gauge that cannot be read, no server scraped (page), and
  `ErpWatchdog`, the dead man's switch, always firing and routed every
  minute to `erp-heartbeat`, an outside service that pages when it stops
  hearing it (seeded like `erp-alerts` until the receivers are decided).
  Severity is a label (`page`, `ticket`, `heartbeat`); every alert
  goes to `erp-alerts` but `ErpWatchdog`, whose receivers decide how each is delivered.
  cmd/erp-server's `TestRulesUseCatalog` holds the rules to the catalog
  (`Metrics.Series`): only series the cell exports, labels they carry,
  and catalog labels in groupings and annotations.
  **Dashboards** (T54f) are code: plain Grafana JSON in
  `deploy/helm/erp-cell/dashboards/` (no generator), one ConfigMap each
  labelled `grafana_dashboard: "1"`, which the add-on's dashboards sidecar
  watches in every namespace and writes to files Grafana's file provider
  rereads (no reload API, so no admin). The cell overview (traffic, errors,
  latency per class, saturation as 429s and work waiting, outbox and
  projection lag), the SLOs and error budget (burn rates of the recorded
  ratios), one per context (ledger, payables, payments, procurement,
  identity) and agents (tokens, cost, latency, override rate). Each has
  the stable UID `erp-<file>` and names no datasource, so the default (the
  cell's Prometheus) serves it. cmd/erp-server's `TestDashboardsUseCatalog`
  parses every panel's PromQL with Prometheus' own parser (the version
  promtool is pinned at) and refuses a series the cell does not export or
  the rules do not record, a label the series does not carry or the
  catalog does not hold, and a value outside a closed list (contexts,
  classes, statuses, token kinds, decisions), so a panel cannot name a
  metric or label that does not exist.
- **Logs and traces** (T54g): logs to CloudWatch, kept 90 days; traces to
  X-Ray, 5% of requests sampled plus every request that failed.

## Steps
T54a conventions (this ADR, the catalog test, events published, the
buckets, `erp_operation_info`) · T54b business gauges · T54c agent
telemetry · T54d the stack add-on · T54e SLOs and burn-rate alerts with
promtool tests · T54f dashboards as code with PromQL lint · T54g logs and
traces.

## Consequences
- A cell's monitoring keeps working when the cell is isolated and holds no
  other tenant's data; looking across cells means opening each cell's
  Grafana until a global view is needed.
- Operators cannot ask Prometheus which tenant is slow; they find it in the
  logs (tenant and principal on every request line) and traces, or in the
  database. That is the price of bounded series.
- Adding a label means adding it to `telemetry.Labels` with a ceiling,
  which the review of that change judges.
- The catalog's worst case is an estimate, not a measurement: it bounds
  what the code can produce, and the live series count is watched on the
  cell's Prometheus once T54d runs it.
