package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A request's object as it says itself (ADR-0052 §2, T73d2): the request's
// page reads the object with the operation of its type and shows its
// summary, with its page in the workspace where there is one. Two types'
// objects are not read by the request's object id alone (T73d3): an order
// change is read through its order and a bank clearing's match through its
// statement, both placed by the request's object_resource, the API path
// its owning context gives (ADR-0034); a request whose resource does not
// place its object says so.

// objectRead is the read of one type's object: its operation in the
// inventory (hidden by its x-permission), the contract's path of that
// operation where the read follows the request's object_resource ("" for
// a read by the object id), and the call. A call that finds no object
// where the request places it answers nil and no error.
type objectRead struct {
	op   Operation
	path string
	call func(ctx context.Context, api *gen.Client, a gen.ApprovalRequest) (any, error)
}

// byObjectID is a read of the object by the request's object id.
func byObjectID(read func(context.Context, *gen.Client, uuid.UUID) (any, error)) func(context.Context, *gen.Client, gen.ApprovalRequest) (any, error) {
	return func(ctx context.Context, c *gen.Client, a gen.ApprovalRequest) (any, error) {
		return read(ctx, c, a.ObjectID)
	}
}

// The contract's paths of the reads that follow object_resource: an order
// change's, through its order, and a statement's, whose line carries a
// bank clearing's match.
const (
	changeResource    = "/v1/purchase-orders/{id}/changes/{change_id}"
	statementResource = "/v1/bank-statements/{id}"
)

// placed are the ids in the request's object_resource when it has the
// shape of the path template, each a UUID in its canonical form where the
// template names a parameter; ok is false for any other resource, or
// none.
func placed(a gen.ApprovalRequest, template string) (ids []uuid.UUID, ok bool) {
	got, want := strings.Split(a.ObjectResource.Or(""), "/"), strings.Split(template, "/")
	if len(got) != len(want) {
		return nil, false
	}
	for i, w := range want {
		if !strings.HasPrefix(w, "{") {
			if got[i] != w {
				return nil, false
			}
			continue
		}
		id, err := uuid.Parse(got[i])
		if err != nil || id.String() != got[i] {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// readChange reads an order change through the order the request places
// it in; the change must be the request's object.
func readChange(ctx context.Context, c *gen.Client, a gen.ApprovalRequest) (any, error) {
	ids, ok := placed(a, changeResource)
	if !ok || ids[1] != a.ObjectID {
		return nil, nil
	}
	return c.GetPurchaseOrderChange(ctx, gen.GetPurchaseOrderChangeParams{ID: ids[0], ChangeID: ids[1]})
}

// readClearing reads a bank clearing's match among the matches of the
// statement the request places it on: the API reads a match only with
// its statement's.
func readClearing(ctx context.Context, c *gen.Client, a gen.ApprovalRequest) (any, error) {
	ids, ok := placed(a, statementResource)
	if !ok {
		return nil, nil
	}
	res, err := c.ListBankStatementMatches(ctx, gen.ListBankStatementMatchesParams{ID: ids[0]})
	list, ok := res.(*gen.StatementMatchList)
	if !ok {
		return res, err
	}
	for _, m := range list.Items {
		if m.ID == a.ObjectID {
			return &m, nil
		}
	}
	return nil, nil
}

// objectReads are the reads of the object types the workspace summarises.
var objectReads = map[string]objectRead{
	"requisition": {op: Operation{ID: "getRequisition", HiddenBy: "procurement.requisition.read", PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetRequisition(ctx, gen.GetRequisitionParams{ID: id})
		})},
	"partner_change": {op: Operation{ID: "getPartnerChange", HiddenBy: permReadSuppliers, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPartnerChange(ctx, gen.GetPartnerChangeParams{ID: id})
		})},
	"config_change": {op: Operation{ID: "getChangeRequest", HiddenBy: "config.change.read", PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
		})},
	"invoice_exception": {op: Operation{ID: "getSupplierInvoice", HiddenBy: permReadInvoices, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetSupplierInvoice(ctx, gen.GetSupplierInvoiceParams{ID: id})
		})},
	"period_unlock": {op: Operation{ID: "getFiscalPeriod", HiddenBy: permReadPeriods, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetFiscalPeriod(ctx, gen.GetFiscalPeriodParams{ID: id})
		})},
	"manual_entry": {op: Operation{ID: "getParkedJournalEntry", HiddenBy: permReadEntries, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetParkedJournalEntry(ctx, gen.GetParkedJournalEntryParams{ID: id})
		})},
	"purchase_order": {op: Operation{ID: "getPurchaseOrder", HiddenBy: permReadOrders, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPurchaseOrder(ctx, gen.GetPurchaseOrderParams{ID: id})
		})},
	"center_change": {op: Operation{ID: "getCenterChange", HiddenBy: permReadCenters, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetCenterChange(ctx, gen.GetCenterChangeParams{ID: id})
		})},
	"gr_ir_clearing": {op: Operation{ID: "getGrIrClearing", HiddenBy: permReadOrders, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetGrIrClearing(ctx, gen.GetGrIrClearingParams{ID: id})
		})},
	"payment_run_release": {op: Operation{ID: "getPaymentRun", HiddenBy: permReadRuns, PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPaymentRun(ctx, gen.GetPaymentRunParams{ID: id})
		})},
	"order_change": {op: Operation{ID: "getPurchaseOrderChange", HiddenBy: permReadOrders, PerObject: true}, path: changeResource, call: readChange},
	"bank_clearing": {op: Operation{ID: "listBankStatementMatches", HiddenBy: permReadStatements, PerObject: true}, path: statementResource,
		call: readClearing},
	"tax_return": {op: Operation{ID: "getTaxReturn", HiddenBy: "tax.return.read", PerObject: true},
		call: byObjectID(func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetTaxReturn(ctx, gen.GetTaxReturnParams{ID: id})
		})},
}

