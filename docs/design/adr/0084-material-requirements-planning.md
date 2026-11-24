# ADR-0084: Material requirements planning

**Status:** accepted (lane mrp, S2k, 2026-11-21; decided by default under
ADR-0053 d, ADR-0082 leaving planning to its own ADR, plan-supply-chain.md
"New contexts"; accepted once `go-reviewer` had no blocking finding). It
decides the planning context of `docs/design/plan-supply-chain.md` §S2k-S2n
and builds S2k. **The SAP behaviour named here is stated from memory** (no
primary source in the repository); it is listed for the SAP specialist.

## Context

The supply chain plan (§S2k) asks for "a planning run per plant [that]
computes net requirements (demand from open sales orders and a reorder
point against stock and open receipts) and proposes purchase
requisitions", accepted when "a rerun replaces its own open proposals and
never a person's requisition". S2l (a replenishment agent), S2m (demand
forecasting) and S2n (S&OP) build on its runs. ADR-0082 decided the
`materials` and `inventory` contexts and left planning to an ADR of its
own; ADR-0012 names "MRP and costing runs" among the work Temporal runs.

What the design already settles:

- A context reads another only through its published API, in a port the
  composition root wires (ADR-0001, `internal/layout`); a new arrow into
  an existing context needs an ADR.
- Plants are tenancy's org units (ADR-0082 §1); quantities exact decimals
  in the base unit with exact unit conversions (ADR-0082 §5).
- Sales orders name the plant they deliver from and confirm what is
  available there (S2j, ADR-0082 As built S2j); purchase order stock items
  name their plant and storage location (S1e); requisitions are drafts of
  their requester, submitted for approval by amount.
- Long-running work is an Operation carried out as a workflow, or in
  process without an engine, with idempotent steps acting as the
  requester (ADR-0012 and its As built).

## Decision

### 1. A leaf context `mrp`

Material requirements planning is the context `mrp` (schema `mrp`,
`internal/mrp`), a **leaf** that no context imports: it imports the
published types of `materials` and `tenancy` and the configuration's
company day (`config`), and reaches tenancy's plants, the ledger's
accounts, sales, inventory and procurement through ports the composition
root wires (`internal/app/mrp.go`). The name is `mrp`, not `planning`: the
tenant's cost and budget planning is controlling's (ADR-0050 §8) and its
agent is `planningagent`. No dependency direction between existing
contexts changes; the new reads below are additions to their published
APIs, which `app` adapts into mrp's own types.

### 2. What it reads and writes

