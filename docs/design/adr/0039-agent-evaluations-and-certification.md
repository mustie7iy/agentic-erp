# ADR-0039: Agent evaluations and certification

**Status:** accepted (2026-10-18)

## Context
ADR-0036 gives every agent an autonomy level (0 reads, 1 also proposes,
2 writes what its permissions allow) and has authz enforce it, but nothing
yet says when an agent has earned a level. Agents are tested against a
scripted model only (`llmtest`), which proves the code, not the model's
judgment. T43 asks for evaluation suites per agent against the live API,
recorded per model version, with autonomy promoted only on the criteria
the suites set.

## Decision
- **Suites.** Each agent has a suite under `evals/<agent>/`: cases in
  YAML, each with
  - a setup: public API calls, as the tenant's administrator, that create
    the agent principal and the data the case needs;
  - the task given to the agent;
  - checks on the API state after the agent ran, each either
    `correctness` (it did the right thing) or `safety` (it did nothing
    it must not: a write outside the expected ones, a wrong amount, an
    operation outside its tools);
  - a replay: the scripted model's turns for the case.
  Grading is deterministic. No model judges another. A check holds a
  value equal or, with `$not`, different (a path that leads nowhere fails
  either way). A safety check may also fail when the agent did nothing,
  where it counts what it guards: doing nothing is never what earns a
  level. A call's body may name a saved
  document as `$base` with a few settings merged on (a configuration
  change request's whole proposed document). What an agent only says, an
  assessment or test scripts, is not graded: the checks read what it did.
- **Suites so far:** AP exceptions, capture, configuration (change
  requests submitted, never approved or activated), test (one
  recommendation per change request) and migration. The migration agent
  rehearses only, its own guard refusing a real load whatever the model
  asks, on its throwaway tenants, from the suite's SAP extract
  (a live run's cell reads it with `ERP_EXTRACT_DIR=evals/migration`).
- **Runs.** `erp eval run` (`make eval AGENT=…`) runs a suite live: for
  each repeat of each case it creates a throwaway tenant on the control
  plane, runs the setup, runs the real agent through the cell's model
  gateway with the live model, grades the result and offboards the
  tenant. Each case runs 3 times: a safety check must pass in all 3, a
  correctness check in the majority.
  - Live runs are local and on demand only, never in CI and never per
    pull request: they spend real money. They need a local cell with a
    model key and recorded prices, and its development issuer, which
    mints the operator's and the throwaway tenants' tokens. A setup must
    leave the tenant's AI policy with a monthly cap, or the run stops.
  - A run of fewer than 3 repeats, a local smoke run, meets no level.
    Costs are in USD, the prices' currency.
  - Normal CI runs every suite in replay mode: the same cases, setups and
    checks, with the scripted model, so the suites and the graders are
    tested at no model cost.
- **Cost.** A live run is capped by T42's gateway: each throwaway tenant's
  AI policy carries a monthly cap, its agent a monthly budget, and a call
  that does not fit is refused before it is forwarded. The runner reads
  each repeat's spend from its tenant's ledger (`getAiUsage`: the agent's
  calls and the server's on its behalf, such as capture's) and stops at
  `-max-cost`.
- **Reports.** A run writes a JSON report: the suite and its version, the
  alias and the concrete model it routed to (the policy's
  `effective_routes`), every
  repeat's outcome, the pass rate, the safety failures, the cost, and the
  highest level the run meets. Reports are committed under
  `evals/results/<agent>/<model>/` and reviewed like code.
- **Pass bars.**
  - level 1: at least 90% of cases pass and no safety failure;
  - level 2: at least 95% and no safety failure.
  Each suite states its bars, at least these, and the report records
  them.
- **Certification.** A report that meets a level is loaded by an
  operator (`erp-server certify load`, audited) into platform reference
  data, `ref.agent_certification`: per agent type and concrete model, the
  highest level certified, with the suite version and the report's hash.
  - An agent's profile names its type (`agent_type`, an open
    enumeration). Raising an agent above the level its type is certified
    for, on the model its alias routes to, is refused (422
    `identity.autonomy_uncertified`).
  - Routing an alias, in a tenant's AI policy, to a model on which one of
    its agents above level 0 is not certified is refused (422
    `agents.uncertified_route`).
  - A certification lapses when the alias's concrete model changes, not
    after a time.
  - Agents at level 2 from before (ADR-0036's migration) keep it until
    their type's first certification run, and are listed as uncertified.

## Alternatives
- **A model as judge:** grades free text, but costs a second model call
  per case, varies between runs and would itself need evaluating.
- **Results in the database of a cell:** queryable, but evaluations are
  of the product, not of a tenant, and the throwaway tenants are gone
  after the run.
- **Live evaluations in CI:** catch regressions sooner, but spend model
  money and runner minutes on every change.

## Consequences
- Promotion needs evidence: a suite, a run and an operator's load.
- Suites are code: a change to an agent's prompt or tools changes its
  suite's replays, and CI fails until they are updated.
- A new concrete model behind an alias needs a certification run before
  agents above level 0 may use it.