// objectOps are the object reads in the inventory: a request's page calls
// the one of its object's type.
func objectOps() []Operation {
	ops := make([]Operation, 0, len(objectReads))
	for _, r := range objectReads {
		ops = append(ops, r.op)
	}
	return ops
}

// object reads the request's object and fills the page's object section:
// its summary; the API's refusal, in the object's words; or why there is
// none (a type the workspace does not summarise, or an object the person
// may not read). ok is false when the answer was written instead
// (apiFailed).
func (s *Server) object(w http.ResponseWriter, r *http.Request, p *person, m me, a gen.ApprovalRequest, rp *ui.RequestPage) bool {
	read, known := objectReads[a.ObjectType]
	switch {
	case !known:
		rp.ObjectNote = i18n.ObjectNoSummary
		return true
	case !m.may(read.op.HiddenBy):
		rp.ObjectNote = i18n.ObjectHidden
		return true
	}
	res, err := read.call(r.Context(), s.API, a)
	if res == nil && err == nil {
		rp.ObjectNote = i18n.ObjectNotPlaced
		return true
	}
	if sum, ok := summaryOf(res); ok {
		rp.Object = sum
		return true
	}
	refused, ok := s.inPlace(w, r, p, m, i18n.ObjectProblems, read.op.ID, res, err, i18n.ObjectRefusedTitle, "object-refusal")
	rp.ObjectRefusal = refused
	return ok
}

// summaryOf is an object's summary from its read's answer; ok is false
// for any answer that is no object.
func summaryOf(res any) (*ui.ObjectSummary, bool) {
	switch v := res.(type) {
	case *gen.GetRequisitionOKHeaders:
		return requisitionSummary(v.Response), true
	case *gen.GetPartnerChangeOKHeaders:
		c := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(c.Number)}, {Key: i18n.SummaryKind, Value: ui.Code(c.Kind)},
			{Key: i18n.SummaryStatus, Value: ui.Code(c.Status)}, {Key: i18n.SummaryReason, Value: ui.Data(c.Reason)}}}, true
	case *gen.GetChangeRequestOKHeaders:
		c := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(c.Number)}, {Key: i18n.SummaryTitle, Value: ui.Data(c.Title)},
			{Key: i18n.SummaryStatus, Value: ui.Code(c.Status)}, {Key: i18n.SummaryTier, Value: ui.Number(c.Tier)}}}, true
	case *gen.GetSupplierInvoiceOKHeaders:
		return invoiceSummary(v.Response), true
	case *gen.FiscalPeriodHeaders:
		fp := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryPeriod, Value: ui.T(i18n.SummaryPeriodOf, fp.Period, strconv.Itoa(int(fp.FiscalYear)))},
			{Key: i18n.SummaryFrom, Value: ui.Day(fp.StartDate)}, {Key: i18n.SummaryTo, Value: ui.Day(fp.EndDate)},
			{Key: i18n.SummaryStatus, Value: ui.StatusTag(labelOf(periodStatuses, fp.Status))}},
			Link: ui.PeriodsBase + "/" + fp.ID.String(), LinkText: i18n.ObjectOpenPeriod}, true
	case *gen.GetParkedJournalEntryOKHeaders:
		return entrySummary(v.Response), true
	case *gen.GetPurchaseOrderOKHeaders:
		po := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(po.DocumentNumber)},
			{Key: i18n.SummaryDate, Value: ui.Day(po.OrderDate)}, {Key: i18n.SummaryTotal, Value: ui.Amount(moneyOf(po.TotalAmount))},
			{Key: i18n.SummaryItems, Value: ui.Number(int32(len(po.Items)))}, {Key: i18n.SummaryStatus, Value: ui.Code(po.Status)}}}, true
	case *gen.CenterChangeHeaders:
		c := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryKind, Value: ui.Code(c.Kind)}, {Key: i18n.SummaryStatus, Value: ui.Code(c.Status)},
			{Key: i18n.SummaryReason, Value: ui.Data(c.Reason)}}}, true
	case *gen.GrIrClearingHeaders:
		c := v.Response
		return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryOrder, Value: ui.Code(c.PurchaseOrderNumber)},
			{Key: i18n.SummaryPosting, Value: ui.Day(c.PostingDate)}, {Key: i18n.SummaryTotal, Value: ui.Amount(moneyOf(c.Total))},
			{Key: i18n.SummaryStatus, Value: ui.Code(c.Status)}, {Key: i18n.SummaryReason, Value: ui.Data(c.Reason)}}}, true
	case *gen.PaymentRunHeaders:
		return runSummary(v.Response), true
	case *gen.GetTaxReturnOKHeaders:
		return returnSummary(v.Response), true
	case *gen.GetPurchaseOrderChangeOKHeaders:
		return changeSummary(v.Response), true
	case *gen.StatementMatch:
		return clearingSummary(*v), true
	}
	return nil, false
}