- **Demand** (sales, `sales.Orders.PlantRequirements`): per material and
  unit, each standing item's ordered quantity less what is delivered of
  the plant's **confirmed** orders. Ordered, not confirmed: an item the
  availability check could confirm only in part (S2j) still demands the
  rest, which planning is to procure (SAP counts a sales order's open
  quantity as its requirement, from memory). Draft and cancelled orders,
  removed items and orders naming no plant demand nothing. Since the
  plant is required (ADR-0082 As built, S2j: the plant required,
  po:2026-11-21) an order confirmed with a stock item always names its
  plant (entered, or the company's only one), so an order naming none is
  one of services alone or one confirmed before that rule. Besides, the
  intercompany orders the plant supplies and has not issued
  (procurement, `Orders.IntercompanyDue`; As built, batch 91).
- **Stock** (inventory, `PlantStock`, S2j's read): what the plant's
  storage locations hold; stock in transit is not counted.
- **Prices** (inventory, `PlantPrices`, new): the material's valuation
  price at the plant, which a proposal is estimated at.
- **Receipts** (procurement, `Orders.OpenReceipts`, new): the open
  quantity (ordered less received) of stock items at the plant of
  purchase orders not closed or cancelled, draft and pending orders
  included (SAP counts a purchase order item whatever its release, from
  memory); and the items of the requisitions earlier runs proposed that a
  person took over and that are not ordered, cancelled or rejected
  (`Orders.Requisitions`, new). A requisition a person raised names no
  plant (requisition items have none), so it is not counted.
- **Requisitions** are written through procurement's published API:
  `ProposeRequisition` creates a draft as `createRequisition` does (its
  checks, number, audit) for the run's requester; `WithdrawProposal`
  cancels one under its row lock only while it is a draft at the version
  the run created it in, audited and published as a cancellation is.
  Procurement keeps no mark of a proposal; mrp records them
  (`mrp.proposal`).

### 3. The planning run

A run of one plant (`POST /v1/mrp-runs`, 202 with its Operation, kind
`mrp.run`) is queued with the Operation, one per plant at a time, and
carried out by the workflow `MrpRun` (ADR-0012), or by the in-process
`Planner` without an engine, as the person who started it, refused once
they are no longer active (PO 2026-11-03). Its one step, `PlanRun`, is
**one transaction**, so a run proposes and cancels everything or nothing:

1. the run under its row lock; a run no longer queued is left alone
   (idempotent); one whose Operation was cancelled fails with 409
   `mrp.run_cancelled`;
2. the company's day (`config.DocumentDay`) and the plant's plans by
   reorder point under a share lock, of materials still active, kept in
   stock and extended to the plant, by material;
3. **settling the plant's earlier proposals**, in requisition order (the
   order their row locks are taken in): an open one still a draft at its
   proposed version is withdrawn (cancelled) and marked `replaced` by this
   run; one a person changed or submitted since is `taken_over`, theirs
   from then on and never touched again; a taken-over one still to be
   ordered (draft, pending approval, approved, in sourcing) counts its
   items as receipts; one ordered, cancelled or rejected is `closed`;
4. per material, in its base unit: available = stock + receipts −
   demand; below the reorder point, the **shortage** is what brings it
   back there, rounded up to six decimals (`money.CeilQuantity`), and is
   proposed as a draft requisition of the material in its base unit, in
   the company's currency at its valuation price at the plant (zero when
   it is not valued there; such a proposal, submitted unchanged, routes
   as an amount of zero, the lowest approval tier: for the product
   owner), naming the plan's account and needed by the run's day; one
   requisition per material (SAP's MRP creates one
   purchase requisition per material and date, from memory);
5. the lines, the proposals and the run `completed` with its counts; the
   Operation succeeded; audited (`mrp.run.complete`).

A business problem (a number range exhausted, an account no longer
postable, a quantity in a unit the material no longer has, 409
`mrp.unit_not_convertible`) fails the run and its Operation through
`FailRun`, nothing proposed or cancelled; a failure of the machinery
outlasting its five tries fails it as `platform.internal`. The failure is
recorded until it is (ADR-0012 As built), so the plant is always freed.

**Locks.** The run row (FOR UPDATE), the plans (FOR SHARE), the earlier
proposals (FOR UPDATE, by requisition id), each requisition withdrawn
(FOR UPDATE, by requisition id), then what `createRequisition` takes (the
number range). A person changing a requisition locks that requisition
alone, a planner changing a plan that plan alone; neither then waits for
a run, so there is no cycle. A change of a proposal racing a run either
commits first, moving its version, and the run, which finds it so under
the withdrawal's lock, reads it again as the person left it (changed:
taken over and counted as it is now; cancelled: closed), or waits for the
run and finds the proposal cancelled (412 on its stale version).

### 4. Planning data

`mrp.material_plan` holds a material's planning data at a plant (SAP
MARC's MRP view; plan-supply-chain.md's "plant-specific MRP data (MARC
fields beyond the assignment, S2k)"), in the planning context rather than
on materials' governed plant data: it changes with the planner's work,
not through a master data approval, and keeps `materials` free of
planning. It holds the MRP type, `reorder_point` (SAP VB: reorder point
planning) or `no_planning` (ND), the reorder point in the base unit and
the account its proposals name (procurement requires an account on every
requisition item; a stock item's receipt posts its valuation class's
accounts, ADR-0082 As built S1e). Created once per material and plant
(`POST /v1/material-plans`), changed at its version (`PATCH`), audited.

### 5. Permissions and agents

`mrp.plan.read` (plans and runs), `mrp.plan.write` (plans) and
`mrp.run.start`. Every write is human-only (`x-agent: false`): S2l's
replenishment agent will recommend from the runs' proposals, never start
runs or change plans.

### 6. SAP behaviour followed, and where it is not

