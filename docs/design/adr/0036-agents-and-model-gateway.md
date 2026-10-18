# ADR-0036: Agents and the model gateway

**Status:** accepted (2026-10-16)

## Context
Agents are API clients: they act only through the public API's operations,
under their own principals (ADR-0010). Their tokens are bound to their
client certificates in production (ADR-0033). Today:

- **Permitted actions are enforced.** `x-agent: false` operations refuse
  agents: approvals, identity administration, period unlocks. SoD rules
  apply to agents too. Agents only recommend (`agents.recommendation`), and
  a person decides. Payables posts automatically only on a clean
  three-way match within the company's auto-post limit.
- **What is not enforced:**
  - `identity.agent_profile.autonomy_level` and `monthly_budget` are stored
    but never checked.
  - `model` is free text.
- **Model calls bypass the platform.** The agents (the invoice exceptions,
  analytics and delivery agents, run by `erp agent` and `erp ask`) call
  Anthropic directly with the operator's `ANTHROPIC_API_KEY`. Their
  sessions' tokens and cost are self-reported, and the cost is usually 0.
  Capture's extraction calls the model from inside `erp-server` and sends
  the whole invoice PDF.
- **Nothing records what was sent.** There is no record of what left the
  platform, per tenant or per agent. There is no spend cap, and no way to
  stop a tenant's agents.

