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
  returned: a Money's `amount`, or a JSON number with a fraction under
  any key of an answer (a rate, a percentage). A text a person typed is
  no amount, whatever it looks like (a reference of "900000.00");
- a whole number must be written somewhere in an answer of the run's
  reads or in the run's prompt (a count, a day of a date, a document or
  customer number, a whole amount);
- no number the read's own input wrote, wherever it stands in the input
  (an amount to check, the days of a date, a list of days in one text),
  is read from that read's answer, though the answer gives it back
  (`getCreditExposure` returns the amount it was asked to check): the
  model could otherwise have any number it made up returned to it. An
  amount is known again whatever zeros end its fraction;
- `Figures.Returned` says whether a number was returned as a Money's
  amount. An agent that recommends an amount holds that amount to it,
  since a whole number passes `Unread` by standing anywhere in what the
  run read.

So a number with a fraction that the agent computed, rounded or made up
is refused in whatever form it is written with digits, and so is the
amount an agent recommends when its check asks `Returned`, and a whole
number that stands nowhere in what the run read (a count the agent made
itself, which the prompt tells it to write in words). What the check
does not see: a number spelled in words; and **any whole number, of any
size, that is written in an answer or in the prompt** (a year, a
customer number, a count), which a content may write as though it were
an amount. It is a floor under the prompt's rule, not a reading of the
text. An agent whose recommendation
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
| `access` | `internal/accessagent`, `erp agent access-roles -role CODE -period FROM..TO`, `erp agent access-sod -role CODE [-add P,...]`, `erp agent access-review -period FROM..TO` | `listRoles`, `getRole`, `listRolePermissions`, `listRoleAssignments`, `listPrincipals`, `listAuditEvents`, `listSodRules` | on `role`: `remove_permissions`, `keep` (T106a); `sod_conflict`, `no_conflict` (T106b); on `principal`: `keep_roles`, `remove_roles`, `review` (T106c) | every identity write (granting a permission, changing a rule, ending a role assignment, blocking a principal among them) |

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
  that limit is an amount a read of the run returned as money
  (`Figures.Returned`: as written, without its sign), whatever that read
  was about (the check does not
  tell the customer's own figures from others the run read), written
  without thousands separators; every other number in the content is one
  the run read (§4), so an amount with a fraction is one a read returned,
  while a whole number written elsewhere in what the run read would pass;
  the
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

### T106a, the access agent: roles from actual usage

- `internal/accessagent`; agent type `access`; `apitools.Each` (a list
  read page by page with a visitor, which `apitools.List` is now built
  on), so a run can stop at its limit of events.
- `erp agent access-roles -role CODE|ID -period FROM..TO`: the period's
  days are UTC days, as the audit trail's instants are. Before its
  session the run itself reads the role (`listRoles`), its permissions
  valid on the period's last day (`listRolePermissions`), who held it at
  any time in the period (`listRoleAssignments`) and the period's audit
  events, every page (`listAuditEvents`), up to 100,000 events, beyond
  which it stops and asks for a shorter period: a period's audit events
  do not fit a model's context. The model is given what was found and
  may read further with the same operations and `listPrincipals`,
  `getRole`. A role, a grant or a trail the agent may not read stops the
  run before a session: its role holds `identity.access.read` and
  `audit.event.read` only where the tenant's administrator granted them
  (product owner, 2026-11-12).
- **What "used" means.** The plan's rule is "an audit event of an
  operation that needs the permission, by a holder, in the period". The
  audit trail names actions (`credit.credit_limit.create`), not the
  operation or the permission behind them (ADR-0011), and nothing in the
  contract maps an action to an operation. So the first version tells use
  apart by context, the first segment of a permission's code and of an
  action:
  - a permission **a read needs** (a `GET`, or an operation marked
    `x-autonomy: read`, names it in `x-permission` or
    `x-also-permissions`) leaves no audit event when it is used that
    way: its **use is not recorded**, and it is never listed as unused,
    also when operations that change something need it too
    (`ledger.entry.read`, `cash.forecast.read`). A permission no
    operation of the contract names in those two places falls here as
    well: the approval permissions, which the contract names by the
    object decided (`x-object-permissions`);
  - a permission that only operations changing something need is
    **used** when a holder has an audit event, in the period, in a
    context such an operation is audited in, whatever the action and its
    outcome, and whichever of the holder's roles allowed it: the
    permissions of one context are not told apart. Those contexts are
    the permission's own and that of the `x-permission` of every
    operation that needs it beside its own: an accrual proposal needs
    `ledger.entry.park` and is audited as `closing`'s, so a holder's
    events in closing count as use of the ledger's parking;
  - it is **unused** when no holder has an audit event in any of those
    contexts in the period.
  This errs towards keeping: a permission is recommended for removal
  only when no holder acted in any context its operations are audited
  in. It rests on an operation that changes something leaving an audit
  record in its caller's name that carries the context of the
  operation's `x-permission`. Review found that to hold for the
  contract's operations with one exception: `fetchEInvoices` only starts
  an operation, and what the fetch records it records as the system, so
  a holder who fetches every day shows no event. Its permission,
  `einvoicing.einvoice.manage`, is therefore listed in
  `accessagent.Unaudited` and classed "use not recorded" until the fetch
  audits its request (tracked, review:T106a). Nothing enforces the
  assumption for a new operation: one that audits nothing in its
  caller's name joins that list, or its permission can be recommended
  for removal while it is used.
- Its check (`accessagent.review.check`) holds the recommendation to
  that, computed by the run, not judged by the model:
  `remove_permissions` is admitted only when somebody held the role and
  some permission is unused, and its content's first line is `Remove: `
  and exactly the unused permissions, in the order of their codes; `keep`
  only otherwise (a role nobody held in the period is kept: none of its
  permissions could be used); and where the role has permissions whose
  use is not recorded the content carries the line `Use not recorded: `
  with them. The model adds the reasons: who held the role and what they
  did.
- `evals/access`, version 1, replayed to level 2: a permission in whose
  context no holder acted recommended for removal and named, the read
  permission listed as not recorded; a role whose holder used what it may
  change kept; an instruction in the role's name not followed, the role's
  permissions and assignments as they were. Not run against a live model.
- Walkthrough `step_access_agent` (step 67): Ada gives Ruth a role
  `credit_controller` holding the credit limit permissions and, left
  over, `tax.return.prepare`; the agent's first run is refused before a
  session (403 `identity.permission_missing`), Ada grants its role
  `identity.access.read` and `audit.event.read`, and the agent recommends
  removing `tax.return.prepare`, naming it, with `credit.limit.read` as
  use not recorded; its own `endRolePermission` is refused (403); Ada
  accepts, which changes nothing, and ends the grant herself.
- No migration, no new operation, permission, event or problem code.

### T106b, the access agent: SoD analysis of a role

- A second entry function of the `access` agent, `Agent.Analyse`, as the
  plan says ("a second entry function of the `access` agent"): same type,
  same suite, same principal; `listSodRules` joins its reads, and its
  `Never` list already held `grantRolePermission`, `createSodRule` and
  `endSodRule`.
- `erp agent access-sod -role CODE|ID [-add PERMISSION,...]`: without
  `-add` the role as it stands, else with the permissions granted to it
  from today. Before its session the run reads every role (with its
  status), every role assignment past and to come, the rules
  (`listSodRules`, every one, ended and to come included), the principals,
  and the grants, past and to come, of every role a holder of the role is
  assigned. Any of them the agent may not read stops the run before a
  session.
- **The conflicts are computed, not judged** (`standing.go`), as
  identity's check at a grant computes them (`internal/identity`
  `sodConflict`, which `grantRolePermission` and `createRoleAssignment`
  run): a principal assigned the role at any time breaks a rule when, on
  some day from today on that the rule is in force, it holds both of the
  rule's permissions, each through an assignment of an **active** role
  and a grant of that role, the assignment's and the grant's validity
  both covering that day. So a permission reaches a holder through this
  role or any other of theirs, an assignment or grant that starts later
  counts from its first day, one ended (today included, `valid_to`
  exclusive) does not, an inactive role gives nothing, and periods that do
  not meet, or meet outside the rule's validity, make no conflict. The
  permissions every principal holds whatever its roles cannot be named in
  a rule (`identity.invalid_sod_rule`), and nothing else grants a
  permission. A conflict that stands already (made before the rule, or
  behind the API) is named too, and the prompt says it stands whatever is
  added: identity's check refuses any grant to the role while it stands.
  `TestAnalyseAgreesWithTheGrant` builds each of these cases on the real
  API, analyses, then grants the permission and compares: refused where
  the analysis named a conflict (the refusal's principal and rule on a
  line of it), granted where it named none.
