package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A request's object as it says itself (ADR-0052 §2, T73d2): the request's
// page reads the object with the operation of its type and shows its
// summary, with its page in the workspace where there is one. Two types
// have no operation that reads the object by the request's object id
// alone: an order change (getPurchaseOrderChange needs its order's id
// too) and a bank clearing (its object is a statement line's match, which
// no operation reads by id); their pages say so.

// objectRead is the read of one type's object: its operation in the
// inventory (hidden by its x-permission) and the call.
type objectRead struct {
	op   Operation
	call func(ctx context.Context, api *gen.Client, id uuid.UUID) (any, error)
}

// objectReads are the reads of the object types the workspace summarises.
var objectReads = map[string]objectRead{
	"requisition": {Operation{ID: "getRequisition", HiddenBy: "procurement.requisition.read", PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetRequisition(ctx, gen.GetRequisitionParams{ID: id})
		}},
	"partner_change": {Operation{ID: "getPartnerChange", HiddenBy: permReadSuppliers, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPartnerChange(ctx, gen.GetPartnerChangeParams{ID: id})
		}},
	"config_change": {Operation{ID: "getChangeRequest", HiddenBy: "config.change.read", PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
		}},
	"invoice_exception": {Operation{ID: "getSupplierInvoice", HiddenBy: permReadInvoices, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetSupplierInvoice(ctx, gen.GetSupplierInvoiceParams{ID: id})
		}},
	"period_unlock": {Operation{ID: "getFiscalPeriod", HiddenBy: permReadPeriods, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetFiscalPeriod(ctx, gen.GetFiscalPeriodParams{ID: id})
		}},
	"manual_entry": {Operation{ID: "getParkedJournalEntry", HiddenBy: permReadEntries, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetParkedJournalEntry(ctx, gen.GetParkedJournalEntryParams{ID: id})
		}},
	"purchase_order": {Operation{ID: "getPurchaseOrder", HiddenBy: permReadOrders, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPurchaseOrder(ctx, gen.GetPurchaseOrderParams{ID: id})
		}},
	"center_change": {Operation{ID: "getCenterChange", HiddenBy: permReadCenters, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetCenterChange(ctx, gen.GetCenterChangeParams{ID: id})
		}},
	"gr_ir_clearing": {Operation{ID: "getGrIrClearing", HiddenBy: permReadOrders, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetGrIrClearing(ctx, gen.GetGrIrClearingParams{ID: id})
		}},
	"payment_run_release": {Operation{ID: "getPaymentRun", HiddenBy: permReadRuns, PerObject: true},
		func(ctx context.Context, c *gen.Client, id uuid.UUID) (any, error) {
			return c.GetPaymentRun(ctx, gen.GetPaymentRunParams{ID: id})
		}},
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
	res, err := read.call(r.Context(), s.API, a.ObjectID)
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
		rows = append(rows, ui.Row{Key: i18n.SummaryReference, Value: ui.Data(ref)})
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
