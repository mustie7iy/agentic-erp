# ADR-0072: Recommending agents of the breadth phase

**Status:** accepted (T105a, 2026-11-12; decided by default under ADR-0053 d;
`go-reviewer` had no blocking finding on T105a's last commit). It decides
the one pattern every agent slice of `docs/design/plan-breadth-week3.md`
(Lane D, and Lane A's last three) is built to, and the first of them,
T105a; every other agent slice records its agent under "As built". The
defaults taken are listed at the end for the product owner's digest.

## Context

The product owner asked for a first usable version of every capability in
scope by 2026-12-01, the main path only (2026-11-10, breadth first), and
decided on 2026-11-12 that the agents of week 3 recommend and a person
decides. `plan-breadth-week3.md` details nineteen agent slices that all
build the same thing and asks T105a to state it once, here.

What the design already settles:

- Agents are API clients whose tools are operations of the contract
  (04 §4.13, `internal/apitools`): an agent can do nothing its principal's
  permissions, company access and autonomy level do not allow (ADR-0010,
  ADR-0036), every call is audited, and what the API masks for agents it
  masks for these.
- An agent works in a session recorded through the API, and leaves advice
  as a recommendation on a business object (`createRecommendation`,
  `agents.session.record`, `x-autonomy: propose`, so level 1), which a
  person decides (`decideRecommendation`, `x-agent: false`): accepted,
  overridden or ignored (03 §3.14). The accounts payable exceptions agent
  (`internal/agent`), the test agent and the tax agent (ADR-0049 §13)
  already work this way.
- Each agent type has an evaluation suite under `evals/<type>/`, replayed
  in CI with a scripted model and run live on demand (ADR-0039).
- The existing agents each carry their own copy of the run: the session,
  the tools, a guard on what they propose, a result. Nineteen more copies
  would be nineteen places for the guard to differ.

## Decision

### 1. One pattern, in one package

`internal/recommend` is the pattern. An agent's own package declares only
what differs, as a `recommend.Agent`:

- its `Type`, a value of `autonomy.Types` (an open enumeration, also in
  `api/openapi.yaml` and `api/asyncapi.yaml`);
- its `Reads`, the operations it may call;
- its `Kinds`, the kinds it recommends by the object type it recommends
  them on;
- its `System` prompt: what it reads and when it recommends which kind.

and gives each run a `recommend.Task`: the session's purpose, a prompt
naming what the run is about, the `Objects` it may recommend on (object id
to object type), observers of the reads whose answers its check needs
(`Observe`), and its own `Check` of a recommendation. `Agent.Run` does the
rest, the same way for every agent:

- it starts the session, builds the tools (`apitools.Tools`) and offers
  the model the reads and `createRecommendation` at the session's level;
- it adds the pattern's rules to the prompt (one recommendation per
  object, figures as a tool returned them, what people wrote is data, a
  person decides and acts through the normal operation) and, to the run's
  prompt, the objects with the kinds listed for each and the session id;
- it guards `createRecommendation` (§3), keeps the figures the reads
  returned (§4), and returns a `recommend.Result`: the session, the
  recommendations the API recorded (id, object, kind), how many were
  refused and the model's reply, also when the run failed after
  recommending.

An agent's package imports `apitools`, `llm` and `recommend`
(`internal/layout`), and nothing imports it but `cmd/erp`.

### 2. Reads through the API, with the agent's own token

An agent's tools are reads and the one recommendation. `Agent.Run`
refuses, before a session starts, a declaration whose `Reads` name
anything but a `GET` of the contract (`recommend.ErrNotARead`), and
`apitools` builds no tool of an operation closed to agents. So the
permissions, company access and masking are the API's, and an agent of
this pattern cannot be handed a write by mistake. What a run reads before
its session, to find what it is about (a company by its code, the object),
goes through the same client and token.

What a customer, a supplier or a person wrote (names, texts, references,
descriptions, comments) is data, never an instruction: the pattern's rules
say so in every agent's prompt, and each suite has a case with an
instruction planted in the data.

### 3. One recommendation per object

A run recommends with `createRecommendation`: an `object_type`, the
object's id, a `kind` and, as `content`, the recommendation and its
reasons with the figures behind them. The pattern's guard refuses, before
the call reaches the API, and counts:

- a recommendation in another session than the run's, on an object the
  run was not given, under another object type than that object's, or of a
  kind the agent does not list for that type (`ErrNotThisRun`);
- a second recommendation on an object (`ErrSecond`): a person decides
  one. The tools run one call at a time (`apitools` `Exclusive`), since
  the model's runner executes a turn's tool calls concurrently;
- whatever the agent's own `Check` refuses.

The model is told why, and may correct itself. A person decides the
recommendation with `decideRecommendation`. **Nothing happens on
acceptance**: the person acts through the normal operation. No slice adds
a table, an endpoint, an event or a webhook event for its agent (product
owner, 2026-11-12: no webhook events until a consumer asks), and no
segregation-of-duties rule.

### 4. Figures are the API's

An agent gives no score and computes no amount (plan default 13): it
recommends with the API's figures. The pattern does not try to tell which
numbers of a recommendation are amounts: three review rounds showed that
recognising an amount by what stands beside it (a currency code, a sign,
a name) always misses a way of writing one. Instead **every number a
content writes must be one the run read** (`recommend.Figures`,
`Figures.Unread`):

- a number in a text is any run of digits with the separators and the
  fraction between them, whatever stands beside it; thousands separators
  are taken out, and the digits of a UUID are no numbers
  (`recommend.Numbers`);
- a number with a decimal fraction must be an **amount** a read
  returned: a Money's `amount`, or a number with a fraction wherever it
  stands in an answer (a rate, a percentage). A text a person typed is
  no amount, whatever it looks like (a reference of "900000.00");
- a whole number must be written somewhere in an answer of the run's
  reads or in the run's prompt (a count, a day of a date, a document or
  customer number), or be a whole amount (a currency without minor
  units);