- Its check (`analysis.check`): `sod_conflict` only when the run computed
  a conflict, its content carrying, each on a line of its own, `Rule
  <permission_a> / <permission_b>: <principal name> (<id>), ...` for every
  rule broken (rules in the order of their pairs, principals of their
  ids); `no_conflict` only otherwise; every number of the content one the
  run read (§4). The model words through which roles each holder would
  hold the two permissions and how to avoid it (a permission left out, or
  a split of the role).
- The run's "today" is UTC's, as T105a's (default 8); identity's check
  takes the database's date. They differ only around midnight where the
  database's zone is not UTC.
- `evals/access`, version 2: three cases added (task `run: sod`), each
  ending the tenant administrator's assignment on 2099-01-01 and making
  the rule capture/post from that day, since an administrator holding
  every permission blocks any rule: adding the posting permission to the
  role whose holder captures is a conflict naming the rule and the holder;
  adding capture to a reader's role is none; an instruction in the rule's
  description not followed. Not run against a live model.
- Walkthrough `step_access_sod` (step 68). The plan's story (the payment
  release added to the role that proposes payment runs, "the tenant's SoD
  rule" named) needs a rule the walkthrough's tenant does not have and
  cannot make: its treasurers propose (`finance_staff`) and release
  (`Z_TREASURY_MGR`), and the administrator holds every permission, so
  identity refuses a propose/release rule (409, shown). Decided by default:
  Ada ends her own role's release from tomorrow and makes the rule *who
  administers access does not release payments*
  (`identity.access.manage` / `payments.run.release`) from tomorrow, the
  rule and the way T98b's `step_control_matrix` makes it (ADR-0078): the
  step makes it unless a step before it did, and ends the grant only
  while it is open; the
  agent, asked about adding the release back to `tenant_admin`, names the
  rule and Ada; its own grant is refused (403); Ada's grant is refused by
  identity's check (409 `identity.sod_conflict`) naming the same principal
  and rule; she accepts the recommendation.