// requisitionSummary is a requisition's summary.
func requisitionSummary(q gen.Requisition) *ui.ObjectSummary {
	rows := []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(q.DocumentNumber)}, {Key: i18n.SummaryDate, Value: ui.Day(q.RequisitionDate)}}
	if d, ok := q.NeededBy.Get(); ok {
		rows = append(rows, ui.Row{Key: i18n.SummaryNeededBy, Value: ui.Day(d)})
	}
	return &ui.ObjectSummary{Rows: append(rows, ui.Row{Key: i18n.SummaryTotal, Value: ui.Amount(moneyOf(q.TotalAmount))},
		ui.Row{Key: i18n.SummaryItems, Value: ui.Number(int32(len(q.Items)))},
		ui.Row{Key: i18n.SummaryStatus, Value: ui.Code(q.Status)})}
}

// invoiceSummary is a supplier invoice's summary, linked to its page.
func invoiceSummary(v gen.SupplierInvoice) *ui.ObjectSummary {
	rows := []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(v.DocumentNumber)}, {Key: i18n.SummaryReference, Value: ui.Data(v.SupplierReference)},
		{Key: i18n.SummaryDate, Value: ui.Day(v.InvoiceDate)}, {Key: i18n.SummaryGross, Value: ui.Amount(moneyOf(v.GrossAmount))},
		{Key: i18n.SummaryStatus, Value: ui.StatusTag(labelOf(invoiceStatuses, v.Status))}}
	if h, ok := v.HoldReason.Get(); ok && h != "" {
		rows = append(rows, ui.Row{Key: i18n.SummaryHold, Value: ui.Data(h)})
	}
	return &ui.ObjectSummary{Rows: rows, Link: invoicePath(v.ID), LinkText: i18n.ObjectOpenInvoice}
}

// entrySummary is a parked journal entry's summary, linked to its page.
func entrySummary(pe gen.ParkedJournalEntry) *ui.ObjectSummary {
	rows := []ui.Row{{Key: i18n.SummaryDocType, Value: ui.Code(pe.DocumentType)}, {Key: i18n.SummaryPosting, Value: ui.Day(pe.PostingDate)},
		{Key: i18n.SummaryCurrency, Value: ui.Code(pe.Currency)}}
	if ref, ok := pe.Reference.Get(); ok && ref != "" {
		rows = append(rows, ui.Row{Key: i18n.SummaryEntryReference, Value: ui.Data(ref)})
	}
	if h, ok := pe.HeaderText.Get(); ok && h != "" {
		rows = append(rows, ui.Row{Key: i18n.SummaryHeader, Value: ui.Data(h)})
	}
	rows = append(rows, ui.Row{Key: i18n.SummaryLines, Value: ui.Number(int32(len(pe.Lines)))},
		ui.Row{Key: i18n.SummaryStatus, Value: ui.StatusTag(labelOf(parkedStatuses, pe.Status))})
	return &ui.ObjectSummary{Rows: rows, Link: parkedPath(pe.ID), LinkText: i18n.ObjectOpenEntry}
}