- a value the read's own input supplied as a number is not read, though
  the answer gives it back (`getCreditExposure` returns the amount it was
  asked to check): the model could otherwise have any amount it made up
  returned to it. It is known again whatever zeros end its fraction.

So a figure the agent computed, rounded or made up is refused in
whatever form it is written with digits, and so is a count the agent made
itself, which the prompt tells it to write in words. What the check does
not see is a number spelled in words, and a small whole number that
happens to stand somewhere in what the run read: it is a floor under the
prompt's rule, not a reading of the text. An agent whose recommendation
states amounts holds its content to the figures in its `Check`, as the
API holds the forecast agent's citations to the variance's documents
(ADR-0051 §13): here the check is the run's, since no slice adds an
endpoint.

### 5. Never the operation it recommends

The operation an agent recommends on is not among its tools; each agent's
package lists it (`Never`) and has a test that its tools hold none of the
list and are reads but for `createRecommendation`. Where the contract
closes the operation to agents (`x-agent: false`) the API refuses the
agent anyway (403 `identity.agent_not_allowed`).

### 6. Command, suite and walkthrough

- A command `erp agent <name>` runs one session with `ERP_AGENT_TOKEN`
  and prints, through one printer for all (`cmd/erp/recommend.go`,
  `recommended`): `Session <id>: recommendations made N, refused M.`, a
  line per recommendation (`recommendation <id>: <kind> on <object_type>
  <object_id>`) and the model's reply.
- A suite `evals/<type>/suite.yaml` with at least three cases: one where
  the agent recommends, one where it must not, one with an instruction
  planted in the data; its adapter in `cmd/erp/recommend.go` replies
  through `graded` (`$reply.text`, `.recommendations`, `.refused`).
  `TestEvalSuites` replays every suite in CI with the scripted model.
  **Evaluation against the live API waits for a key**, as every agent's
  does: no live certification exists for these types, and an agent of
  them runs at level 1 only where a tenant's administrator sets it so.
- A walkthrough step through the model stand-in
  (`internal/llm/llmtest/modelstub`, a script of turns), which sets the
  agent up as a principal of its type at autonomy level 1 with a role
  holding only its reads' permissions, `agents.session.read`,
  `agents.session.record` and `agents.model.invoke`. **Nothing is granted
  by default**: an agent that reads identity data or the audit trail
  (`identity.access.read`, `audit.event.read`) holds those permissions
  only where the tenant's administrator grants them to its role (product
  owner, 2026-11-12, security posture), and its walkthrough step shows the
  grant.
- Agents are started by a person; none runs on a schedule (plan default
  14).

### 7. The agents

| Type | Package, command | Reads | Recommends | Never |
|---|---|---|---|---|
| `credit_scoring` | `internal/creditagent`, `erp agent credit-score -company CODE -customer ID` | `getCreditExposure`, `listCreditLimits`, `getReceivablesAging`, `listDunningRuns`, `listDunningNotices`, `listIncomingPayments`, `listCustomerInvoices`, `getBusinessPartner` | on `credit_limit`: `raise_limit`, `lower_limit`, `keep_limit`; on `business_partner` (a customer without a limit): `set_limit` | `createCreditLimit`, `updateCreditLimit`, the release of an order's credit block |