Followed (from memory): planning per plant; MRP types VB (reorder point)
and ND; net requirements of stock plus fixed receipts (purchase orders,
purchase requisitions) against sales requirements; a rerun deleting its
own planned proposals that nobody firmed and keeping the firmed ones; one
purchase requisition per material proposed at its valuation price.

Not followed (Later, plan §S2l "Later"): lot sizing procedures (the
shortage is ordered exactly), safety stock, MRP areas, planned orders for
production, scheduling with lead times and calendars (a proposal is
needed by the run's day), the planning time fence and firming by date,
stock in transit and stock transport orders as receipts, net change
planning (every run plans every material of the plant), a run over
several plants, exception messages, and source determination (a
proposal names no supplier). A requisition a person raised is not counted
as a receipt, having no plant.

## Consequences

- A tenant plans a material by creating its planning data; nothing is
  planned by default.
- Each run cancels and recreates its open proposals, so their numbers
  move from run to run; a buyer who wants a proposal to stay changes or
  submits it.
- `sales`, `inventory` and `procurement` each gain a read (and
  procurement two writes) in their published APIs; no context imports
  `mrp`.

## Alternatives

- **MRP data on `materials.plant_data`**: rejected for now: every change
  of a reorder point would be a governed material change with an
  approval, and materials would carry planning fields its readers do not
  need.
- **A mark on the requisition** (`procurement.requisition.source`):
  rejected: procurement would know of planning; the proposal table keeps
  the direction one way and lets a person's change make a proposal
  theirs by its version alone.
- **Deleting replaced proposals**: rejected: nothing is deleted (04-api-design, Methods); a
  cancellation is audited and published.
- **Chunked runs (a step per material)**: rejected for S2k: a pilot
  plant plans few materials, and one transaction makes the replacement
  all or nothing; chunking is for when a plant's run outgrows a
  transaction.

## As built

### S2k (2026-11-21)

- Migration 000436 (03 §3.37; schema tests `db/mrp_test.go`, the
  triggers also as the owner).
- API: `createMaterialPlan`, `listMaterialPlans`, `getMaterialPlan`,
  `updateMaterialPlan`, `startMrpRun`, `listMrpRuns`, `getMrpRun`.
  Problems `mrp.plant_invalid`, `mrp.material_invalid`,
  `mrp.invalid_mrp_type`, `mrp.invalid_quantity`,
  `mrp.account_not_postable`, `mrp.material_plan_exists`,
  `mrp.nothing_to_change`, `mrp.run_in_progress`, `mrp.run_cancelled`,
  `mrp.unit_not_convertible`; `MrpRunError` lists the codes a run
  fails with, which are not the request's.
- Workflow `MrpRun` (`{tenant}-mrp_run-{run}`) with the activities
  `PlanMrpRun` and `FailMrpRun`; the in-process `Planner`. Its first
  version's histories (a run planned, and one whose plan fails with a
  problem and goes to `FailMrpRun`) are recorded in
  `internal/workflows/testdata/mrp_run_v1.json` and
  `mrp_run_problem_v1.json`, which `TestMrpRunReplay` replays (batch 87's
  integration).
- No event: the run publishes none until a consumer asks
  (plan-supply-chain.md, "What every slice reuses"); the requisitions'
  cancellations are procurement's `RequisitionCancelled`.
- Walkthrough step 152, `step_mrp`.
- **Batches and serial numbers** (batch 87's integration, decided by
  default): a material managed in batches (S2f) or by serial number
  (S2g) is planned as any stock material; its proposed requisition waits,
  as a person's would, until a purchase order's goods receipt carries
  batches and serial numbers (`procurement.invalid_stock_item` refuses
  such a stock item for now).

### S2l (2026-11-21)

- The replenishment agent (ADR-0072 As built S2l, `erp agent replenish
  -run ID`) reviews a completed run's proposals through the API, as §5
  foresaw: it reads `getMrpRun` and each proposal, never starts a run or
  changes a plan or a requisition. It finds a proposal's supplier in the
  sources the tenant's data has (catalog items of the material, contracts
  with an item of it, purchase orders with an item of it); proposals
  still name no supplier (§6: source determination Later), and a buyer
  who names one on the requisition takes the proposal over.
