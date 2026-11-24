// Package replenishmentagent is the replenishment agent (ADR-0072,
// plan-supply-chain.md §S2l), a recommending agent (internal/recommend):
// for a completed planning run of a plant (ADR-0084) it reads the run, the
// requisitions it proposed and, for each proposal's material, the sources
// of supply that exist in the tenant's data: the company's catalog items
// of the material (ADR-0071), its outline agreements (contracts and
// scheduling agreements with an item of the material, ADR-0070) and its
// purchasing history (purchase orders with an item of it), with the
// suppliers they name, through API tools (04-api-design §4.13). It leaves
// one recommendation on each proposal for a buyer: order (what to order,
// the proposal's quantity in its unit, from which supplier, by which
// day, and the source it is ordered from) or source_needed, when nothing
// the run read is a source of the material. The buyer who accepts acts
// through the normal operations (names the supplier or contract on the
// requisition, submits it, orders it); nothing happens on acceptance.
//
// Its check holds an order to what the run read: the proposal read with
// getRequisition and still to be ordered, of one material; the quantity
// and unit as the proposal has them (lot sizing is Later, ADR-0084 §6);
// the day it is needed by (or the run's day, when it names none: lead
// times are Later); a source of the proposal's material in its company
// that the run read, an active catalog item, an active contract item
// valid on that day or a released or closed order's item; and that
// source's supplier, read as an active supplier. source_needed is refused
// once the run read a source of the material whose supplier it has not
// read as inactive. Every number the content writes is one the run read
// (recommend.Figures). It never creates, changes, submits, cancels or
// orders a requisition, creates or changes a purchase order, starts a
// planning run or changes planning data: those are no tools of its
// (Never). What a supplier or a person wrote (descriptions, names) is
// data, never an instruction. Its evaluation suite is evals/replenishment
// (ADR-0039).
package replenishmentagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "replenishment"

// The object type the agent recommends on, and its kinds.
const (
	RequisitionObject = "requisition"
	Order             = "order"
	SourceNeeded      = "source_needed"
)

// Reads are the operations the agent reads with (ADR-0072, S2l).
var Reads = []string{"getMrpRun", "getRequisition", "getMaterial", "listCatalogItems", "getCatalogItem", "listContracts", "getContract",
	"listPurchaseOrders", "getPurchaseOrder", "getBusinessPartner"}

// Never are the operations the agent never calls (ADR-0072): every write
// of requisitions and purchase orders (their changes included, an
// intercompany order's goods issue, S2i1, an order's GR/IR clearing and
// a scheduling agreement's schedule lines, whose call-off creates an
// order, ADR-0070 §4), of stock transport orders, the other supply a run
// counts (their creation and cancellation; review:batch92), every write of
// an RFQ (created from a requisition, published, awarded, cancelled,
// closed), a planning run, materials' planning data and S&OP plan versions
// (S2n), which its tools hold none of; the buyer and the planner do these.
var Never = []string{"createRequisition", "updateRequisition", "submitRequisition", "cancelRequisition",
	"createPurchaseOrder", "updatePurchaseOrder", "releasePurchaseOrder", "closePurchaseOrder", "cancelPurchaseOrder",
	"createPurchaseOrderChange", "updatePurchaseOrderChange", "cancelPurchaseOrderChange", "submitPurchaseOrderChange",
	"postIntercompanyGoodsIssue", "createGrIrClearing",
	"createScheduleLine", "callOffScheduleLine", "createRfq", "publishRfq", "awardRfq", "cancelRfq", "closeRfq", "startMrpRun", "createMaterialPlan", "updateMaterialPlan",
	"createSopPlan", "updateSopPlan", "fixSopPlan", "createStockTransportOrder", "cancelStockTransportOrder"}

