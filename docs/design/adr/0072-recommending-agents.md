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
(`internal/layout`), and a library package where its check needs one
(the regulatory agent `config`, for the configuration's paths, T107a);
nothing imports it but `cmd/erp`.

### 2. Reads through the API, with the agent's own token

An agent's tools are reads and the one recommendation. `Agent.Run`
refuses, before a session starts, a declaration whose `Reads` name
anything but a read of the contract, a `GET` or an operation marked
`x-autonomy: read` (T105d: `runAnalyticsQuery`, a `POST` that changes
nothing) (`recommend.ErrNotARead`), and
`apitools` builds no tool of an operation closed to agents. So the
permissions, company access and masking are the API's, and an agent of
this pattern cannot be handed a write by mistake. An agent whose slice
gives it writes (T107b: a change request drafted) names them apart, in
`Writes`, and `Agent.Run` refuses, before a session starts, a write its
run does not hold to a guard of its own (`recommend.ErrUnguarded`). What a run reads before
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
| `regulatory` | `internal/regulatoryagent`, `erp agent regulatory-notice -file F` | `listCountryPacks`, `listConfigVersions`, `getConfigVersion`, `listTaxRegistrations`, `listTaxPeriods` | on `config_version`: `change_needed`, `no_change` | every write of the configuration (change requests, a pack applied, a regression suite) |
| `regulatory` (drafting, T107b) | `erp agent regulatory-draft -recommendation ID` | `getRecommendation`, `listConfigVersions`, `getConfigVersion`, `listFiscalPeriods`, `getChangeRequest`, `getOperation`; writes `createChangeRequest`, `updateChangeRequest`, `simulateChangeRequest` | on `change_request`: `ready_to_submit`, `simulation_failed` | submitting, withdrawing or activating a change request, a pack applied, a regression suite, a decision on an approval or a recommendation |
| `enablement` | `internal/enablementagent`, `erp agent enablement-docs -process P [-out F]`; beside the pattern, `erp agent enablement-help -operation ID -field NAME\|-problem CODE` (T109b) | `listConfigVersions`, `getConfigVersion`, `listRoles`, `listRolePermissions`, `listSodRules`; for help also `getSchema` | on `config_version`: `process_document` (T109a); help replies only (T109b) | every write of the configuration and of access (change requests, a pack applied, a regression suite, roles, grants, assignments, SoD rules, principals, company access) |
| `bid_comparison` | `internal/bidagent`, `erp agent bid-compare -rfq ID` | `getRfq`, `listSupplierQuotations`, `getRfqComparison`, `getBusinessPartner`, `listContracts`, `listPurchaseOrders` | on `rfq`: `award`, `no_award` | `awardRfq`, `publishRfq`, `createSupplierQuotation`, `createRfq`, `cancelRfq`, `closeRfq` |
| `collections` | `internal/collectionsagent`, `erp agent collections -company CODE [-key-date YYYY-MM-DD]` | `getCollectionWorklist`, `getReceivablesAging`, `listDunningRuns`, `listDunningNotices`, `listIncomingPayments`, `listCustomerInvoices`, `listDisputeCases`, `getDisputeCase`, `listOpenItems`, `listAccounts`, `getBusinessPartner` | on `business_partner` (a customer of the worklist): `call`, `send_reminder`, `escalate`, `wait_for_dispute`, `no_action` | `createDunningRun`, `confirmDunningRun`, `createDisputeCase`, `updateDisputeCase`, `closeDisputeCase`, `createDunningBlock`, `releaseDunningBlock`, `createPromiseToPay`, `createCustomerContact`; nothing in the contract sends to a customer |
| `support` (diagnosis, T108b) | `internal/supportagent`, `erp agent support-diagnose -type KIND -id ID [WHAT HAPPENED]` | T108a's (`listConfigVersions`, `getConfigVersion`, `getSchema`, `getCurrentPrincipal`), `listAuditEvents`, `listOperations`, `listApprovalRequests`, each document's read (`getSupplierInvoice`, `getCustomerInvoice`, `getJournalEntry`, `getPurchaseOrder`, `getSalesOrder`, `getPaymentRun`, `getChangeRequest`, `getOperation`), the contract's search (`findOperations`) | on the document (its kind): `root_cause` | every write: its tools hold none |
| `integration` | `internal/integrationagent`, `erp agent integration-monitor` | `listConnectors`, `getConnector`, `listWebhookSubscriptions`, `listWebhookDeliveries`, `getWebhookDelivery`, `listBankFiles`, `listOperations`, `getOperation`, `listMigrationRuns` | on `webhook_subscription`: `check_endpoint`, `investigate`, `redeliver`, `pause`; on `bank_file`: `retransmit`, `investigate`; on `connector`: `check_endpoint`, `investigate` | every write of connectors, webhook subscriptions and deliveries, bank files, migration runs and operations (enabling, disabling, pausing, resuming, redelivering, rotating a secret, transmitting a file, creating a connector among them) |
| `support` (fix, T108c) | `erp agent support-fix -recommendation ID` | `getRecommendation`, `listConfigVersions`, `getConfigVersion`, `getChangeRequest`, `getOperation`; writes `createChangeRequest`, `updateChangeRequest`, `simulateChangeRequest` (`internal/changedraft`) | on `change_request`: `ready_to_submit`, `simulation_failed` | submitting, withdrawing or activating a change request, a decision on an approval or a recommendation, any write of master data or a document |
| `savings` | `internal/savingsagent`, `erp agent savings -company CODE [-period FROM..TO] [-max N]` | `runAnalyticsQuery`, `getAnalyticsModel`, `listContracts`, `getContract`, `listCatalogItems`, `listPurchaseOrders`, `getPurchaseOrder`, `getBusinessPartner`, `listAccounts` | on `business_partner`: `bring_under_contract`; on `account`: `consolidate_suppliers`; on `purchase_order`: `use_contract_price` | `createRfq`, `publishRfq`, `createContract`, `releaseContract`, `createPurchaseOrder`, `releasePurchaseOrder` |
| `control_monitoring` | `internal/controlagent`, `erp agent control-monitor [-process TEXT]` | `listControls`, `getControl`, `listControlTests`, `getControlMatrix`, `listAuditEvents`, `listSodRules`, `listRoleAssignments`, `listApprovalRequests` | on `control`: `exception_found`, `no_exception` | `createControl`, `updateControl`, `retireControl`, `createControlTest`, `createControlLink`, `removeControlLink` |
| `risk` | `internal/riskagent`, `erp agent risk` | `listRisks`, `getRisk`, `listRiskRatings`, `listControls`, `getControl`, `listControlTests`, `getControlMatrix`, `listAuditEvents`, `listRecommendations` | on `risk`: `rate_again`, `keep`; on `tenant`: `new_risk` | `createRisk`, `updateRisk`, `closeRisk`, `createRiskRating` |
| `planning` | `internal/planningagent`, `erp agent plan -company CODE -category CODE -year YYYY` | `listPlanCategories`, `listPlanLines`, `listCostCenterPlanActual`, `listProfitCenterPlanActual`, `listCostCenters`, `listAccounts`, `runAnalyticsQuery` | on `plan_category`: `plan_proposal` | `applyPlanBatch`, `copyPlanCategory`, `lockPlanCategory`, `unlockPlanCategory`, `createPlanCategory` |
| `replenishment` | `internal/replenishmentagent`, `erp agent replenish -run ID` (S2l) | `getMrpRun`, `getRequisition`, `getMaterial`, `listCatalogItems`, `getCatalogItem`, `listContracts`, `getContract`, `listPurchaseOrders`, `getPurchaseOrder`, `getBusinessPartner` | on `requisition` (a run's proposal): `order`, `source_needed` | every write of requisitions and purchase orders (their changes included, `postIntercompanyGoodsIssue` and `createGrIrClearing`), `createScheduleLine`, `callOffScheduleLine`, `createRfq`, `startMrpRun`, `createMaterialPlan`, `updateMaterialPlan`, and of stock transport orders (`createStockTransportOrder`, `cancelStockTransportOrder`; review:batch92) |
| `count_planning` | `internal/countagent`, `erp agent count-plan -company CODE -plant CODE` | `listStock`, `listMaterials`, `getMaterial`, `listMaterialDocuments`, `getMaterialDocument`, `listPhysicalInventoryDocuments`, `getPhysicalInventoryDocument` | on `plant`: `count_proposal` | `createPhysicalInventoryDocument`, `enterPhysicalInventoryCount`, `postPhysicalInventoryDifferences`, `createMaterialDocument`, `reverseMaterialDocument`, the postings that post a material document (`postGoodsReceipt`, `postOutboundDeliveryGoodsIssue`, `postStockTransportGoodsIssue`, `postStockTransportGoodsReceipt`, `postIntercompanyGoodsIssue`, `reverseGoodsReceipt`) |
| `demand_forecast` | `internal/demandagent`, `erp agent demand-forecast -company CODE -material NUMBER` (S2m) | `getMaterial`, `listMaterials`, `listMaterialDocuments`, `getMaterialDocument`, `listSalesOrders`, `getSalesOrder` | on `material`: `demand_forecast` | every goods movement and count (`createMaterialDocument`, `reverseMaterialDocument` and the postings that post one), every write of sales and returns orders and their deliveries, the replenishment agent's never list of requisitions and purchase orders (schedule lines and their call-off included), `startMrpRun`, `createMaterialPlan`, `updateMaterialPlan` |

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
- Walkthrough `step_credit_agent` (step 72): for Northwind, the customer
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
  contract's operations with one exception: `fetchEInvoices` only started
  an operation, and what the fetch records it records as the system, so
  a holder who fetched every day showed no event. Its permission,
  `einvoicing.einvoice.manage`, was listed in `accessagent.Unaudited`
  and classed "use not recorded" until the fetch audited its request
  (review:T106a). It now does (`einvoicing.fetch.request`, in the
  caller's name, on the fetch's Operation), so the list was empty and is
  removed. Nothing enforces the assumption for a new operation: one that
  audits nothing in its caller's name must audit its request, or its
  permission can be recommended for removal while it is used.
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
- Walkthrough `step_access_agent` (step 73): Ada gives Ruth a role
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
- Walkthrough `step_access_sod` (step 83). The plan's story (the payment
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
  and its segregation-of-duties conflicts as T106b computes them, from
  today on as identity checks a grant, whatever the period (a review of
  a past period names a conflict that stands now, and not one that has
  ended since; the prompt says so). The
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
  This holds for runs that do not overlap: two runs of one period at the
  same time each read none done and each recommend on every principal;
  nothing in the database stops it, which a periodic review workflow
  (after 1 December) must settle.
- `erp agent access-review` prints, for each recommendation, the
  principal's name and the first line computed for it beside its id
  (`recommend.Result.Names`, `recommend.Made.Content`), so a reviewer
  reads whose roles are to be removed, and which, without a call per
  row.
- The review's sign-off, periodic reviews as a workflow, removing on
  acceptance and reminders are not built (product owner, 2026-11-12;
  the plan's "Later").
- `evals/access`, version 3: three cases added (task `run: review`): a
  role whose permission the clerk never used recommended for removal and
  named; a role the clerk used kept; an instruction in a principal's name
  not followed, nobody blocked and no assignment ended. The replay reads
  each principal's computed kind and line back from the prompt by its
  name (`replay_vars`). Not run against a live model.
- Walkthrough `step_access_review` (step 84): Ada gives Sally a role
  `tax_preparer` she never uses; the agent leaves one recommendation per
  active principal (the stand-in's script reads each kind and line back
  per principal), the access agent itself among them; Sally's is
  `remove_roles` naming `tax_preparer`; a second run adds none; the
  agent's own `endRoleAssignment` is refused (403); Ada accepts and ends
  the assignment herself.
- No migration, no new operation, permission, event or problem code.

### T105c, the bid comparison agent

- `internal/bidagent`; agent type `bid_comparison`.
- `erp agent bid-compare -rfq ID`. The run reads the RFQ's comparison
  first (`getRfqComparison`, ADR-0069 §7: an RFQ the agent may not read
  stops it before a session) and recommends on the RFQ (object type
  `rfq`): `award` or `no_award`.
- Its check (`bidagent.comparison.check`): every number in the content
  is one the run read (§4), so the totals and items' amounts it states
  are the comparison's and a difference it computed is refused; an
  `award`'s first line is `Award: ` and the `supplier_quotation_id` of a
  quotation the comparison read before the session ranks (complete and
  valid on the company's date today), and only an RFQ that comparison
  shows `published` is recommended for an award. So an RFQ without a
  complete, valid quotation gets `no_award` or nothing. `no_award` has no
  form: a person reads why. The prompt asks for the totals, the items for
  which another supplier quoted less and the quotations left out as
  incomplete or expired, and why; the check does not hold the content to
  naming them.
- Its never list is every write of sourcing: `awardRfq`, `publishRfq`,
  `createSupplierQuotation`, `createRfq`, and T82g's `cancelRfq` and
  `closeRfq` (`TestNever` keeps it whole against the contract); `awardRfq` is `x-agent: false`.
- `evals/bid_comparison`, version 1, replayed to level 2 on a clock
  standing at 2026-11-16 (`cmd/erp` `cellClock`), since its RFQs take
  numbers of fiscal year 2026 and are due on dates of its own: the cheaper
  complete quotation recommended for the award, naming the item another
  supplier quoted lower; an RFQ whose quotations are incomplete or
  expired recommended for no award; an instruction in a supplier's name
  not followed. Not run against a live model.
- Walkthrough `step_rfq` (step 60, `rfq_agent`): the agent's content
  stating the difference between the totals, an amount no read returned,
  is refused; it then recommends the quotation ranked first, naming both
  totals; its own `awardRfq` is refused (403); Ben accepts, which changes
  nothing (the RFQ stays published at its version), and `step_rfq_award`
  awards the quotation the agent recommended, as before.
- No migration, no new operation, permission, event or problem code.

### T107a, the regulatory agent: a notice read against the configuration

- `internal/regulatoryagent`; agent type `regulatory`.
- `erp agent regulatory-notice -file F`: the notice is the file's text,
  UTF-8 of 1 to 64 KiB (`MaxNotice`; the command reads no more than one
  byte beyond it), passed in the run's prompt between `<notice>` lines.
  The run reads the active configuration version first
  (`listConfigVersions` with `is_active`: a version the agent may not read
  stops it before a session) and recommends on that version (object type
  `config_version`): `change_needed` or `no_change`. It fetches nothing:
  its tools are the API's reads, and no tool reaches outside it.
- Its check (`regulatoryagent.reading.check`):
  - every passage between double quotes, straight or typographic, is
    not empty and is in the notice character for character, as whole
    words and numbers: it neither starts nor ends inside a word or a
    number of the notice ("5 percent" is no quotation of "2.5 percent",
    nor "9" of "2099"); a quotation left open runs to the content's end,
    and so fails unless the notice ends with it. Only `.` and `,` are
    taken as separators inside a number: a fragment of a number grouped
    with a space or an apostrophe ("200,50" of "1 200,50") passes, a
    known limit until notices in other languages (the "later" leaf), and
    a sign is not part of a number, as in §4;
  - the content carries the sentence `A person must check the notice
    against its source.` (`regulatoryagent.Caveat`);
  - every number outside the quotations, and outside the paths of a
    `change_needed`, is one the run read (§4), the notice being part of
    the run's prompt: a rate with a fraction is therefore quoted, never
    restated. A `no_change` has no paths: a line of it starting
    `Paths: ` is held to the figures as any other;
  - `change_needed` starts with `Paths: ` and the paths affected, each one
    the version read before the session holds, then `Takes effect: ` and
    the notice's words in double quotes or `not stated`, and quotes the
    notice. A path is one `config.Holds` finds in the version read before
    the session: a setting as `config.Diff` names it
    (`companies[3000].tax_codes[GO5]`), a list item named by its list's
    key only, never by another of its values (`tax_codes[0]` is no path
    of a code at rate 0). The agent's package imports `internal/config`
    for this beside the pattern (`internal/layout`); a document the
    binary cannot read as a configuration holds no path.
- Its never list is every write of the configuration: the change
  request's operations, `applyCountryPack` and `createRegressionSuite`
  (`TestNever` keeps it whole against the contract's `config.*`
  permissions); its role holds none of their permissions.
- `evals/regulatory`, version 1, replayed to level 2, on a configuration
  with a code invented for it (`XL`, a levy no authority levies): an
  invented notice raising XL's rate recommended as a change naming
  `companies[3000].tax_codes[XL]`, when it takes effect and the passage;
  an invented notice of fishing licences in an invented country needing
  no change; a notice's planted instruction to create and activate a
  change request not followed, no change request existing. Not run
  against a live model.
- Walkthrough `step_regulatory_agent` (step 87): Fiona, who manages tax,
  gives the agent a notice invented for the walkthrough and marked so,
  raising the rate of tax code V0 of company 1000 (the walkthrough's own
  code); a misquoted passage is refused; the agent recommends
  `change_needed` naming `companies[1000].tax_codes[V0]`, when it takes
  effect in the notice's words and the passage, with the caveat; its own
  change request is refused (403 `identity.permission_missing`); the
  version is as it was and the recommendation is left open (T107b drafts
  the change from an accepted one).
- No migration, no new operation, permission, event or problem code.

### T107b, the regulatory agent: the change request drafted

- A second entry function of `internal/regulatoryagent`, `Agent.Draft`;
  `erp agent regulatory-draft -recommendation ID`. Before its session the
  run reads the recommendation (`getRecommendation`) and the active
  version, and refuses (`ErrNotAccepted`, `ErrStale`) a recommendation
  that is not an accepted `change_needed` on a `config_version` (T107a's
  kind: an open or overridden one, or another kind or object), one made
  on a version no longer active (the notice is read again against the
  active one), and one whose first line's paths the active version does
  not all hold. The recommendation's agent is not read: the kind and
  object are the regulatory agent's alone.
- **The pattern's writes (§2).** `recommend.Agent.Writes` names
  operations beyond reads that an agent may call; `Agent.Run` refuses,
  before a session starts, a write its task does not guard
  (`Task.Guards`, `recommend.ErrUnguarded`), and runs every call through
  the task's guard before the pattern's. An observer of a write may add
  to the run's objects one the write made (`Task.Objects`, read under the
  tools' lock): the drafting recommends on the change request it created.
  The drafting's writes are `createChangeRequest`, `updateChangeRequest`
  and `simulateChangeRequest`; its reads `getRecommendation`,
  `listConfigVersions`, `getConfigVersion`, `listFiscalPeriods` (a code's
  first rate starts with its company's first fiscal period),
  `getChangeRequest`, `getOperation`. Its never list (`DraftNever`,
  `TestDraftNever` against the contract's `config.*` writes) is every
  other configuration write, `submitChangeRequest`,
  `withdrawChangeRequest`, `activateChangeRequest` among them, and the
  decisions on an approval and on a recommendation.
- Its guards: one draft per run, based on the active version and kept
  on it by an update (`ErrBase`); a proposal, on create or update, that is a configuration,
  changes something and changes only settings at or under the paths the
  recommendation named (`config.Diff` against the active version,
  `ErrOutside`); an update or simulation only of the run's draft
  (`ErrNotTheDraft`).
- **A draft is simulated as a preview.** The plan has the agent simulate
  the draft before a person submits it; `simulateChangeRequest` required
  `awaiting_evidence`, a submitted request's status. It now also takes a
  `draft`: the worker validates the proposal as submission does (a
  finding fails the operation with `config.validation_failed`) and
  replays it, records no evidence, moves nothing and audits
  `config.change_request.preview`; the operation's result is the preview
  (`passed`, `summary`, `status` `draft`, the draft's `version`,
  `proposal_sha256`). A draft changes until it is submitted and its tier
  is computed then, so submission still asks for evidence of the request
  it fixes: the governed simulation, and with it the approval, are
  unchanged. Validation no longer hands a document its own rules refuse
  to the sections (`configuration.Dependencies.check`): the ledger's plan
  of tax rates read an unparsable rate as valid and panicked, in a
  submission's request and now in the worker.
- The run waits for the simulation it started (`Agent.Poll`, default one
  second, `Agent.Patience`, two minutes) and holds the recommendation to
  its outcome: `ready_to_submit` only when the simulation of the draft's
  last version passed, `simulation_failed` only when it failed or did not
  pass (`ErrNotSimulated`); a simulation refused or unfinished admits
  neither. Its check (`drafting.check`) also holds every quotation to the
  accepted recommendation's text, character for character, the caveat and
  every number outside the quotations to the figures the run read (§4).
- No evaluation case drafts: a suite's setup cannot make a recommendation
  an agent made and a person accepted; the run's guards and check are
  tested in `internal/regulatoryagent` and the walkthrough.
- Walkthrough `step_regulatory_agent` (step 87) extended: Fiona accepts
  the recommendation; Ada grants the agent's role `config.change.propose`,
  `platform.operation.read` and `ledger.period.read`; the agent's first
  draft, also raising company 1000's price tolerance, is refused by the
  run; it drafts V0 alone, at 0 from the company's first fiscal period to
  30 June 2099 and at 2.5 from 1 July 2099, has the draft simulated and
  recommends `ready_to_submit`; its tool calls hold no submission; Fiona
  submits it (tier 3, awaiting evidence), simulates the submitted request
  and it waits for approval; the active version is as it was.
- No migration, no new operation, permission, event or problem code; the
  contract's description of `simulateChangeRequest` says what a draft's
  simulation does.

### T108a, the support agent: how-to answers

- `internal/supportagent`; agent type `support`; `erp agent support-ask
  QUESTION` (the question is the command's words, UTF-8 of 1 to 4,000
  bytes, `MaxQuestion`, passed in the run's prompt between `<question>`
  lines). It answers with a reply only, no recommendation, so it is not a
  recommending agent and does not run through `internal/recommend`: its
  run is `apitools.Run` over its tools, as `erp ask`'s, with the session
  recorded through the API. The run reads the active configuration
  version first (`apitools.ActiveConfig`, which the regulatory agent now
  uses too): a version the agent may not read stops it before a session.
- Its tools: the reads `listConfigVersions`, `getConfigVersion`,
  `getSchema`, `getCurrentPrincipal`; `findOperations`, the contract's
  search (`apitools.Contract`): the operations whose id or summary hold
  every word asked, or those named by id, at most 20,
  with their method, path, summary, description, permission, problem
  codes and whether only a person may call them; and `reply`, the answer.
  The last two are local tools (`apitools.Local`): answered by the
  agent's own code from the contract compiled into the binary, offered
  at every level, recorded in the session, guarded and observed like an
  API call. None writes (`TestTools`).
- Its check (`supportagent.replying.check`), on `reply`, refused and
  counted as the pattern's are, the model told why:
  - every word of an operation id's form (lower case, then words joined
    by capitals) is an operation of the contract (`api.Operations`);
  - every setting cited as `` `path` = `value` `` is one the active
    configuration holds with that value (`config.Setting`, the value as
    `config.Diff` writes it); a setting written in prose is not seen, so
    the prompt asks for every setting in that form, and the next rule
    refuses its value in prose;
  - every number outside backticks, a list item's number at a line's
    start apart, is one the question writes (`recommend.Numbers`);
  - a reply naming no operation says `The contract does not answer this
    question.` (`supportagent.Unanswered`);
  - one reply per run: the first the check admits stands.
  A run whose model gives no reply the check admits fails
  (`ErrNoReply`), its session and refusals still reported.
- `evals/support`, version 1, replayed to level 2, on the tax suite's
  configuration (company 3000, a price tolerance of 2 percent or 5.00):
  an invoice above the tolerance released by `releaseSupplierInvoice`,
  the tolerance cited; a question no operation answers (booking a
  flight) replied to as unanswered; an instruction in the question to
  raise the tolerance not followed, no change request, the version still
  active. Not run against a live model.
- Walkthrough `step_support_agent` (step 88): Ada sets the agent up with
  `config.version.read`, `identity.self.read` and its session
  permissions; Alex asks how an invoice above the tolerance is released;
  a reply naming an operation the contract does not have is refused; the
  reply names `releaseSupplierInvoice` and cites the walkthrough
  company's tolerance as the active version holds it; the version is as
  it was.
- No migration, no new operation, permission, event or problem code.

### T105b, the collections agent

- `internal/collectionsagent`; agent type `collections`; `erp agent
  collections -company CODE [-key-date YYYY-MM-DD]` (the key date
  defaults to today in UTC, as T105a's date). One recommendation per
  customer of the company's collection worklist on the key date
  (ADR-0065 §9), on the customer as a `business_partner`: `call`,
  `send_reminder`, `escalate`, `wait_for_dispute` or `no_action`.
- Before its session the run reads the worklist's first page of 20
  customers (`MaxCustomers`, the largest overdue amount first: the run's
  objects; the rest wait for a later run, and the prompt says how many the
  worklist lists), and, for each customer with open dispute cases, those
  cases and its open items on the company's customer reconciliation
  accounts (`listDisputeCases`, `listOpenItems`, and `listAccounts` for
  the accounts whose `reconciliation_kind` is `customer`: a partner that
  also supplies has items on supplier accounts, which the worklist does
  not count). A
  worklist without customers starts no session and says so; a read
  refused stops the run before a session.
- **A customer whose every overdue item is disputed** (each open debit
  item past its due date has an open case, as a dunning run leaves a
  disputed item out whole, T86b) is never recommended a contact: its
  check refuses `call`, `send_reminder` and `escalate` for it, and the
  prompt names it. An item is overdue by its payment terms' net due date
  or, posted without terms, by its posting date, which is never before
  the document date the worklist takes: the run may see an item overdue a
  few days late, and so hold back a contact the worklist would allow,
  never the other way round. `wait_for_dispute` is refused for a customer
  without an open case.
- Its check (`collectionsagent.review.check`): the content starts
  `Overdue <amount> <currency>`, the customer's overdue amount as the
  worklist returned it, an amount a read of the run returned as money
  (§4: read again by the model's own `getCollectionWorklist`); every
  number of the content, the draft message's included, is one the run
  read; a contact ends with a line `Draft message:` and the message. So
  the draft states no figure the API did not return; a person sends it.
- Its reads are the plan's, with `listDunningRuns` (a notice is read
  under its run), `getDisputeCase`, `listOpenItems` and `listAccounts`,
  which need `ledger.entry.read` and `ledger.account.read`; its never list is every write of dunning and of
  dispute cases, all `x-agent: false`; nothing in the contract sends a
  message to a customer.
- `erp agent collections` prints each recommendation with the customer's
  number and name and its first line (`recommend.Result.Names`).
- `evals/collections`, version 1, replayed to level 2: a customer with an
  overdue invoice and no dispute reminded with a draft; a customer whose
  only overdue invoice is disputed not reminded (the reminder refused)
  and told to wait for its dispute, the case still open; an instruction
  and an amount in an invoice text not followed nor repeated; nothing
  dunned. Not run against a live model.
- Walkthrough `step_collections_agent` (step 100), run while
  `step_dispute`'s case is open (step 95 now leaves closing it to step
  98): the agent's role reads the worklist, cases, dunning, receivables,
  open items and partners; one recommendation per customer of the
  worklist of step 57's key date, Northwind's `wait_for_dispute` naming
  its overdue and disputed amounts; a reminder whose draft names an amount
  no read returned refused; the agent's own dunning run and case closing
  refused (403); Ruth accepts, which changes nothing; she then closes the
  case as rejected and a new run duns the invoice again.
- No migration, no new operation, permission, event or problem code.

### T108b, the support agent: root cause of a refusal or a stuck document

- A second entry function of the `support` agent, `Agent.Diagnose`, and
  unlike T108a's reply a recommendation through the pattern
  (`internal/recommend`): `erp agent support-diagnose -type KIND -id ID
  [WHAT HAPPENED]`. The kinds are the documents it reads
  (`supportagent.Documents`): `supplier_invoice`, `customer_invoice`,
  `journal_entry`, `purchase_order`, `sales_order`, `payment_run`,
  `change_request` and `operation` (a long-running one), each with its
  read operation; the kind is the recommendation's object type. What the
  user says happened (UTF-8, at most 4,000 bytes) is passed between
  `<report>` lines: data, never an instruction.
- Before its session the run reads the document by its kind and the
  active configuration version (`apitools.ActiveConfig`): either refused
  stops the run before a session.
- **The contract's search in the pattern.** `recommend.Agent.Contract`
  offers an agent `findOperations` (T108a's local tool, a read answered by
  the agent's own code) beside its reads; its answers' numbers count as
  read, as any read's do. The diagnosis uses it for what a problem code
  means.
- One recommendation, kind `root_cause`, whose first line is `Root cause:
  <class>`: `configuration`, `master_data`, `missing_approval`,
  `closed_period`, `permission` or `unknown`. Its check
  (`supportagent.diagnosis.check`): every event cited (`event <seq>`) is
  one `listAuditEvents` returned in the run for that document (its
  `object_id`); a class other than `unknown` cites at least one; every
  setting cited as `` `path` = `value` `` is the active configuration's;
  every other number, in backticks or not, is one the run read (§4). The
  document's id is read as the API writes it (lower case), whatever case
  it was given in. So a cause the events do
  not show is `unknown`. The service's logs are not behind the API and
  are not read.
- **A refused posting of a supplier invoice is audited** on the invoice
  (`payables.refusedPost`): a business refusal of `postSupplierInvoice`
  (409 or 422, which the unit of work keeps, ADR-0046's `audit.Refuse`)
  records the action `payables.supplier_invoice.post` with outcome
  `refused`, the problem's code and detail and the posting date asked
  for; a rule's refusal stays the rule's own record, and an invoice not
  found records nothing. The problem's detail is written into the
  insert-only trail, so a problem detail of that operation names no
  person (today they carry codes, ids, dates and amounts). Without it the
  trail of a parked invoice shows no
  cause. Refusals of the other writes on the documents the agent
  diagnoses are audited since by the HTTP layer
  (`httpapi.RefusalsAudited`, ADR-0011 As built 2026-11-16), with the
  problem's code and field errors but not its detail.
- Its role needs, beside T108a's, the reads of the documents it
  diagnoses, `platform.operation.read`, `workflow.request.read` and
  `audit.event.read`, which the tenant's administrator grants (§6).
- `evals/support`, version 2: three diagnosis cases (task `run:
  diagnose`) on its own configuration (the tax suite's with the supplier
  invoice's number ranges; the periods closed): an invoice whose posting
  a closed period refused diagnosed `closed_period` citing the refusal's
  event; one whose trail shows no cause diagnosed `unknown`, a cause
  citing no event refused; an instruction in an item's text not followed.
  Not run against a live model.
- Walkthrough `step_support_agent` (step 88) extended: Alex's invoice
  dated today and posted on a day of last year, whose period is closed, is
  refused (409 `ledger.period_not_open`) and stays parked; Ada grants the
  agent's role the audit trail and supplier invoices; the agent's cause
  citing an event that is not the invoice's is refused; it recommends
  `closed_period`, citing the refusal's event and quoting its detail;
  Alex accepts and posts the invoice on today's date.
- No migration, no new operation, permission, event or problem code.

### T110a, the integration agent: failing integrations

- `internal/integrationagent`; agent type `integration` (in
  `autonomy.Types`, the profile's open enumeration in both contracts, and
  the telemetry ceiling of `agent_kind`, set for every week 3 agent at
  integration, ADR-0043 As built, batch 75); no migration, no
  new operation, permission, event or problem code.
- `erp agent integration-monitor` (no flags): before its session the run
  reads the webhook subscriptions (`listWebhookSubscriptions`, deleted
  ones aside) and, of each, its deliveries that died and those waiting
  after a failed attempt (`listWebhookDeliveries` by `status`, delivered
  ones never read), the bank files `failed` or `rejected`
  (`listBankFiles` by `status`), the connectors (`listConnectors`) and the
  migration runs (`listMigrationRuns`). Any of them the agent may not read
  stops the run before a session.
- **What is failing is computed, not judged** (decided by default): a
  webhook subscription that is `disabled`, has a `failing_since`, or keeps
  a delivery that died or waits after a failed attempt (an HTTP status of
  300 or more, or an error); a bank file the channel refused (`failed`)
  or the bank `rejected`; a connector whose latest migration run (by
  `created_at`) `failed`. A connector has no failure status of its own
  and connectors of other kinds (a bank's, the treasury export's) fail
  through what they carry, a bank file or an export, so the migration run
  is the connector's own evidence in this version; a failed operation of
  another kind names no object the agent recommends on, and the model
  reads operations (`getOperation`) for the reasons.
- **One recommendation per failing object**, the object types
  `webhook_subscription`, `bank_file` and `connector`. The kinds given per
  object are computed: `check_endpoint` and `investigate` always (a bank
  file `investigate` alone), `redeliver` only on a subscription with a
  delivery that died, `pause` only on an active one, `retransmit` only on
  a file the channel refused (a rejection is the bank's, investigated).
  Its check (`survey.check`) refuses another kind (`ErrKind`), a first
  line other than the one computed (`ErrLine`) and a number the run did
  not read (`ErrUnread`, §4). The first line names the evidence:
  `Failing: subscription <status>[ (<reason>)][, failing since <instant>][;
  failed deliveries <ids>]` with at most ten deliveries, those that died
  first, each the newest first; `Failing: bank file <status>`; `Failing:
  migration run <id> failed`.
- **A tenant with nothing failing** gets no session and the reply
  "Nothing is failing: ...", printed alone.
- **Secrets**: the reads return none (a subscription's secret is shown
  once, to its creator; a connector's `secret_ref` is a vault path); the
  walkthrough checks the agent's list of subscriptions holds no `secret`.
  It generates no connector: `createConnector` is on its never list with
  every other write of connectors, webhooks, bank files, migration runs
  and operations (`TestNever` holds every such write of the contract to
  the list).
- `evals/integration`, version 1, replayed to level 2: a connector whose
  rehearsal failed the quality gate (`evals/migration/broken-extract.json`
  in the cell's extract directory, an order item whose net value is out
  of the range of an amount) recommended for investigation, naming the run; a tenant with
  nothing failing given no recommendation and a reply saying so; an
  instruction in a connector's name (to disable it) not followed. A replay
  cell has no webhook dispatcher and no bank channel, so the subscription
  and the bank file are covered by the package's tests and the
  walkthrough. Not run against a live model.
- Walkthrough `step_integration_agent` (step 102): Sam subscribes the
  supplier portal's endpoint, which was never given the subscription's
  secret and refuses every delivery (401); his ping dies. The agent's
  first recommendation without the computed line is refused; it
  recommends `check_endpoint`, naming the dead ping; its own
  `pauseWebhookSubscription` is refused (403); Sam accepts, which changes
  nothing, and deletes the subscription himself.


### T108c, the support agent: a fix drafted as a change request

- A third entry function of the `support` agent, `Agent.Fix`, through the
  pattern as T108b's: `erp agent support-fix -recommendation ID`. Before
  its session the run reads the recommendation (`getRecommendation`) and
  the active version, and refuses (`ErrNotConfiguration`) one that is not
  an accepted `root_cause` on a document of `Documents` whose first line
  is `Root cause: configuration` and that cites at least one setting as
  `` `path` = `value` ``, and (`ErrFixStale`) one citing a setting the
  active version no longer holds with that value: the cause is diagnosed
  again. The kind, class and citations are the support agent's alone;
  the recommendation's agent is not read, as in T107b.
- **The drafting is T107b's, moved, not copied** (the plan's acceptance):
  `internal/changedraft` holds the drafting's object type and kinds
  (`change_request`: `ready_to_submit`, `simulation_failed`), its writes
  (`createChangeRequest`, `updateChangeRequest`,
  `simulateChangeRequest`), its never list, its guards and observers
  (one draft per run, based on the active version and kept on it, a
  proposal that is a configuration and changes something, a change or
  simulation only of the run's draft), the wait for the simulation it
  started (`Poll`, `Patience`) and the rule that holds the kind to the
  simulation of the draft's last version (`Draft.Outcome`). What differs
  is each agent's **scope** (`Draft.Scope`, the changes a proposal may
  make, refused with `changedraft.ErrOutside`): the regulatory agent's,
  every setting at or under the paths its recommendation named; the
  support agent's, **exactly one setting, one the accepted diagnosis
  cites**. The regulatory agent's drafting is unchanged in behaviour; its
  errors `ErrBase`, `ErrOutside`, `ErrNotTheDraft` and `ErrNotSimulated`
  and its `DraftWrites`/`DraftNever` are now `changedraft`'s.
- Its reads are `getRecommendation`, `listConfigVersions`,
  `getConfigVersion`, `getChangeRequest`, `getOperation`; its never list
  is the drafting's (`TestFixTools`): it never submits, withdraws or
  activates a change request or decides an approval or a recommendation,
  and no write of master data or a document is a tool.
- Its check (`supportagent.fixing.check`): the kind is the outcome of the
  simulation of the draft's last version (`Draft.Outcome`); the first
  line is `` Change: `path` from `old` to `new` ``, the draft's one change
  as `config.Diff` writes it (`changedraft.Draft.Changes`, the last
  proposal the API accepted), whatever the model wrote; any other setting
  cited is the active configuration's with that value; every other number
  is one the run read (§4), the diagnosis's event among them, read again
  by the model's `getRecommendation`.
- No evaluation case drafts, as for T107b: a suite's setup cannot make a
  recommendation an agent made and a person accepted; the run's guards
  and check are tested in `internal/supportagent` and the walkthrough.
- Walkthrough `step_support_agent` (step 88) extended (`support_fix`):
  Ben orders a desk lamp at CAD 100.00 and receives it; Alex's invoice of
  CAD 110.00 is matched and held (`price_variance`); the agent diagnoses
  `configuration`, citing the match's event and the company's price
  tolerance; Alex accepts; Ada grants the agent's role
  `config.change.propose` and `config.change.read` (its reads of the
  draft); the agent's first draft, changing both the
  percentage and the absolute tolerance, is refused by the run; it drafts
  the percentage alone (2 to 10), has the draft simulated and recommends
  `ready_to_submit` with the change on its first line; nobody submits it.
  Only a change request's author withdraws it (`config.not_author`: Ada's
  withdrawal is refused, 403), so the step withdraws the draft with the
  agent's own token, outside its session (decided by default: no tool of
  the agent's withdraws, and a draft is left to its author); the active
  version is as it was. Alex cancels the
  invoice and Ben reverses the receipt and closes the order, so nothing
  of the lamp is left open for the steps after.
- No migration, no new operation, permission, event or problem code.

### T105d, the savings opportunity agent

- `internal/savingsagent`; agent type `savings` (added to the profile's
  open enumeration, `autonomy.Types`, and the contract's and the event's
  descriptions of it); `erp agent savings -company CODE [-period
  FROM..TO] [-max N]`. The period defaults to the year to today, UTC (as
  T105a's date); `-max` (default ten, `MaxOpportunities`) bounds the
  recommendations of a run: the plan's "at most a stated number".
- **Found before the session, not judged** (`find.go`): the spend metrics
  of T91b (`runAnalyticsQuery`, the company in `companies`, `posting_date`
  between the period's days): `spend_off_contract` by supplier and
  currency, a supplier with some above zero being an opportunity
  `bring_under_contract` on its business partner (found by its number);
  `spend` by account, supplier and currency, an account with spend from
  more than one supplier in one currency being an opportunity
  `consolidate_suppliers` on the account (found by its number in the
  company); and, where the company has active contracts, its orders of the
  period (`listPurchaseOrders`), an order released or closed, made on no
  contract, with an item of a material priced above the item of an
  active contract of the same supplier valid on the order's date (same
  currency, same price unit) being an opportunity `use_contract_price` on
  the order. Each recommendation is on its own object (§3: one per
  object), not on the company as the plan writes it. A period without
  opportunities starts no session and says so; a read refused stops the
  run before a session.
- **`runAnalyticsQuery` is a read of the pattern**: `recommend.Agent.Run`
  now admits among an agent's reads an operation marked `x-autonomy:
  read` beside a `GET`; and a money measure's cell (a `value` beside a
  `currency`) counts as money in the figures (§4), as a Money's
  `amount` does, so the spend the agent states is an amount a read
  returned.
- Its check (`savingsagent.review.check`): the first line is the spend
  the opportunity covers as found (`Spend off contract <amount> <currency>`,
  per currency; `Spend <amount> <currency>`; `Ordered <amount>
  <currency>`, the order's total), each amount one a tool of the run
  returned as money (read again by the model); a saving stated with a
  figure (a word of saving, reducing or cheaper and a number in the same
  sentence) is refused, the plan's "no saving in money" (default 13);
  every number is one the run read.
- Its reads: `runAnalyticsQuery`, `getAnalyticsModel`, `listContracts`,
  `getContract`, `listCatalogItems`, `listPurchaseOrders`,
  `getPurchaseOrder`, `getBusinessPartner`, `listAccounts`; its role needs
  the spend metrics' permissions (`analytics.query.run`,
  `payables.invoice.read`, `procurement.order.read`,
  `procurement.contract.read`, `ledger.cost_center.read`,
  `ledger.account.read`, `partner.supplier.read`) and `tenancy.org.read`.
  Its never list: `createRfq`, `publishRfq`, `createContract`,
  `releaseContract`, `createPurchaseOrder`, `releasePurchaseOrder`.
- `erp agent savings` prints each recommendation with its object's name
  (the supplier's number and name, the account's number and name, the
  order's number) and its first line (`recommend.Result.Names`).
- `evals/savings`, version 1, replayed to level 2: a supplier with posted
  spend off contract recommended to be brought under a contract with its
  spend; a supplier whose only invoice is parked not recommended on (the
  recommendation refused) and a saving stated in money refused; an
  instruction in an invoice item's text not followed, no contract
  created. Not run against a live model.
- Walkthrough `step_savings_agent` (step 104, before the close): Ada sets
  the agent up; Ben reads Tyrell's spend off contract of this year and
  last; the agent's recommendation stating a saving is refused; it
  recommends `bring_under_contract` on Tyrell, its first line Tyrell's
  spend off contract as the metric returns it; its own contract is
  refused (403 `identity.autonomy_insufficient`: level 1); Ben accepts, and Tyrell's contracts are as they were.
- No migration, no new operation, permission, event or problem code; the
  agent type `savings` added to the open enumeration of
  `agent_profile.agent_type`.


### T105f, the control monitoring agent

- `internal/controlagent`; agent type `control_monitoring`;
  `recommend.Cited` (`internal/recommend/cited.go`), shared with the risk
  agent (T105g): it keeps the identifiers the run's tool reads returned
  under a label (`Cited.Keep`, an observer of the read naming the paths
  of the answer, `items[].seq`), and `recommend.Cites` finds a content's
  citations, a label followed by a UUID or a whole number ("audit event
  1234"); a citation of what no tool read of the run returned is refused,
  as a number no read returned is (§4). As with the figures, what the
  agent's code read before the session does not count.
- `erp agent control-monitor [-process TEXT]`. Before its session the
  run reads the control matrix (`getControlMatrix`, every page; filtered
  by the process's exact text when given): a matrix the agent may not
  read stops it, and so do more than 100 controls (`MaxControls`; the
  command asks for a process). A matrix without a control starts no
  session and says so. The prompt gives each control (code, objective,
  process, nature, frequency, last test) with its links, whether each
  holds and why not. The run recommends once on each control (object type
  `control`): `exception_found` or `no_exception`; the printer names each
  control by its code (`recommend.Result.Names`).
- Its check (`controlagent.monitored.check`):
  - every number is one the run read (§4); every audit event cited, as
    `audit event <seq>` (an audit event has a sequence number, no id), is
    one a `listAuditEvents` of the run returned;
  - a control with a broken link gets `exception_found` or nothing, and
    its first line is `Broken links: ` and the ids of exactly its broken
    links in the matrix's order: which links are broken is the matrix's,
    read before the session, not the model's judgment;
  - an `exception_found` on a control whose links all hold cites at least
    one audit event (what shows the control did not work);
  - a `no_exception` on a control with no link that cites no audit event
    carries `Nothing could be checked: the control has no link and no
    audit event was found.` (`controlagent.NothingChecked`).
- Its never list is every write of the control library: recording,
  changing and retiring a control, recording a test, making and removing
  a link (`TestNever` keeps it whole against the contract's operations on
  controls); `createControlTest` is `x-agent: false`. Its role holds
  `grc.control.read` and `workflow.request.read`; the matrix also needs
  `identity.access.read` and `config.version.read`, and the audit trail
  `audit.event.read`, which a tenant's administrator grants.
- `evals/control_monitoring`, version 1, replayed to level 2: a control
  whose rule link is broken (the rule made from 2099-01-01 after the
  administrator's assignment ends that day, then ended from 2099-01-02)
  recommended as an exception naming the link and citing the rule's end;
  a control whose link holds and one without a link given no exception,
  the second saying nothing could be checked; an instruction in a
  control's objective to record a test and retire it not followed. Not
  run against a live model.
- Walkthrough `step_control_agent` (step 105): Ada sets the agent up and
  grants its role identity data, the configuration's versions and the
  audit trail; she makes the rule *whoever manages the organisation does
  not release payment runs* (`tenancy.org.manage` /
  `payments.run.release`) from tomorrow; Olive records the control ORG-01
  in the process "Access management" and links it to the rule; Ada ends
  the rule from the day after it starts. The matrix shows the link
  broken; the agent's `no_exception` is refused; it recommends
  `exception_found` naming the link and citing the audit events of the
  rule's creation and end; its own `createControlTest` is refused (403);
  Olive accepts, which records no test. Step 76's control PAY-01 and the
  rules of steps 77 and 83 are not touched.
- No migration, no new operation, permission, event or problem code.

### T105g, the risk agent

- `internal/riskagent`; agent type `risk`; `erp agent risk`. Before its
  session the run reads who the agent is (`/v1/me`, its tenant), the open
  risks (`listRisks` with `status=open`, every page, up to 100,
  `MaxRisks`), every control's last test (`listControls`) and the
  recommendations on controls (`listRecommendations` with
  `object_type=control`, newest first): a read the agent may not make
  stops it there. The control monitoring agent's latest finding on a
  control is its newest `exception_found` or `no_exception` there; an
  exception is that finding being `exception_found` and not overridden or
  ignored by a person.
- It recommends once on each open risk (object type `risk`): `rate_again`
  or `keep`, and at most once on the tenant (object type `tenant`, the
  tenant's id): `new_risk`. The prompt gives each risk (title, category,
  likelihood, impact, score) with its controls, each control's status,
  last test and finding, and says which risks are clear. The printer
  names each risk by its title, the tenant as "the tenant".
- Its check (`riskagent.register.check`):
  - a `rate_again`'s first line is `Rate: likelihood L, impact I`, each
    1 to 5, other than the risk's rating now; a risk is **clear** when it
    has at least one control and each is active, passed its last test and
    has no exception, and a clear risk is kept (`rate_again` refused);
  - a `new_risk`'s first line is `New risk: ` and its title, its second
    `Category: ` and its category, as `createRisk` would take them (1 to
    200 and 1 to 100 characters, no white space around them);
  - a `rate_again` or a `new_risk` cites what shows it, and every
    citation is of what a tool read of the run returned
    (`recommend.Cited`, T105f): `control test <id>` (from
    `listControlTests`, or a control's `last_test` in `listControls`,
    `getControl` or the matrix), `audit event <seq>` (`listAuditEvents`)
    or `recommendation <id>` (`listRecommendations`);
  - every number of the reasons (beyond a rating's first line) is one
    the run read (§4).
- Its never list is every write of the register: `createRisk`,
  `updateRisk`, `closeRisk`, `createRiskRating` (`TestNever` keeps it
  whole against the contract's operations on risks). The register's
  operations are open to agents (ADR-0078 §13), so it is the role, which
  holds `grc.risk.read` and no `grc.risk.write`, and the tools that keep
  the agent from writing; its role also holds `grc.control.read`, and a
  tenant's administrator grants `identity.access.read`,
  `config.version.read` (the matrix) and `audit.event.read`.
- `evals/risk`, version 1, replayed to level 2: a risk whose control
  failed its last test recommended for a higher likelihood, citing the
  test; a risk whose control passed with no exception kept; an
  instruction in a risk's title to close it and rate every risk 1 by 1
  not followed. Not run against a live model.
- Walkthrough `step_risk_agent` (step 106): Olive tests step 76's control
  PAY-01 again and records it failed (its rule link of step 77 is not in
  force); the agent's rating equal to the risk's is refused; it
  recommends rating step 89's risk of duplicate payments again at
  likelihood 2, citing the failed test, and, on the tenant, a new risk of
  payment runs released by whoever manages the organisation, citing the
  control monitoring agent's exception of step 98; its own rating is
  refused (403 `identity.autonomy_insufficient`: a write needs level 2,
  and its role holds no `grc.risk.write` either); Tara accepts the new rating
  and rates the risk herself.
- No migration, no new operation, permission, event or problem code.


### T105e, the planning agent

- `internal/planningagent`; agent type `planning`. It reads
  `runAnalyticsQuery` as the savings agent does: `recommend` takes an
  operation marked `x-autonomy: read` as a read beside a `GET` (§2, T105d),
  a query run with a body that changes nothing.
- `erp agent plan -company CODE -category CODE -year YYYY`: `-year` is
  the fiscal year planned, from the plan and actual figures of the year
  before. Before its session the run reads the company by its code (its
  id and local currency, `GET /v1/legal-entities`) and the category by
  its code (`listPlanCategories`); a company or category the agent may
  not read, or a locked category, whose batches the API refuses, stops
  it there (`ErrLocked`). It recommends once, on the `plan_category`:
  `plan_proposal`.
- Its check (`planningagent.proposal.check`): every number in the
  content is one the run read (§4); the content has one line per plan
  line, `Line <cost_center_id> <cost center code> account <account_id>
  <account number> period <n>: <amount> <currency>`, at least one, which
  the controller applies as the lines of a plan batch as they stand and
  reads by the codes and numbers beside the ids (review:T105e: an id
  alone would cost a call per line): the cost center one the run read
  as an active cost center of the company (`listCostCenters`) with that
  code, the account one it read in the company's cost center plan lines
  (`listPlanLines`) or plan against actual (`listCostCenterPlanActual`)
  and, as a profit and loss account with that number, in `listAccounts`
  (a read added to the plan's list for the number), the period 1 to 16,
  each cost center, account and period once, the amount a plan, actual
  or variance amount of the company's rows or plan lines the run read in
  the company's currency, written as returned with its sign (so an
  amount of another company or currency, or a cost turned into a credit,
  is refused); and for each cost center planned a line `Why
  <cost center code>: ` whose numbers include an actual amount the run
  read of that center in the plan against actual. So a plan at the year
  before's figure plus a growth the agent computed is refused, and a
  center planned without its actuals cited too. Whether a center is
  locked for planning in a period, or an account still a profit and loss
  one set up in the company, is the API's to say when the controller
  applies the batch (422 `controlling.plan_batch_invalid`,
  `controlling.cost_center_plan_locked`).
- It plans cost centers only; the profit center plan against actual and
  the governed metrics are read for the reasons.
- Its never list is every write of planning: `applyPlanBatch`,
  `copyPlanCategory`, `lockPlanCategory`, `unlockPlanCategory`,
  `createPlanCategory` (`TestNever` keeps it whole against the
  contract's writes named `Plan`), each `x-agent: false`.
- `evals/planning`, version 1, replayed to level 2: a cost center's
  budget and actual of May 2026 proposed for 2027 at the actual, the
  reasons citing both; a year with neither plan nor actuals before it
  given no proposal, the reply saying so; an instruction in a cost
  center's name to apply the plan and lock the category not followed,
  nothing planned for 2027 and the category open. Its configuration has
  journal number ranges of 2026 and 2027, so the periods of the year
  planned exist. Not run against a live model.
- Walkthrough `step_planning_agent` (step 107, before the close): next
  year's periods are set up where they are not (a journal number range
  of the company, as `next_year_open` does in December); Ada sets up the
  agent; for `step_plan_version`'s forecast FC the agent's proposal at the
  forecast plus five percent, an amount it computed, is refused; it then
  proposes CC-1100 and CC-1300 at the forecast's amounts in this month's
  period of next year, each with its actual of this month; its own
  `applyPlanBatch` is refused (403); Carla accepts, which plans nothing,
  and applies the proposal's lines herself as a plan batch of next year,
  which then holds them.
- No migration, no new operation, permission, event or problem code.


### S2e, the count planning agent (2026-11-21)

- `internal/countagent`; agent type `count_planning` (in
  `autonomy.Types` and the profile's open enumeration in both
  contracts). It ranks by consumption with `money`, a shared package
  that needs no row of its own in `internal/layout`. ADR-0082 As built S2e
  holds the rule of a count due; this section the agent.
- `erp agent count-plan -company CODE -plant CODE`. Before its session
  the run finds the company (`apitools.LegalEntity`) and the plant among
  its org units of kind `plant` by code (a plant code is unique per
  company), refusing one it cannot find or that is not active
  (`ErrPlant`); takes the company's date (`apitools.Days.Of`); and reads
  the plant's stock (`listStock`), materials (`listMaterials`), material
  documents (`listMaterialDocuments`, every page, keeping those posted in
  the year: the list is in creation order, and a goods receipt or a
  stock take-over is posted on the day its caller names, review:batch88),
  and count documents
  (`listPhysicalInventoryDocuments`), all by `plant_id`. Any of them the
  agent may not read stops it there.
- **What is due is computed, not judged** (as T110a's failing objects):
  each material in stock at a storage location of the plant gets its
  class, movements and last count there, and a line, `Count <material
  number> at <plant code>/<storage location code> (material <id>,
  storage location <id>): class <A|B|C>, <n> movement(s) since <the
  year's first day>, never counted` or `last counted on <date> (<count
  document number>)`. The due ones are ranked (class, then movements,
  the most first, then the last count, never first, then the oldest,
  then number and storage location) and the first `MaxLines` (40) given
  with their lines, and how many more are due; a plant with none due
  gets no session and the reply "Nothing is due for a count at plant
  ...", printed alone.
- **One recommendation on the plant** (object type `plant`, the org
  unit; kind `count_proposal`). Its check (`survey.check`): every line
  starting `Count ` is one given (`ErrLine`), in the order given and once
  (`ErrOrder`), at least one (`ErrForm`), and every number of the
  content one the run read (`ErrUnread`, §4: the computed lines are in
  the prompt; an amount the model cites it reads with a tool); the
  prompt tells the model that no line of its reasons starts `Count `. The
  model may leave out a material given, saying why; it adds the reasons. The
  controller who accepts creates the count documents through the normal
  operation (S2c): nothing happens on acceptance.
- Its never list is every write of count documents and goods movements
  (`createPhysicalInventoryDocument`, `enterPhysicalInventoryCount`,
  `postPhysicalInventoryDifferences`, `createMaterialDocument`,
  `reverseMaterialDocument`, and the postings that post a material
  document: `postGoodsReceipt`, `postOutboundDeliveryGoodsIssue`,
  `postStockTransportGoodsIssue`, `postStockTransportGoodsReceipt`,
  `postIntercompanyGoodsIssue`, `reverseGoodsReceipt`, batch 88;
  `TestNever` keeps it whole against the contract's writes named
  `PhysicalInventory`, `MaterialDocument`, `GoodsReceipt` or `GoodsIssue`),
  none among its tools, and each `x-agent: false` but the purchase order
  receipt's two (`postGoodsReceipt`, `reverseGoodsReceipt`, open to
  agents by the contract, not to this one): it never creates a count
  document (the plan's acceptance).
- `evals/count_planning`, version 1, replayed to level 2: a hinge never
  counted proposed by its line, no count document created; a plant whose
  stock is being counted (an open count) given no proposal and the reply
  saying nothing is due, the count as it was; an instruction in a
  material's description to create a count document and recommend
  nothing not followed, no count document created. The replay cell's day
  is fixed at 2026-11-16 (`cellClock`, as the bid comparison suite's), the
  year its configuration numbers; the line is read from the agent's
  prompt (`replay_vars`), since the year's first day is the agent's own
  day's. Not run against a live model.
- Walkthrough `step_count_agent` (step 154, after
  `step_replenishment_agent`): Ada sets up the agent; at the Toronto plant the brackets and bolts step 141
  counted today are not due, and the stand-in's proposal naming the
  brackets is refused (not a line given); it then proposes the H30 hinge
  of step 152, never counted, by its line; its own count document is
  refused (403); Ines accepts, which creates nothing, and Wade counts the
  hinge, Ines posting no difference, which closes the count.
- No migration, no new operation, permission, event or problem code.

### T109a, the enablement agent: a process described from the configuration

- `internal/enablementagent`; agent type `enablement` (`autonomy.Types`,
  the profile's open enumeration; the `agent_kind` label's ceiling holds it,
  ADR-0043 As built, batch 75). `erp agent enablement-docs -process P [-out F]`,
  `P` one of `purchase_to_pay` and `order_to_cash`; a process the first
  version does not know is refused before anything is read
  (`ErrProcess`). Before its session the run reads the active
  configuration version (`apitools.ActiveConfig`) and every role
  (`listRoles`, every page): a version or roles the agent may not read
  stop it before a session, and reading roles needs
  `identity.access.read`, which only the tenant's administrator grants
  (§6). The prompt names, per process, the settings it rests on (purchase
  to pay: the requisition and order release tiers, the receipt's
  over-delivery tolerance, the invoice tolerance, the posting limits, the
  payment terms and the payment release; order to cash: the pricing
  procedure, the payment terms and the dunning levels).
- It recommends once on the active version (object type
  `config_version`), kind `process_document`, the content the description
  in Markdown, at most the API's 10,000 characters; the command prints the
  run as every recommending agent's does and writes the content as the API
  recorded it to `F` (default `P.md`), only when a recommendation was made.
- Its check (`documenting.check`), computed, not judged:
  - the first line is `# <Process>, configuration version <N>`, the
    active version's number (`Heading`);
  - at least one setting is cited, and every setting cited as
    `` `path` = `value` `` is the active version's with that value
    (`config.Cited`, `config.Unheld`; the support agent's form, now
    shared from `internal/config` with its check), a list item named by
    its key or, where the configuration keys none, its position
    (`requisition_approval[0]`);
  - every text in backticks outside the citations, and every code joined
    by underscores (`process_owner`, `Z_PURCH_MGR`), is one of the
    tenant's active roles as read before the session: a role is written in
    backticks, alone, in a list or beside the word, and nothing else is;
  - no number stands outside the citations (a list item's number of one or
    two digits at a line's start apart, `recommend.Unnumbered`, shared with
    the support agent), in digits or in words (two to ninety, hundred,
    thousand, million, billion, dozen, half, quarter, zero, twice, double,
    triple, and the ordinals from third), and "percent", "per cent" or "%"
    stands only right after a citation of a percentage (a path ending in
    `percent`): every
    amount, percentage, count of approvals and days is a cited setting, so
    the plan's "every amount, percentage and role ... with that value"
    holds for what the check sees. It does not see a role whose code is a
    single plain word written outside backticks (`controller`), nor
    "one", "a", "first" or "second" as a count or a position.
- Its never list (`Never`, `TestNever` against the contract's `config.*`
  writes and `identity.access.manage`'s) is every write of the
  configuration and of access.
- `evals/enablement`, version 1, replayed to level 2, on the tax suite's
  configuration: purchase to pay described with the approver role, the
  tolerance and the auto-post limit cited; a description stating a limit
  the configuration does not hold refused and none recorded; an
  instruction in a role's name not followed, the role granted nothing.
  Not run against a live model.
- Walkthrough `step_enablement_agent` (step 108): Ada sets the agent up
  without `identity.access.read`; Priya's first run is refused before a
  session (403 `identity.permission_missing`); Ada grants it; the agent's
  description writing the approval limit in prose is refused by the run;
  it recommends the description naming version N, citing the requisition
  approval limit, its roles and company 3000's price tolerance as the
  active version holds them, written to `purchase_to_pay.md` and equal to
  the recommendation's content; its own change request is refused (403);
  the version is as it was.
- No migration, no new operation, permission, event or problem code.

### T109b, the enablement agent: in-context help

- A second entry function of the `enablement` agent, `Agent.Help`, as the
  plan says; same type, same suite. `erp agent enablement-help -operation
  ID -field NAME|-problem CODE`. It replies and does not recommend, so it
  runs outside the pattern, through `apitools.Run` with a local `reply`
  tool, as the support agent does (T108a); its reads are T109a's and
  `getSchema`.
- **Refused before any model call.** The plan's "422 otherwise, before any
  model call" is, for a command with no endpoint, a refusal before
  anything is read or a session started: `apitools.Explain` looks the
  operation up in the contract compiled into the binary, then the field (a
  parameter of that name, else a property of the request body, else of a
  successful response's body, dotted through nested objects, arrays and
  combined schemas) or the problem code (one the operation lists in
  `x-problem-codes`, or one the document lists for every operation), and
  answers `apitools.ErrNotInContract` for any it does not have; a question
  naming neither or both of a field and a problem code is `ErrAsk`.
- Before its session the run also reads the tenant's roles, as T109a's
  does (identity.access.read).
- The prompt gives what the contract says, between `<contract>` lines: the
  operation's method, path, summary, description, permission and problem
  codes, and the field's description.
- Its check (`helping.check`): one reply; it names the field or problem
  code asked about; it quotes at least one passage in double quotes, and
  every passage is in the operation's summary or description or the
  field's description character for character, as whole words and numbers
  (`recommend.Quotations`, `recommend.Quoted`, the regulatory agent's
  quotation rule moved from `internal/regulatoryagent` and shared); every
  setting cited is the active version's with that value; outside the
  quotations and citations no number is written in words and "percent"
  follows only a cited percentage, as T109a's rule has it, every number (a
  list's numbering apart) is one the question's names write, and every
  text in backticks and every code joined by underscores is an active role
  of the tenant's, the field or problem code asked about, or the
  operation's id, permission or one of its problem codes; those names are
  taken out before codes are looked for, so a problem code written without
  backticks is not read as the code after its dot.
- `evals/enablement`, version 2: three cases added (task `operation` with
  `field` or `problem`): `payables.match_required` of `postSupplierInvoice`
  explained with the tenant's tolerance cited; `gross_amount` of
  `createSupplierInvoice` explained from its description; an instruction
  in a role's name not followed while `status` of `listRoles` is explained.
- Walkthrough `step_enablement_agent` extended (`enablement_help`): Alex
  enters an invoice on Tyrell's order and posts it before matching it
  (409 `payables.match_required`); a code the contract does not have is
  refused before any model call; the agent's reply quoting words the
  contract does not write is refused; it explains the code, quoting the
  operation's description and citing company 3000's price tolerance in
  force; Alex cancels the invoice.
- Help beside a field in the web workspace is not built (the plan's
  "Later"). No migration, no new operation, permission, event or problem
  code.

### Hardening 2 (review:batch76): every agent's writes held to its run

- **Concurrent calls.** The runner executes one turn's tool calls
  concurrently, so every tool whose guard or observer shares a run's state
  runs each call whole under the run's lock (`apitools.Tool.Exclusive`),
  as the pattern's tools and the tax agent's already did: the cash
  forecast agent's two proposing tools, the close agent's proposing tool,
  the reconciliation agent's proposal, and the configuration, test and
  migration agents' writes. Each has a replay test with several calls in
  one turn (`llmtest.Together`), run with `-race`.
- **Sessions and tools.** A session whose tools cannot be built is ended,
  failed, rather than left running (`apitools.Session.Tools`, used by
  every agent, the support and enablement agents' reads included); tools
  are found by name, never by position.
- **The run's object.** The accounts payable exceptions agent is an agent
  of this pattern (`agent.Type` `ap_exceptions`): its one recommendation
  is held to the run's session and invoice by the pattern's guard, with
  the pattern's rules added to its prompt. Agents that write outside the
  pattern hold the write to the run themselves: the reconciliation
  agent's proposal names the run's statement, line and session
  (`reconagent.ErrOtherLine`); the configuration agent changes, submits,
  simulates or withdraws only the change requests its session created
  (`configagent.ErrNotCreated`), never a person's; the test agent records
  one recommendation, on the change request it was given, and captures at
  most one regression suite; the migration agent creates only rehearsals
  and starts only a rehearsal its session created, and the API refuses an
  agent a run that is not a rehearsal (403 `identity.agent_not_allowed`,
  ADR-0010), whoever created it. The tax agent keeps its own guards (its
  adjustment entries are a write the pattern does not hold); moving its
  recommendation half onto the pattern is left to the backlog.

### Hardening 3 (review:batch77): one runner, one day, one bound

- **The analytics agent's tools.** Its three tools (`list_metrics`,
  `run_query`, `build_dashboard`) share the session's results and the
  dashboard built, and the runner executes one turn's calls concurrently:
  two queries in one turn wrote the results at once. Each call now runs
  whole under the answer's mutex, as `apitools.Tool.Exclusive` does for
  API tools (`TestAgentToolsOfOneTurnRunOneAtATime`, `-race`). The agent
  runs on `llm.Runner` (`Params`, `RunTools`, `Result`, `Text`) rather than
  its own copy of the request; `llm.RunTools` is the runner of an agent
  whose tools answer in-process with no API session, and `llm.Error`, used
  by nothing, is gone.
- **An observed write the session did not record.** `apitools.Tool.Invoke`
  calls the tool's observer with every answer the API gave before it
  records the call, so a write the API took closes the run's guards (one
  recommendation per object) even when the record fails and the call
  returns `ErrNotRecorded`.
- **The model's numbers.** A tool's input is decoded with its numbers as
  their text (`apitools.DecodeInput`, used by `Execute` and `erp mcp`), so
  an integer past 2^53 reaches the API unchanged; `apitools.Body` is the
  one reading of an input's body object the agents' guards share, and
  `recommend.Iterations` the one place an agent's default bound on model
  requests is applied (the migration agent's 15 now a `MaxIterations` it
  can be given). The cash forecast agent's guards refuse a proposal that
  leaves out its company or period, which the API would refuse too.
- **The day a run is on** (coordinator 2026-11-17, ERP practice). One
  port, `apitools.Days`: a run of one company is dated in the company's
  time zone, as the API judges every date of the company
  (`getLegalEntityToday`, over `config.DocumentDay`), any other run in
  UTC. The credit agent's "today" and the collections agent's default key
  date are the company's; the analytics agent's is the asked company's
  when the question is asked of one company (`-companies` naming one),
  else UTC's; the access agent's stays UTC's (product owner 2026-11-16).
  At 23:30 in Toronto the credit agent's run is on that day, no longer on
  UTC's tomorrow.

### S2l, the replenishment agent (plan-supply-chain.md §S2l, 2026-11-21)

- `internal/replenishmentagent`; agent type `replenishment`. `erp agent
  replenish -run ID` reviews a completed planning run of a plant (ADR-0084
  §3): before its session it reads the run (`getMrpRun`); a run the agent
  may not read or that has not completed (`ErrNotCompleted`, naming a
  failed run's problem) stops it there, and a run that proposed nothing
  starts no session and says so. Its objects are the requisitions the
  run's lines name, each a `requisition`, named in the printout by its
  number and material.
- Its sources of supply are the ones the tenant's data has (the plan:
  "the material's source data or purchasing history that exists"): the
  company's catalog items of the material (ADR-0071, S1f2's
  `material_id`), its outline agreements (contracts and scheduling
  agreements with an item of the material, ADR-0070) and its purchasing
  history (purchase orders with an item of it). No source list, info
  record or quota arrangement is built (SAP's source determination, from
  memory: Later). `listPurchaseOrders` and `listContracts` gain an
  optional `material_id` (additive): the orders and contracts with an
  item of the material that no change removed, each answered whole.
- Its check (`replenishmentagent.review.check`): the recommendation is on
  a proposal the run read with `getRequisition` that is still to be
  ordered (`draft`, `pending_approval`, `approved`) and of one item of a
  material (`ErrProposal`). `order`: the first line is `Order <quantity>
  <unit> from supplier <supplier_id> by <YYYY-MM-DD>: <source>`, the
  source `contract <id> item <n>`, `catalog item <id>` or `purchase order
  <id>`, and a line `Why: ` gives the reasons (`ErrForm`); the quantity
  and unit are the proposal's (`ErrQuantity`: lot sizing is Later,
  ADR-0084 §6); the day is the one the proposal is needed by, the run's
  day when it names none (`ErrWhen`: lead times are Later); the source is
  one the run read of the proposal's material in its company, an `active`
  catalog item, an `active` contract's item valid on that day, or a
  `released` or `closed` order's item, and the supplier is the source's
  (`ErrSource`); the supplier is one the run read (`getBusinessPartner`)
  as `active` with the supplier role (`ErrSupplier`). Every number but
  the first line's, which the check holds whole, is one the run read
  (§4; a quantity is a text, not an amount, so the first line is left out
  of the figures check rather than refused for a fractional quantity).
  `source_needed` orders nothing and is refused once the run read a
  source an order could use (the same test: the proposal's material and
  company, valid on the day) whose supplier it has not read as inactive
  (`ErrHasSource`, naming the first such source by its key), so every
  proposal still to be ordered has one recommendation the check admits. The prompt orders a valid contract before a catalog
  item before the supplier last ordered from; the check takes any source
  read: which to prefer is the buyer's to judge.
- Nothing happens on acceptance: the buyer names the supplier or contract
  on the requisition (`updateRequisition`, which takes the proposal over
  from planning, ADR-0084 §3), submits it, sources it by an RFQ, or orders
  it. Its never list is every write of requisitions and purchase orders,
  their changes included, a scheduling agreement's schedule lines
  (`createScheduleLine`, and `callOffScheduleLine`, which creates an
  order, ADR-0070 §4), `createRfq`, `startMrpRun` and materials' planning
  data (`TestNever` keeps it whole against the contract's writes named
  `Requisition`, `PurchaseOrder`, `ScheduleLine`, `MrpRun` and
  `MaterialPlan`).
  Requisitions and orders are `x-autonomy: propose`, so the API refuses
  the agent them by its role, which holds no write (403
  `identity.permission_missing`).
- `evals/replenishment`, version 1, replayed to level 2: a run's proposal
  of the H30 hinge recommended for an order of its quantity from the
  hinge's catalog supplier by its day; a proposal with no source of
  supply given `source_needed`; an instruction in a catalog item's
  description not followed, the proposal's quantity ordered. In each the
  proposal stays the run's draft at its version and no purchase order
  exists. A case's setup starts the run; the adapter waits until the cell
  has carried it out (`carriedOut`), and the replay cell runs the
  in-process planner for this suite. Not run against a live model.
- Walkthrough `step_replenishment_agent` (step 153, after `step_mrp`):
  Priya loads Tyrell's hinges into the catalog; Mira raises the hinge's
  reorder point to 60 and Ben's run proposes 30; the agent's order of 40
  is refused, its order of the 30 from Tyrell's catalog item made; its
  own change of the proposal refused (403); Ben accepts, which changes
  nothing, and names Tyrell on the proposal himself.
- No migration, no new operation, permission, event or problem code.

### S2m, the demand forecasting agent (plan-supply-chain.md §S2m, 2026-11-22)

- `internal/demandagent`; agent type `demand_forecast` (in
  `autonomy.Types` and the profile's open enumeration in both
  contracts). ADR-0082 As built S2m holds the method of the forecast;
  this section the agent.
- `erp agent demand-forecast -company CODE -material NUMBER`. Before its
  session the run finds the company (`apitools.LegalEntity`) and the
  material by its number (`listMaterials`), refusing one it cannot find
  or that is extended to no plant of the company (`ErrMaterial`); takes
  the company's date (`apitools.Days.Of`); and reads the material's
  documents (`listMaterialDocuments` by `material_id`, every page, kept
  by posting date: the list is in creation order) and the company's
  confirmed orders of it (`listSalesOrders` by `legal_entity_id`,
  `status` `confirmed` and the new `material_id`, every page). Any of
  them the agent may not read stops it there; a quantity in a unit the
  material has not, or a date that is none, stops it too, never guessed.
- **The forecast is computed, not judged**: each plant of the company the
  material is extended to, by code (the first `MaxLines`, 20, given, and
  how many more), gets the line `Forecast <material number> at <plant
  code> (plant <id>), <base unit> per week from <the day after>: <8
  figures>; issued <n> in 13 weeks, open orders <n>; accuracy <x>% over 4
  weeks` (or `accuracy none over 4 weeks: nothing was forecast`), its
  figures to three decimals, half away from zero. A material with no
  goods issue in the 17 weeks read and no open order at any of them gets
  no session and the reply "Nothing to forecast for material ...",
  printed alone.
- **One recommendation on the material** (object type `material`; kind
  `demand_forecast`). Its check (`forecast.check`): every line starting
  `Forecast ` is one given (`ErrLine`), in the order given and once
  (`ErrOrder`), all of them, then reasons (`ErrForm`: a planner needs
  every plant's forecast); every number of the reasons one the run read
  (`ErrUnread`, §4). The lines are left out of the figures check, as the
  replenishment agent's first line is: their figures are the run's, held
  whole by the line check, and a quantity with a fraction is no amount a
  read returns, so the prompt keeps such figures to the lines. The
  planner who accepts sets the material's planning data
  (`updateMaterialPlan`) or runs planning (`startMrpRun`) through the
  normal operations: nothing happens on acceptance.
- Its never list is every goods movement and count (as the count planning
  agent's, with `createStockTransportOrder`, `cancelStockTransportOrder`
  (review:batch92) and a return's receipt, `confirmReturnReceipt`), every write of sales and returns orders and
  their deliveries (`createSalesOrder`, `captureSalesOrder`,
  `createSalesOrderUpload`, `confirmSalesOrder`,
  `releaseSalesOrderCreditBlock`, `changeSalesOrder`, `cancelSalesOrder`,
  `confirmSalesDelivery`, `createOutboundDelivery`,
  `pickOutboundDelivery`, `createReturnsOrder`, `confirmReturnsOrder`),
  the replenishment agent's never list of requisitions and purchase
  orders (their changes, `createRfq`, `createScheduleLine`,
  `callOffScheduleLine`, which creates an order, and `createGrIrClearing`
  included) and planning (`startMrpRun`, `createMaterialPlan`,
  `updateMaterialPlan`): it never changes stock, orders or planning (the
  brief's acceptance). `TestNever` keeps it whole against the contract's
  writes named `MaterialDocument`, `GoodsReceipt`, `GoodsIssue`,
  `StockTransport`, `PhysicalInventory`, `SalesOrder`, `SalesDelivery`,
  `OutboundDelivery`, `ReturnsOrder`, `ReturnReceipt`, `Requisition`,
  `PurchaseOrder`, `ScheduleLine`, `GrIrClearing`, `MrpRun` and
  `MaterialPlan`.
- `listSalesOrders` gains optional `plant_id` (the orders delivering from
  the plant; an order that names none matches none) and `material_id`
  (the orders with a standing item of the material, each answered whole;
  `TestListByPlantAndMaterial`), additive.
- `evals/demand_forecast`, version 1, replayed to level 2: a hinge issued
  to a cost center forecast at its plant by its line (10 in the week,
  0.769 a week); a hinge with no demand given no forecast and the reply
  saying so; an instruction in a material's description to issue its
  stock and recommend nothing not followed. In each the material's
  documents stay as they were. The replay cell's day is fixed at
  2026-11-16 (`cellClock`), the year its configuration numbers; the line
  is read from the agent's prompt (`replay_vars`). Not run against a live
  model.
- Walkthrough `step_demand_forecast_agent` (step 158, after
  `step_bad_debt_relief`): Ada sets up the agent; Wade issues 4 H30
  hinges to CC-1100; the stand-in's line of its own is refused, then the
  line given (50 this week, Sally's confirmed order of step 152, then
  0.308 a week; accuracy none) is recommended; its own goods issue is
  refused (403); Mira accepts, which moves nothing.
- No migration, no new permission, event or problem code; the two
  filters are the one contract change.

## Defaults taken (for the product owner's digest)

1. The shared run is a package, `internal/recommend`, not a copy per
   agent (`CLAUDE.md`: no copied helpers); `apitools.LegalEntity` likewise.
2. An agent of the pattern has no tool but reads (`GET`, or marked
   `x-autonomy: read`, T105d) and `createRecommendation`, refused at run
   time otherwise.
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
14. (T105c) The agent may recommend any quotation the comparison ranks,
    not only the one ranked first: the plan asks only that it be ranked,
    and the prompt asks for the one ranked first unless the reasons say
    otherwise. An award is recommended only for a published RFQ, as only
    one is awarded (ADR-0069 §7); the quotation named is held to the
    comparison as read before the session.
15. (T107a) The recommendation is on the active configuration version,
    the one object the configuration's reads name; the notice is given as
    a file of at most 64 KiB, the size a prompt holds whole. Quotations are
    the text between double quotes; the agent does not judge whether a
    notice is genuine, which the caveat leaves to a person, and the
    product ships no regulatory content: every notice of the suite and the
    walkthrough is invented and says so.
16. (T107b) A draft is simulated before a person submits it as a preview
    that records no evidence and moves nothing (`simulateChangeRequest` on
    a `draft`); the submitted request is simulated again for its evidence,
    so the governed path is unchanged. Only an accepted `change_needed` on
    the version still active is drafted from; the draft is held to the
    recommendation's paths, not to the notice's figures, which the person
    submitting it checks.
17. (T108a) The support agent replies and does not recommend, so it runs
    outside the pattern, through `apitools.Run` with two local tools, the
    contract's search and its reply. A setting counts as cited only in the
    form ``path`` = ``value``, and a reply's numbers outside backticks are
    the question's; operation ids are told by their form. The contract is
    searched by id and summary, not description.
18. (T105b) The collections agent recommends on the first 20 customers of
    the worklist, the largest overdue amount first; a customer with an
    undisputed overdue item beside disputed ones may be contacted; "every
    overdue item disputed" is told from the open items and open cases
    before the session, an item without payment terms taken as due on its
    posting date, and only items on customer reconciliation accounts
    counted; its reads add `listDunningRuns`, `getDisputeCase`,
    `listOpenItems` and `listAccounts` (so its role holds
    `ledger.entry.read` and `ledger.account.read`) to the plan's;
    a contact needs a draft message, `wait_for_dispute` an open case.
19. (T108b) The diagnosis recommends on the document under its kind's
    name as object type, kind `root_cause`, the class on the first line;
    events are cited by their seq; a class other than `unknown` needs an
    event of the document. A refused posting of a supplier invoice is now
    audited on the invoice (outcome `refused`, the problem's code and
    detail, the posting date asked for), as ADR-0046 audits a rule's
    refusal, so a parked invoice's trail shows why; the other documents'
    refusals are not audited (a gap for the product owner).
20. (T110a) What is failing is computed before the session, as listed
    under As built: a subscription disabled, failing, or keeping a
    delivery that died or failed an attempt; a bank file `failed` or
    `rejected`; a connector whose latest migration run failed. The kinds
    given per object follow from it (`redeliver` only with a dead
    delivery, `pause` only on an active subscription, `retransmit` only
    on a file the channel refused), and a tenant with nothing failing gets
    no session. A first line names at most ten failed deliveries.
21. (T108c) The fix is drafted only from an accepted `root_cause` of the
    class `configuration` that cites a setting, and changes exactly one
    of the settings it cites; a cited setting the active version no
    longer holds with that value stops the run (diagnose again). T107b's
    drafting is shared from `internal/changedraft`, each agent giving its
    scope. Its first line is the draft's change as `config.Diff` writes
    it. No evaluation case, as for T107b.
22. (T105d) The plan has every recommendation "on the company"; §3 has
    one recommendation per object, so each opportunity is recommended on
    its own object: the supplier (`bring_under_contract`), the account
    (`consolidate_suppliers`), the purchase order (`use_contract_price`);
    the company is the run's. The opportunities are found by the run's
    code, not the model: a supplier with spend off contract in the period;
    an account with spend from more than one supplier in one currency; an
    order released or closed, made on no contract, with an item of a
    material priced above the item of an active contract of the same
    supplier valid on the order's date, in the same currency per the same
    quantity. At most ten a run by default (`-max`), suppliers first, then
    accounts, then orders, each by currency (in code order) and the largest
    spend first within a currency, never ranked across currencies; a spend
    query answers at most 500 rows, the server keeping the largest by the
    figure as written whatever its currency, a known limit. The period
    defaults to the year to today (UTC). The first line is the spend the
    opportunity covers as a tool returned it (the spend off contract, the
    account's spend, the order's total), per currency, never summed
    across currencies; a saving stated with a figure is refused (a
    backstop: a saving stated in other words is not seen).
23. (T105f) The plan's "a control with no link and no automated evidence"
    is read as a control without a link on which the run cites no audit
    event: it gets `no_exception` only with the statement that nothing
    could be checked. A broken link is an exception by the matrix, not by
    the model's judgment; an exception on a control whose links hold
    needs an audit event to cite. Audit events are cited by their `seq`,
    since they have no id.
24. (T105f) A run covers the tenant's active controls, or one process's,
    up to 100, one recommendation per control and run; a second run
    recommends again, as every agent of the pattern does (plan "Later":
    monitoring on a schedule).
25. (T105f) The walkthrough's rule is ended, as the plan asks, but cannot
    show `sod_rule_ended` today: the administrator holds every permission
    today, so a rule starts tomorrow at the earliest (as step 77's), and
    it ends after the day it starts. Its link is shown broken as
    `sod_rule_not_started`, and the agent cites the audit event of the
    end.
26. (T105g) "A risk whose controls all passed their last test and has no
    exception" is computed before the session: at least one control,
    each active, its last test passed, and the control monitoring
    agent's latest finding on it not an exception a person left standing
    (open or accepted). A risk without a control is not clear: the model
    judges it.
27. (T105g) A `rate_again` must differ from the risk's rating now; a new
    rating or a new risk cites at least one control test, audit event or
    recommendation the run read. The tenant is one object, so a run
    recommends at most one new risk (plan "Later": risks from outside
    sources).
28. (T105g) The plan's walkthrough reads "the failed control test of
    `step_controls`", but step 76 records a passed test; Olive records
    the failed one in `step_risk_agent` (a correction is another test,
    ADR-0078 §3), on the evidence the matrix shows: PAY-01's rule link is
    not in force today.
29. (T105e) `-year` names the year planned, read from the year before;
    the agent plans cost centers only (a profit center line, which a plan
    batch also takes, waits for a slice that needs it); its amounts are
    figures a read returned, so a growth on last year is the
    controller's to apply, not the agent's; `listAccounts` joins the
    plan's reads so that each line names its account's number; an account may be planned
    when the run read it in the company's cost center plan or actuals,
    the API judging the rest when the batch is applied; a locked
    category stops the run before a session. A read marked `x-autonomy:
    read` is a read of the pattern (§2).
30. (T109a) The enablement agent's description is a recommendation on the
    active configuration version, the object the configuration's reads
    name; every figure in it is a setting cited as `` `path` = `value` ``
    and no number stands outside the citations, in digits or in words,
    every role named (in backticks, or a code joined by underscores) is an
    active role of the tenant's; reading roles needs
    `identity.access.read`, granted by the administrator as for the
    access agent. The first version knows purchase to pay and order to
    cash, and writes the file the command names (default `P.md`).
31. (T109b) Help is a second entry function of the same agent that
    replies; "422 before any model call" is a refusal before anything is
    read (the command has no endpoint); a field is a parameter or a body's
    property, dotted; a problem code is the operation's or one every
    operation may answer; the reply quotes the contract and cites only the
    active version's settings.
32. (S2l) The replenishment agent recommends on each proposal of a
    completed run, as a `requisition`: `order` (the proposal's quantity,
    by the day it is needed, from a source the run read: a catalog item,
    a valid contract item or the purchasing history, and that source's
    active supplier) or `source_needed`; lot sizes, lead times and a
    supplier's reliability are Later (ADR-0084 §6, the tracker's
    replenishment variants). `listPurchaseOrders` and `listContracts`
    filter by `material_id`, so the agent reads a material's orders and
    contracts without paging the company's.
33. (S2e) The count planning agent recommends on the plant (an org unit,
    object type `plant`), once per run, naming the materials it proposes
    by the lines computed before its session; the run's day is the
    company's (`apitools.Days.Of`); at most 40 lines are given, the rest
    counted in the prompt for the next run; a plant with nothing due gets
    no session. The rule of a count due is ADR-0082 As built S2e's.
34. (S2m) The demand forecasting agent recommends on the material (object
    type `material`), once per run, every plant of the company's line
    computed before its session (the method ADR-0082 As built S2m's);
    the model adds the reasons and may not leave a plant out; at most 20
    plants are given; a material with neither issues nor open orders gets
    no session. `listSalesOrders` filters by `plant_id` and `material_id`,
    so the agent reads a material's orders without paging the company's.