// runSummary is a payment run's summary, linked to its page.
func runSummary(pr gen.PaymentRun) *ui.ObjectSummary {
	return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryNumber, Value: ui.Code(pr.DocumentNumber)}, {Key: i18n.SummaryRunDate, Value: ui.Day(pr.RunDate)},
		{Key: i18n.SummaryDueBy, Value: ui.Day(pr.DueBy)}, {Key: i18n.SummaryTotal, Value: ui.Amount(moneyOf(pr.TotalAmount))},
		{Key: i18n.SummaryStatus, Value: ui.StatusTag(labelOf(runStatuses, pr.Status))}}, Link: runPath(pr.ID), LinkText: i18n.ObjectOpenRun}
}

// returnSummary is a tax return's summary; the workspace has no page of
// its own for a return.
func returnSummary(tr gen.TaxReturn) *ui.ObjectSummary {
	return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryTitle, Value: ui.Data(tr.ReturnName)},
		{Key: i18n.SummaryRegistration, Value: ui.Code(tr.RegistrationCode)}, {Key: i18n.SummaryReturnKind, Value: ui.Code(tr.Kind)},
		{Key: i18n.SummaryFrom, Value: ui.Day(tr.PeriodStart)}, {Key: i18n.SummaryTo, Value: ui.Day(tr.PeriodEnd)},
		{Key: i18n.SummaryItems, Value: ui.Data(strconv.FormatInt(tr.ItemCount, 10))}, {Key: i18n.SummaryStatus, Value: ui.Code(tr.Status)}}}
}

// changeSummary is an order change's summary: the order as it is to read
// against what it read; the workspace has no page of its own for an
// order.
func changeSummary(ch gen.PurchaseOrderChange) *ui.ObjectSummary {
	return &ui.ObjectSummary{Rows: []ui.Row{{Key: i18n.SummaryReason, Value: ui.Data(ch.Reason)},
		{Key: i18n.SummaryTotalBefore, Value: ui.Amount(moneyOf(ch.TotalBefore))}, {Key: i18n.SummaryTotalAfter, Value: ui.Amount(moneyOf(ch.TotalAfter))},
		{Key: i18n.SummaryDate, Value: ui.Day(ch.OrderDate)}, {Key: i18n.SummaryTerms, Value: ui.Code(ch.PaymentTermCode)},
		{Key: i18n.SummaryItems, Value: ui.Number(int32(len(ch.Items)))}, {Key: i18n.SummaryStatus, Value: ui.Code(ch.Status)}}}
}

// clearingSummary is a bank clearing's summary: the statement line's match
// whose posting waits, its reason shown as data (a person's, or the text
// of an agent's proposal a person accepted), linked to the line on its
// statement's page.
func clearingSummary(m gen.StatementMatch) *ui.ObjectSummary {
	rows := []ui.Row{{Key: i18n.LineNo, Value: ui.Number(m.LineNo)}, {Key: i18n.SummaryMatched, Value: ui.Labelled(labelOf(matchKinds, m.Kind))}}
	if rule, ok := m.Rule.Get(); ok {
		rows = append(rows, ui.Row{Key: i18n.MatchRule, Value: ui.Labelled(labelOf(matchRules, rule))})
	}
	rows = append(rows, ui.Row{Key: i18n.SummaryReason, Value: ui.Data(m.Reason)}, ui.Row{Key: i18n.SummaryClears, Value: clears(m.Targets)})
	if d, ok := m.PostingDate.Get(); ok {
		rows = append(rows, ui.Row{Key: i18n.MatchPostingDay, Value: ui.Day(d)})
	}
	if why, ok := m.PostingReason.Get(); ok && why != "" {
		rows = append(rows, ui.Row{Key: i18n.MatchPostingReason, Value: ui.Data(why)})
	}
	return &ui.ObjectSummary{Rows: append(rows, ui.Row{Key: i18n.SummaryStatus, Value: ui.Labelled(labelOf(matchStatuses, m.Status))}),
		Link: statementPath(m.BankStatementID) + "#" + lineAnchor(m.LineNo), LinkText: i18n.ObjectOpenLine}
}

// clears says what a match clears: its payments, a posting rule, the
// incoming payments clearing account, or nothing.
func clears(targets []gen.StatementMatchTarget) templ.Component {
	payments := 0
	for _, t := range targets {
		if _, ok := t.PaymentID.Get(); ok {
			payments++
		}
	}
	switch {
	case payments > 0:
		return ui.T(i18n.SummaryClearsPayments, payments)
	case len(targets) == 0:
		return ui.T(i18n.SummaryClearsNothing)
	case targets[0].Incoming.Or(false):
		return ui.T(i18n.TargetIncoming)
	}
	return ui.T(i18n.SummaryClearsRule, targets[0].PostingRule.Or(""))
}