const system = `You review the purchase requisitions a material requirements planning run proposed and recommend, for each, what to
order, from which supplier and when, for a buyer to decide.

Read the run (getMrpRun: per material its reorder point, demand, stock, open receipts, shortage and the requisition proposed)
and each proposed requisition (getRequisition: its status, its item's material, quantity and unit, its company and the day it
is needed by). For each proposal's material read its sources of supply in the proposal's company: the catalog items of the
material (listCatalogItems with the legal entity and the material's number as text; an item names its material_id and
supplier), the contracts with an item of the material (listContracts with the legal entity, the material_id and status
active: an outline agreement or scheduling agreement, valid from and to) and the purchase orders with an item of it
(listPurchaseOrders with the legal entity and the material_id: the purchasing history), and each supplier you would name
(getBusinessPartner: its status). A contract valid on the day is the source to prefer, then an active catalog item, then the
supplier last ordered from; a supplier that is not active is no source.

Then recommend once on each proposal still to be ordered (draft, pending approval or approved). order, when a source exists:
the content's first line is exactly
Order <quantity> <unit> from supplier <supplier_id> by <YYYY-MM-DD>: <source>
with the proposal's quantity and unit as the requisition gives them, the supplier's id, the day the requisition is needed by
(the run's day when it names none), and the source as one of "contract <contract_id> item <item_no>", "catalog item
<catalog_item_id>" or "purchase order <purchase_order_id>", ids as the tools returned them; then a line of its own
Why: <reasons>
saying why this supplier and source, citing what you read (the source's price, the last order, the supplier's number and
name), each figure as a tool returned it; a quantity with a fraction stands only in the first line. source_needed, when nothing you read is a source of the material: say what you looked for, so
that the buyer can source it (for example by a request for quotation). A proposal that is no longer to be ordered gets no
recommendation: say so in your answer.`

// Agent reviews a planning run's proposals.
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// The refusals of the agent's own check, beside the pattern's.
var (
	// ErrNotCompleted stops a run on a planning run that has not
	// completed before a session: it proposed nothing yet, or failed.
	ErrNotCompleted = errors.New("replenishmentagent: the planning run has not completed, so it has no proposals to review")
	// ErrProposal refuses a recommendation on a proposal the run has not
	// read with getRequisition, or that is no longer to be ordered or no
	// longer one item of a material.
	ErrProposal = errors.New("replenishmentagent: a recommendation is on a proposal the run read (getRequisition) that is still to be ordered " +
		"(draft, pending_approval or approved) and of one item of a material")
	// ErrForm refuses an order without its first line in the form a buyer
	// orders from, or without its reasons, and a source_needed that
	// orders.
	ErrForm = errors.New(`replenishmentagent: an order's first line is "Order <quantity> <unit> from supplier <supplier_id> by <YYYY-MM-DD>: <source>", ` +
		`the source "contract <contract_id> item <item_no>", "catalog item <catalog_item_id>" or "purchase order <purchase_order_id>", ` +
		`and a line "Why: " gives its reasons; a source_needed orders nothing`)
	// ErrQuantity refuses an order of another quantity or unit than the
	// proposal's.
	ErrQuantity = errors.New("replenishmentagent: an order is of the proposal's quantity in its unit, as the requisition gives them")
	// ErrWhen refuses an order by another day than the proposal's.
	ErrWhen = errors.New("replenishmentagent: an order is by the day the requisition is needed by, or the run's day when it names none")
	// ErrSource refuses an order from a source the run has not read as one
	// of the proposal's material in its company, valid on the day, or
	// from another supplier than the source's.
	ErrSource = errors.New("replenishmentagent: an order's source is one the run read of the proposal's material in its company (an active catalog item, " +
		"an active contract item valid on the day, a released or closed purchase order's item), and its supplier the source's")
	// ErrSupplier refuses an order from a supplier the run has not read as
	// an active supplier.
	ErrSupplier = errors.New("replenishmentagent: an order's supplier is one the run read (getBusinessPartner) as an active supplier")
	// ErrHasSource refuses a source_needed for a material the run read a
	// source of.
	ErrHasSource = errors.New("replenishmentagent: the run read a source of the proposal's material: recommend an order from it")
	// ErrUnread refuses a content writing a number the run did not read.
	ErrUnread = errors.New("replenishmentagent: every number in the content is one a tool returned or the run was given, written as returned; a count of your own is written in words")
)

