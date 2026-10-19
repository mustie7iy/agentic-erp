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
  receiver.
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
  worker statuses and webhook delivery gauges (T50) follow those steps.
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
- **SLOs and alerts** (T54e) are multi-window burn-rate rules on the
  availability (99.95%) and latency objectives, with promtool unit tests.
  **Dashboards** (T54f) are code: Grafana JSON in the repository, whose
  PromQL is linted against the catalog so a panel cannot name a metric or
  label that does not exist.
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