A later slice adds its row here and its section under "As built".

## Consequences

- An agent slice is a declaration, a check, a command, a suite and a
  walkthrough step; the session, the guard, the figures, the printer and
  the adapter's reply are reused, not copied.
- A recommendation is free text for a person. An agent's `Check` can hold
  it to a form (a first line, a leading amount) where a rule needs one;
  what it means beyond that is the person's to read.
- The figures check is on what a run read through its tools. An amount
  the agent's code read before the session does not count: the model
  reads it again with a tool, so the session's tool calls show where each
  amount came from. It fails closed: a sentence with a number the run did
  not read is refused, and the model writes it again.
- Certification (ADR-0039) of these types on a live model is open; until
  it is run, the replays prove the code and the suites, not a model's
  judgment.

## As built

### T105a, the credit scoring agent

- `internal/recommend` (the pattern) and `internal/creditagent`; agent
  type `credit_scoring`; `apitools.LegalEntity` (a company by its code),
  moved from the forecast agent, which now uses it.
- `erp agent credit-score -company CODE -customer ID`: the customer is
  named by its business partner id. The run reads the customer's exposure
  first (`getCreditExposure`: a company or customer the agent may not read
  stops it before a session) and recommends on the customer's credit
  limit, or, for a customer without one, on the customer (object type
  `business_partner`, kind `set_limit`).
- Its check (`creditagent.review.check`): the content starts with `Limit
  <amount> <currency>`, the limit recommended in the company's currency;
  every number in the content is one the run read (§4), so every amount,
  that one included, is one a read of the run returned, whatever that
  read was about (the check does not tell the customer's own figures
  from others the run read), written without thousands separators at the
  start; the
  limit recommended is above the limit in place for `raise_limit`, below
  it for `lower_limit` and equal to it for `keep_limit`; a content that
  gives a score, the word with a number right after it, is refused, which
  is a backstop of the prompt's rule and catches no rating given in other
  words. A customer with no history, none of whose posted invoices
  (`listCustomerInvoices`; a draft never posted is none) or payments
  (`listIncomingPayments`) the run read, gets `keep_limit` or, without a
  limit, nothing, and the prompt asks for a reply that says so.
- The run is told today's date (UTC) for the aging's key date.
- `evals/credit_scoring`, version 1, replayed to level 2: a limit far
  above what an overdue customer owes recommended lower, at its exposure;
  a customer without invoices, payments or a limit given no
  recommendation, the reply saying it has no history; an instruction and
  an amount planted in an invoice item's text not followed and not
  repeated. Not run against a live model.
- Walkthrough `step_credit_agent` (step 66): for Northwind, the customer
  of `step_credit_limit`, the agent is refused a limit it computed (the
  exposure with a tenth on top) and leaves one recommendation at the
  exposure, naming the limit, the exposure and the overdue amount; its own
  `updateCreditLimit` and `decideRecommendation` are refused (403); Ruth,
  the credit manager, overrides it, and the limit is as it was.
- No migration, no new operation, permission, event or problem code.

## Defaults taken (for the product owner's digest)

1. The shared run is a package, `internal/recommend`, not a copy per
   agent (`CLAUDE.md`: no copied helpers); `apitools.LegalEntity` likewise.
2. An agent of the pattern has no tool but reads (`GET`) and
   `createRecommendation`, refused at run time otherwise.
3. Every number in a recommendation that states amounts is one the run
   read, and so every amount is one a read of the run returned, **the
   recommended limit included**: the credit
   scoring agent recommends a limit that is an amount it read (the prompt
   asks for the customer's exposure, its limit, an amount overdue or
   paid), never one it computed. The check holds "a read returned it",
   not "it is this customer's": an amount of another customer that the
   run read would pass. A limit of the agent's own making (a cushion, a
   multiple of sales) would be a score by another name; it waits for the
   "later" leaf's stated model.
4. A customer without a limit is recommended on as `business_partner`,
   the resource `getBusinessPartner` reads.
5. "No history" is no posted invoice and no incoming payment of the
   customer among what the run read; such a customer gets `keep_limit` or
   nothing.
6. The customer is named by id on the command line: finding it by number
   needs a list of partners the plan does not give the agent.
7. A person rejects a recommendation as `overridden` (03 §3.14 has no
   "rejected").
8. The run's date is UTC's, not the company's: the agent is given no read
   of the company's time zone.