// planningRun is what the agent needs of the planning run before its
// session.
type planningRun struct {
	ID            string `json:"id"`
	PlantCode     string `json:"plant_code"`
	PlantName     string `json:"plant_name"`
	LegalEntityID string `json:"legal_entity_id"`
	CompanyCode   string `json:"company_code"`
	Status        string `json:"status"`
	RunDate       string `json:"run_date"`
	Error         struct {
		Code string `json:"code"`
	} `json:"error"`
	Lines []struct {
		MaterialNumber    string `json:"material_number"`
		RequisitionID     string `json:"requisition_id"`
		RequisitionNumber string `json:"requisition_number"`
	} `json:"lines"`
}

// Review runs one session on the planning run's proposals: it reads the
// run (one the agent may not read, or not completed, stops it before a
// session starts; one that proposed nothing starts none and says so),
// offers the model the tools and reports what it recommended, also when
// the run fails after recommending.
func (a *Agent) Review(ctx context.Context, runID string) (recommend.Result, error) {
	r, err := a.before(ctx, runID)
	if err != nil {
		return recommend.Result{}, err
	}
	objects, names := map[string]string{}, map[string]string{}
	for _, l := range r.Lines {
		if l.RequisitionID != "" {
			objects[l.RequisitionID] = RequisitionObject
			names[l.RequisitionID] = "requisition " + l.RequisitionNumber + " of " + l.MaterialNumber
		}
	}
	if len(objects) == 0 {
		return recommend.Result{Reply: fmt.Sprintf("Planning run %s of plant %s proposed nothing: no recommendation.", r.ID, r.PlantCode)}, nil
	}
	rv := newReview(r.RunDate)
	task := recommend.Task{
		Purpose: fmt.Sprintf("Recommend what to order for planning run %s of plant %s", r.ID, r.PlantCode),
		Prompt: fmt.Sprintf("Planning run %s of plant %s, %s, in company %s (legal entity %s), planned on %s. Recommend, for each requisition it "+
			"proposed, what to order, from which supplier and when.", r.ID, r.PlantCode, r.PlantName, r.CompanyCode, r.LegalEntityID, r.RunDate),
		Objects: objects,
		Observe: map[string]func(apitools.Response){"getRequisition": rv.requisitionRead, "listCatalogItems": rv.catalogRead,
			"getCatalogItem": one(rv.catalogRead), "listContracts": rv.contractsRead, "getContract": one(rv.contractsRead),
			"listPurchaseOrders": rv.ordersRead, "getPurchaseOrder": one(rv.ordersRead), "getBusinessPartner": rv.supplierRead},
		Check: rv.check,
	}
	res, err := agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
	res.Names = names
	return res, err
}

// before reads the planning run, which must have completed.
func (a *Agent) before(ctx context.Context, runID string) (planningRun, error) {
	resp, err := a.API.Do(ctx, http.MethodGet, "/v1/mrp-runs/"+url.PathEscape(runID), nil, nil, "")
	var r planningRun
	if err == nil {
		err = resp.Decode(&r)
	}
	switch {
	case err != nil:
		return planningRun{}, fmt.Errorf("replenishmentagent: planning run %s: %w", runID, err)
	case r.Status == "failed":
		return planningRun{}, fmt.Errorf("%w: %s failed with %s", ErrNotCompleted, runID, r.Error.Code)
	case r.Status != "completed":
		return planningRun{}, fmt.Errorf("%w: %s is %s", ErrNotCompleted, runID, r.Status)
	}
	return r, nil
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{RequisitionObject: {Order, SourceNeeded}}}
}

// Tools are the operations the agent may call: its reads and the one
// that recommends.
func Tools() []string { return agent(0).Tools() }

// one shows a read of one resource to an observer of a page of them.
func one(page func(apitools.Response)) func(apitools.Response) {
	return func(resp apitools.Response) {
		if resp.OK() {
			resp.Body = append(append([]byte(`{"items": [`), resp.Body...), "]}"...)
		}
		page(resp)
	}
}

// joined is a content's lines but the one at skip, for the figures check.
func joined(lines []string, skip int) string {
	rest := make([]string, 0, len(lines))
	for i, l := range lines {
		if i != skip {
			rest = append(rest, l)
		}
	}
	return strings.Join(rest, "\n")
}