## Decision
- **One gateway, in the cell.**
  - Every model call goes through the cell's model gateway: the agents'
    calls, and the server's own (capture). The gateway is
    `agents.Gateway` (`internal/agents`), served at
    `POST /v1/model/v1/messages` (`api/model.yaml`, a hand-served contract
    like the control plane's).
  - It speaks the provider's Messages API, so the agents keep their SDK
    and tool runner and only change the base URL. They authenticate with
    their bearer token (certificate-bound in production), never with a
    provider key.
  - Only the cell holds provider credentials: `ERP_MODEL_API_KEY`, a
    secret reference (ADR-0017). Data stays in the cell's region
    (ADR-0003).
- **Routing.**
  - Callers name a model alias: `erp-default`, `erp-extraction` or
    `erp-analytics`.
  - The tenant's AI policy maps each alias to an allowed concrete model.
    Platform defaults apply when the policy says nothing. A concrete model
    outside the tenant's allow-list is refused.
  - `agent_profile.model` becomes an alias.
- **Metering.**
  - Every call is one row of `agents.model_call`, written by the gateway.
    It records the principal, the session, the alias, the concrete model,
    the input, output and cache tokens, the cost, the outcome and the
    number of redactions. It never records content.
  - Cost is the provider's reported usage times
    `ref.model_price` (versioned, per model).
  - A session's tokens and cost are the sum of its calls. Client-reported
    usage is ignored.
- **Budgets and caps.** These apply per agent per month
  (`agent_profile.monthly_budget`) and per tenant per month (the AI
  policy's cap).
  - Caps are in the prices' currency (USD). An agent budget in another
    currency refuses the agent's calls (422 `agents.budget_currency`)
    rather than leaving them uncapped. The agent's budget is read through
    identity's published API (`identity.AgentBudget`), not its tables.
  - Before forwarding, the gateway reserves what the call may cost
    against both lines: all of `max_tokens` in output plus an input
    estimate (a token per three bytes, at most a million tokens). Each
    line is a row of `agents.spend`, read `FOR UPDATE`, which holds only
    what the line has spent. A reservation is a row of
    `agents.model_reservation`, committed before the call is forwarded.
    After the answer the gateway marks it settled and adds the actual
    cost.
  - A reservation expires after the call's timeout and margin
    (12 minutes). A call whose settlement is lost, for example when the
    server stops mid-call, then frees its line instead of holding it
    until the month ends.
  - The cap holds up to the input estimate's error. Concurrent calls
    cannot spend past it by more than that error; a call that costs more
    than it reserved (tokens its body does not show, such as the
    provider's tool prompt or a page's image) can take its line past the
    cap, and the 100% event announces when it is reached.
  - A call that does not fit is refused with 429
    `agents.budget_exhausted` and is not forwarded. Retry-After is the
    next month when the budget is spent, or 10 seconds when calls in
    flight hold it. Every refused attempt is audited, so an SDK's
    retries are audited each time.
  - `BudgetThresholdReached` fires once a month per line at 80% and once
    at 100%, for an agent's line and for the tenant's.
- **Kill switch.** The AI policy has `enabled`, set by the tenant's
  administrators, and `suspended`, set by operators on the control plane.
  The operator's setting wins. While off:
  - the gateway refuses every call, including capture's: 403
    `agents.ai_disabled`;
  - the unit of work refuses agent principals' API requests too: an
    agent stopped mid-run cannot go on acting with the model answers it
    already has.

  People and services are unaffected.
- **What may leave.**
  - Only what the request carries leaves the cell: prompts, tool
    definitions, and the tool results the agent sends back. Bank accounts
    and national identifiers reach agents already masked by the API.
  - Before forwarding, the gateway redacts the request's text
    deterministically: every block's text and nested content, whatever its
    type, and the input of earlier tool calls the client sends back.
    It replaces IBANs, payment card numbers (Luhn) and the tax-identifier
    patterns of the tenant's countries with placeholders, and counts the
    replacements.
    Names and e-mail addresses pass: the work needs them (a supplier's
    name, the address a draft message goes to).
    IBANs are taken in capitals only, as banks print them: with groups of
    one to four characters, ordinary lower-case words would pass mod 97 by
    chance about one time in 97 (PO, 2026-10-18).
  - Documents (PDF and image blocks) cannot be redacted. They leave only
    while the policy's `documents_to_models` is on, which it is by
    default; while a tenant has it off, the gateway refuses every call
    carrying one, capture's included, with 422 `agents.documents_withheld`.
  - Prompt and response content is never stored. Tool calls
    (`agents.tool_call`) are recorded minimized by the same redaction,
    from T42e on; rows recorded before are not rewritten.
- **Audit (ADR-0011, ADR-0026).** Policy changes, kill switch changes,
  budget refusals and threshold crossings are audited, with counts and
  identifiers only and no content or personal data. `model_call` is the
  usage ledger, not the audit trail.
- **Permitted actions and approval points** are recorded here as rules:
  - **Agents** act only through operations open to them (`x-agent`). They
    never decide approvals, identity, keys, periods or payments' release.
    They recommend, and a person records the decision on the
    recommendation.
  - **Autonomy levels.** Level 0 reads, and calls models. Level 1 also
    proposes: recommendations, change requests and drafts. Level 2 also
    performs the contract's agent-permitted writes within their
    configured limits, such as auto-post. Level 3 is reserved.
  - Authz checks the level on every agent call (T42g, PO 2026-10-18):
    the contract marks the operations that only propose `x-autonomy:
    propose` (sessions, tool-call records, recommendations, change
    requests, drafts); above its level an agent is refused with 403
    `identity.autonomy_insufficient`. An agent without a profile is at 0.
    The agents that existed were raised to level 2 (000074); new agents
    start at 0 and an administrator raises them. Proposing are the
    agent's own sessions and records, change requests (configuration,
    partner data, bank accounts) and drafts that have no effect until
    someone else acts (parked journal entries, requisitions with their
    submit and cancel, draft purchase orders, parked supplier invoices),
    and cancelling an approval request one raised (PO, 2026-10-18).
    Payment run proposals reserve items and stay at level 2, as does
    cancelling a purchase order, which cancels released orders too.
    `x-autonomy: read` marks an operation that changes nothing though it
    runs by POST (the audit chain's verification): every level performs
    it. An agent is offered every tool its permissions allow; those above
    its level answer 403 (filtering them by level is tracked).

## Alternatives
- **A global gateway service:** one place for keys and prices, but tenant
  data would cross regions, and it would be one more tier to run.
- **Our own message schema instead of the provider's:** a cleaner
  contract, but every agent would need its own loop instead of the SDK's
  tool runner, and every new model feature would need a contract change.
- **Budgets checked after the fact:** simpler, but concurrent calls
  overspend, and a runaway loop is caught only at month end.
- **Storing prompts for debugging:** useful, but it would be the largest
  store of personal and commercial data in the platform, with no
  retention story.

## Consequences
- Agents need no provider key. The gateway becomes the cell's dependency
  on the provider, and the provider's outage is the gateway's 503.
- Prices are reference data the platform maintains.
- Costs are exact per call (`money.Micro`, an amendment to ADR-0005) and
  presented in minor units.
- Usage reporting (cost per agent per month, `getAiUsage`) reads
  `model_call`, the ledger; `agents.spend` is what the budgets hold.