- `listPurchaseOrders` and `listContracts` take an optional
  `material_id` (procurement's API, additive): the orders and contracts
  with an item of the material that no change removed. No index is
  added: the filter tests each order or contract of the page's scan by
  its items' key, so a selective material reads the company's orders
  page by page; an index of items by material waits for a budget that
  needs it (no budget test covers these lists).

### S2n (2026-11-22)

S&OP plan versions live in this context (decided by default, ADR-0082
As built S2n, which records why not controlling's planning versions):

- Migration 000447 (03 §3.37; schema tests `db/sop_test.go`, the
  triggers also as the owner): `mrp.sop_plan` and `mrp.sop_plan_line`;
  permissions `mrp.sop.read` and `mrp.sop.write`.
- API: `createSopPlan`, `listSopPlans`, `getSopPlan`, `updateSopPlan`
  (the consensus by line key, JSON Merge Patch over a map, so a key not
  named stays), `fixSopPlan`; every write human-only (`x-agent: false`),
  in the demand forecasting and replenishment agents' never lists.
  Problems `mrp.company_invalid`, `mrp.invalid_horizon`,
  `mrp.sop_plan_empty`, `mrp.sop_plan_concurrent` (two reviews of a
  company numbered at once, `sop_plan_number_once`; retry),
  `mrp.sop_line_unknown`, `mrp.sop_plan_fixed`, with
  `mrp.invalid_quantity`, `mrp.nothing_to_change` and
  `mrp.unit_not_convertible` of S2k.
- **Reads** (additions to published APIs, §2's direction): inventory's
  `PlantIssues` (a plant's goods issues 201, 601, 643 less 202, 602 of
  days, not in transit), sales' `PlantRequirementsDue` (§2's demand by
  the day each order is due), procurement's `TransportReceipts` (stock
  transport orders within the company in transit to the plant) and
  `IntercompanyDue` (the intercompany orders the plant supplies and has
  not issued, by the day each item is due). The review reads the plant's stock,
  purchase orders' receipts and the run's proposals as a run does, and
  counts a proposal open or taken over while its requisition is still to
  be ordered (`counted`); the run's unit check (`inBase`) is shared.
- **Database rules**: a fixed version and its lines change no more and a
  fixed version gains no line (`sop_plan_fixed`, `sop_plan_line_fixed`,
  on insert, update and delete); only a line's consensus changes.
- **Locks.** A review reads everything as it is and locks nothing: it is
  a snapshot of the company's day, and the next review counts what moved
  since. Setting the consensus and fixing take the version's row lock
  (FOR UPDATE) and then update its lines; nothing else writes them, so
  there is no second order to deadlock with.
- No event (the plan's "What every slice reuses": none until a consumer
  asks); audited `mrp.sop_plan.create`, `.consensus` (each line's key
  and consensus before and after) and `.fix`.
- Walkthrough step 159, `step_sop_review`.

### Intercompany demand (batch 91, 2026-11-22; review:batch90)

A run counted only the plant's confirmed sales orders as demand, so a
plant that supplies another company's plant (S2i) proposed too little.
Decided by default:

- A run's demand at the plant is also what the intercompany orders it
  supplies (purchase orders of the receiving company naming the plant
  as `supplying_plant_id`) order and it has not issued, through
  procurement's `Orders.IntercompanyDue`, the read S2n's review uses (no
  new query; its days are not needed, as a sales order's due day is
  not). The line's `demand` holds both; the contract says so
  (`startMrpRun`, `MrpRunLine.demand`).
- Draft and pending orders count, as they count at the receiving plant
  (§2's receipts: a purchase order item whatever its release), so one
  order is counted once on each side: a receipt of the receiving plant,
  a demand of the supplying one. Once issued (643) the stock has left
  the supplying plant and the order demands nothing more there; the
  receiving plant counts it until received. Closed and cancelled orders
  and removed items demand nothing.
- No double counting with S2h: a stock transport order within the
  company is counted by a run on neither side (§6, not followed: stock
  transport orders as receipts; the issuing plant's requirement of such
  an order waits with them), and an intercompany order is a purchase
  order, never a stock transport order, so no order is in both reads
  (`TestPlanningRunIntercompany`). SAP's MRP counts a stock transport
  order's requirement at the supplying plant (its release order), from
  memory, for the SAP specialist.