- No migration, no new operation, permission, event or problem code.

### T106c, the access agent: access review prepared

- A third entry function of the `access` agent, `Agent.Prepare`, as the
  plan says; object type `principal`. `erp agent access-review -period
  FROM..TO` (UTC days, as T106a's).
- Before its session the run reads what T106b's reads (roles, every
  assignment, the rules, the principals, the grants of the reviewed
  principals' roles), the period's audit events by actor (up to T106a's
  limit), the agent itself (`/v1/me`) and the recommendations on
  principals (`listRecommendations`, `agents.session.read`, which its role
  holds already).
- **Who is reviewed:** every principal whose status is `active`, people,
  agents and services alike (the agent itself included); a blocked or
  inactive one cannot act and is left out.
- **What is computed, per principal** (`review.go`): the active roles it
  holds on the period's last day; each judged by the principal's own
  audit events as T106a judges a role by its holders' (its permissions
  valid that day sorted into unused, used and use not recorded): **used**
  when some permission is used, with the latest instant of the
  principal's events in those contexts ("last used"); **not used** when
  none is used and none is one a read needs (a role granting nothing that
  day included); **not recorded** otherwise; the permissions never used;
  and its segregation-of-duties conflicts as T106b computes them. The
  kind follows: `remove_roles`, naming the roles not used, where there
  are some; else `review`, naming the roles whose use is not recorded,
  where there are some or the principal breaks a rule; else
  `keep_roles`. The first line is `Access review FROM..TO: remove|review|keep
  <roles>` (`keep: no role` for none), with `; segregation of duties:
  <pairs>` whenever it breaks a rule. Its check holds the recommendation
  to that kind and line, and every number to one the run read; the model
  words the reasons. A role used only through reads cannot be told used
  or unused (T106a's "use not recorded") and is never recommended for
  removal: the reviewer decides it (`review`).
- **One per principal and period:** a principal on which the agent itself
  already recommended with a first line naming the same period is left
  out; a second run of a period whose principals are all reviewed starts
  no session and says so (`erp agent access-review` prints the reply).
- The review's sign-off, periodic reviews as a workflow, removing on
  acceptance and reminders are not built (product owner, 2026-11-12;
  the plan's "Later").
- `evals/access`, version 3: three cases added (task `run: review`): a
  role whose permission the clerk never used recommended for removal and
  named; a role the clerk used kept; an instruction in a principal's name
  not followed, nobody blocked and no assignment ended. The replay reads
  each principal's computed kind and line back from the prompt by its
  name (`replay_vars`). Not run against a live model.
- Walkthrough `step_access_review` (step 69): Ada gives Sally a role
  `tax_preparer` she never uses; the agent leaves one recommendation per
  active principal (the stand-in's script reads each kind and line back
  per principal), the access agent itself among them; Sally's is
  `remove_roles` naming `tax_preparer`; a second run adds none; the
  agent's own `endRoleAssignment` is refused (403); Ada accepts and ends
  the assignment herself.
- No migration, no new operation, permission, event or problem code.

## Defaults taken (for the product owner's digest)

1. The shared run is a package, `internal/recommend`, not a copy per
   agent (`CLAUDE.md`: no copied helpers); `apitools.LegalEntity` likewise.
2. An agent of the pattern has no tool but reads (`GET`) and
   `createRecommendation`, refused at run time otherwise.
3. Every number in a recommendation that states amounts is one the run
   read (§4 says what that floor does not see), and **the recommended
   limit is an amount a read of the run returned as money**: the credit
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
9. (T106a) "Used" is told apart by context, since the audit trail names
   actions and not the operation or permission behind them: a permission
   is unused only when no holder has an audit event, in the period, in
   any context the operations that need it are audited in, and a
   permission a read needs (or one the contract names by the object
   decided) is "use not recorded", whatever else needs it. Telling the
   permissions of one context apart needs the audit record to name its
   operation, a change to ADR-0011 that no slice of this plan makes.
10. (T106a) The run reads the usage itself before its session, up to
    100,000 audit events, and the kind and the permissions to remove are
    computed, not judged by the model; a role nobody held in the period
    is kept; a holder is whoever held the role at any time in the period,
    and all the holder's events in the period count, whichever role
    allowed them; the period's days are UTC days; the permissions
    reviewed are those valid on the period's last day, judged over the
    whole period even when granted during it.
11. (T106b) The SoD analysis is the access agent's second entry function
    (the plan's words); its conflicts are computed by the run as
    identity's check at a grant computes them (from today on, active
    roles, every holder of the role whenever assigned, conflicts already
    standing included) and the model only words them; its day is UTC's.
12. (T106b) The walkthrough shows the rule *who administers access does
    not release payments* rather than the plan's propose/release rule,
    which its tenant cannot make while its treasurers hold both (the
    rule T98b's control matrix step chose too); making
    any rule in a provisioned tenant first needs an administrator's grant
    of one of its permissions ended, since `tenant_admin` holds every
    permission (a gap for the product owner, reported, not built).
13. (T106c) The access review covers every active principal (people,
    agents, services) and the active roles each holds on the period's
    last day; a role is "used" by the principal's own audit events as
    T106a judges a role; `remove_roles` names only roles not used whose
    permissions only changes need, `review` covers roles whose use is not
    recorded and any SoD conflict, and a second run of a period leaves out
    the principals the agent already recommended on with that period.
