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
  - Before forwarding, the gateway reserves the call's maximum cost,
    `max_tokens` plus an input estimate, against both, in
    `agents.spend` read `FOR UPDATE`. After the answer it settles the
    actual cost. Concurrent calls therefore cannot overspend.
  - A call that does not fit is refused with 429
    `agents.budget_exhausted`, with Retry-After set to the next month.
  - An event fires once at 80% and once at 100%.
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
  - Before forwarding, the gateway redacts text blocks deterministically.
    It replaces IBANs, payment card numbers (Luhn) and the tax-identifier
    patterns of the tenant's countries with placeholders, and counts the
    replacements.
    Names and e-mail addresses pass: the work needs them (a supplier's
    name, the address a draft message goes to).
  - Documents (PDF and image blocks) cannot be redacted. They leave only
    while the policy's `documents_to_models` is on, which it is by
    default; a tenant that switches it off has capture refuse with 422
    `agents.documents_withheld`.
  - Prompt and response content is never stored. The tool calls already
    recorded (`agents.tool_call`) are minimized by the same redaction.
- **Audit (ADR-0011, ADR-0026).** Policy changes, kill switch changes,
  budget refusals and threshold crossings are audited, with counts and
  identifiers only and no content or personal data. `model_call` is the
  usage ledger, not the audit trail.
- **Permitted actions and approval points** are recorded here as rules:
  - **Agents** act only through operations open to them (`x-agent`). They
    never decide approvals, identity, keys, periods or payments' release.
    They recommend, and a person records the decision on the
    recommendation.
  - **Autonomy levels.** Level 0 reads. Level 1 also recommends. Level 2
    also performs the contract's agent-permitted writes within their
    configured limits, such as auto-post. Level 3 is reserved.
  - Enforcement of levels 0–2 in authz is a step of its own (T42g).

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
- Usage reporting (cost per agent per month) reads `model_call` and
  `agents.spend`.
