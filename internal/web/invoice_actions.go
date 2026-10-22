package web

import (
	"bufio"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// The actions a person takes on an invoice (ADR-0052 §7): each a real
// form with the invoice's ETag as If-Match and the form's own
// idempotency key, so a double click or a resubmit replays the first
// answer. Done, the invoice's page says so; refused, it is rendered
// again, fresh from the API, with the refusal and what was typed.

// invoiceDoneOf is the notice of each action done.
var invoiceDoneOf = map[string]string{ui.ActMatch: "matched", ui.ActPost: "posted", ui.ActHold: "held", ui.ActRelease: "released",
	ui.ActCancel: "cancelled", ui.ActReverse: "reversed"}

// formFields are a posted form's If-Match and idempotency key; ok is
// false, the answer written, when the form is not the page's.
func (s *Server) formFields(w http.ResponseWriter, r *http.Request, back string) (ifMatch, key string, ok bool) {
	// csrf parsed the form unless htmx sent the token as a header; a body
	// that does not parse leaves the fields empty, refused below.
	_ = r.ParseForm()
	ifMatch, key = r.PostForm.Get(ui.FieldIfMatch), r.PostForm.Get(ui.FieldIdempotency)
	if _, err := uuid.Parse(key); err != nil || ifMatch == "" {
		// Not the page's form: nothing to keep or explain.
		s.render(w, r, http.StatusBadRequest, ui.Problem(s.shell(i18n.RefusedTitle, ""), i18n.RefusedTitle, i18n.ProblemInvoiceBody, back, i18n.OrderBack))
		return "", "", false
	}
	return ifMatch, key, true
}

// act takes the action verb on the invoice.
func (s *Server) act(verb string) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		ifMatch, key, ok := s.formFields(w, r, invoicePath(id))
		if !ok {
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		t := typed{verb: verb, text: r.PostForm.Get(ui.FieldComment), date: strings.TrimSpace(r.PostForm.Get(ui.FieldDate))}
		t.textErr = reasonError(t.text, ui.Requires(verb))
		date, dateErr := dateParam(t.date)
		t.dateErr = dateErr
		if dateErr == i18n.FilterDateInvalid {
			t.dateErr = i18n.ActionDateInvalid
		}
		if t.textErr != "" || t.dateErr != "" {
			s.showInvoice(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
			return
		}
		res, err := s.sendAction(r, verb, id, ifMatch, key, t.text, date)
		switch res.(type) {
		case *gen.UpdateSupplierInvoiceOKHeaders, *gen.MatchSupplierInvoiceOKHeaders:
			http.Redirect(w, r, invoicePath(id)+"?done="+invoiceDoneOf[verb], http.StatusSeeOther)
			return
		}
		if prob, show := s.refusal(w, r, p, actionOps[verb].ID, res, err); show {
			s.showInvoice(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.InvoiceProblems, i18n.InvoiceRefusedTitle, prob, m))
		}
	}
}

// sendAction calls the verb's operation: a reason where it needs one, a
// comment or a date where it takes one.
func (s *Server) sendAction(r *http.Request, verb string, id uuid.UUID, ifMatch, key, text string, date gen.OptDate) (any, error) {
	ctx := r.Context()
	switch verb {
	case ui.ActMatch:
		return s.API.MatchSupplierInvoice(ctx, gen.MatchSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.ActPost:
		var body gen.OptSupplierInvoicePost
		if date.Set {
			body = gen.NewOptSupplierInvoicePost(gen.SupplierInvoicePost{PostingDate: date})
		}
		return s.API.PostSupplierInvoice(ctx, body, gen.PostSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.ActHold:
		return s.API.HoldSupplierInvoice(ctx, &gen.ReasonInput{Reason: text}, gen.HoldSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.ActRelease:
		var body gen.OptDecisionInput
		if text != "" {
			body = gen.NewOptDecisionInput(gen.DecisionInput{Comment: gen.NewOptString(text)})
		}
		return s.API.ReleaseSupplierInvoice(ctx, body, gen.ReleaseSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	case ui.ActCancel:
		return s.API.CancelSupplierInvoice(ctx, &gen.ReasonInput{Reason: text}, gen.CancelSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	default: // ui.ActReverse: the routes are the verbs of invoiceActions
		return s.API.ReverseSupplierInvoice(ctx, &gen.SupplierInvoiceReverse{Reason: text, PostingDate: date},
			gen.ReverseSupplierInvoiceParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
	}
}

// reasonError is what is wrong with an action's text, checked before the
// API is called so it stays on the page: a reason is required where the
// action needs one (ReasonInput), and any text has at most 2,000
// characters.
func reasonError(text string, required bool) i18n.Key {
	switch {
	case required && strings.TrimSpace(text) == "":
		return i18n.ActionReasonNeeded
	case utf8.RuneCountInString(text) > maxComment:
		return i18n.DecideTooLong
	}
	return ""
}

// decideRecommendation records what the person did with an agent's
// recommendation on the invoice (decideRecommendation, human-only):
// accepted, overridden or ignored, with an optional comment.
func (s *Server) decideRecommendation(w http.ResponseWriter, r *http.Request, p *person) {
	id, err1 := uuid.Parse(r.PathValue("id"))
	rec, err2 := uuid.Parse(r.PathValue("rec"))
	if err1 != nil || err2 != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, invoicePath(id))
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t := typed{rec: rec.String(), outcome: r.PostForm.Get(ui.FieldOutcome), text: r.PostForm.Get(ui.FieldComment)}
	if !slices.ContainsFunc(ui.RecommendationOutcomes, func(o ui.Option) bool { return o.Value == t.outcome }) {
		t.outcomeErr = i18n.RecOutcomeNeeded
	}
	if t.textErr = reasonError(t.text, false); t.textErr != "" || t.outcomeErr != "" {
		s.showInvoice(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
		return
	}
	body := &gen.RecommendationDecide{Outcome: t.outcome}
	if t.text != "" {
		body.Comment = gen.NewOptString(t.text)
	}
	res, err := s.API.DecideRecommendation(r.Context(), body, gen.DecideRecommendationParams{ID: rec, IfMatch: ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.DecideRecommendationOKHeaders); ok {
		http.Redirect(w, r, invoicePath(id)+"?done=decided", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "decideRecommendation", res, err); show {
		s.showInvoice(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.InvoiceProblems, i18n.RefusalTitle, prob, m))
	}
}

// order is an invoice's purchase order and its goods receipts: the
// section htmx loads into the invoice's page (HX-Request), or a page of
// its own for a browser without htmx.
func (s *Server) order(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Add("Vary", "HX-Request") // its refusals too: a page or a section
	fragment := r.Header.Get("HX-Request") == "true"
	var m me
	if !fragment {
		var ok bool
		if m, ok = s.whoami(w, r, p); !ok {
			return
		}
	}
	o, ok := s.orderView(w, r, p, m, id)
	if !ok {
		return
	}
	if fragment {
		s.render(w, r, http.StatusOK, ui.OrderSection(o))
		return
	}
	s.render(w, r, http.StatusOK, ui.OrderPage(s.page(i18n.OrderTitle, ui.InvoicesPath, p, m), o))
}

// orderView reads the invoice's order (getPurchaseOrder) and its goods
// receipts (listGoodsReceipts). ok is false when the answer is already
// written; a refusal is shown in their place.
func (s *Server) orderView(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID) (ui.OrderView, bool) {
	o := ui.OrderView{Back: invoicePath(id)}
	refused := func(op string, res any, err error) bool {
		prob, show := s.refusal(w, r, p, op, res, err)
		switch {
		case !show:
		case prob.Status == http.StatusNotFound:
			s.notFound(w, r)
			return false
		default:
			o.Refusal = explainIn(i18n.InvoiceProblems, i18n.OrderRefusedTitle, prob, m)
			o.Refusal.ID = "order-refusal"
		}
		return show
	}
	res, err := s.API.GetSupplierInvoice(r.Context(), gen.GetSupplierInvoiceParams{ID: id})
	inv, ok := res.(*gen.GetSupplierInvoiceOKHeaders)
	if !ok {
		return o, refused("getSupplierInvoice", res, err)
	}
	poID, ok := inv.Response.PurchaseOrderID.Get()
	if !ok {
		s.notFound(w, r) // an invoice on no order has no order to show
		return o, false
	}
	pres, err := s.API.GetPurchaseOrder(r.Context(), gen.GetPurchaseOrderParams{ID: poID})
	po, ok := pres.(*gen.GetPurchaseOrderOKHeaders)
	if !ok {
		return o, refused("getPurchaseOrder", pres, err)
	}
	gres, err := s.API.ListGoodsReceipts(r.Context(), gen.ListGoodsReceiptsParams{PurchaseOrderID: gen.NewOptUUID(poID), PageSize: gen.NewOptInt32(maxReceipts)})
	grs, ok := gres.(*gen.GoodsReceiptPageHeaders)
	if !ok {
		return o, refused("listGoodsReceipts", gres, err)
	}
	o.Header, o.Items = orderHeader(po.Response), orderItems(po.Response)
	o.Receipts = receiptsOf(grs.Response.Items, poID)
	return o, true
}

// maxReceipts is the most goods receipts an order's section lists.
const maxReceipts = 100

// orderHeader is an order's summary.
func orderHeader(po gen.PurchaseOrder) ui.OrderHeader {
	return ui.OrderHeader{Number: po.DocumentNumber, Status: ui.Label{Raw: po.Status}, Date: po.OrderDate, Total: moneyOf(po.TotalAmount)}
}

// orderItems are an order's items, ordered and received.
func orderItems(po gen.PurchaseOrder) []ui.OrderItemView {
	out := make([]ui.OrderItemView, 0, len(po.Items))
	for i, it := range po.Items {
		v := ui.OrderItemView{No: it.ItemNo.Or(int32(i + 1)), Description: it.Description, Ordered: it.Quantity.Value, Unit: it.Quantity.Unit,
			Price: moneyOf(it.UnitPrice), Received: "0", TaxCode: it.TaxCode.Or("")}
		if q, ok := it.ReceivedQuantity.Get(); ok {
			v.Received = q.Value
		}
		out = append(out, v)
	}
	return out
}

// receiptsOf are the receipts' items of the order, one row each.
func receiptsOf(grs []gen.GoodsReceipt, po uuid.UUID) []ui.ReceiptView {
	var out []ui.ReceiptView
	for _, gr := range grs {
		for _, it := range gr.Items {
			if it.PurchaseOrderID == po {
				out = append(out, ui.ReceiptView{Number: gr.DocumentNumber, Date: gr.PostingDate, OrderItem: it.PoItemNo,
					Quantity: it.Quantity.Value, Unit: it.Quantity.Unit})
			}
		}
	}
	return out
}

// documentCSP is the policy of an invoice's document (ADR-0052 §7): a
// sandbox, so a hostile PDF, image or XML cannot script the workspace's
// origin, nothing loaded but its own images, framed by the workspace's
// own pages alone.
const documentCSP = "sandbox; default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; frame-ancestors 'self'"

// document is the document an invoice was captured from
// (getSupplierInvoiceDocument), untrusted bytes served sandboxed: inline
// as itself only when it is a PDF, PNG or JPEG, an XML document as plain
// text, anything else as a download. The API answers application/
// octet-stream, so the type is detected here as capture detected it
// (http.DetectContentType).
func (s *Server) document(w http.ResponseWriter, r *http.Request, p *person) {
	h := w.Header()
	h.Set("Content-Security-Policy", documentCSP)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	res, err := s.API.GetSupplierInvoiceDocument(r.Context(), gen.GetSupplierInvoiceDocumentParams{ID: id})
	doc, ok := res.(*gen.GetSupplierInvoiceDocumentOKHeaders)
	if !ok {
		prob, show := s.refusal(w, r, p, "getSupplierInvoiceDocument", res, err)
		switch {
		case !show:
		case prob.Status == http.StatusNotFound:
			s.notFound(w, r)
		default:
			s.render(w, r, int(prob.Status), ui.RefusalPage(s.shell(i18n.DocumentTitle, ""), i18n.DocumentTitle, invoicePath(id), i18n.OrderBack,
				*explainIn(i18n.InvoiceProblems, i18n.DocumentTitle, prob, me{})))
		}
		return
	}
	body := bufio.NewReaderSize(doc.Response.Data, 512)
	head, _ := body.Peek(512) // fewer bytes for a shorter document; the client holds it in memory, so reading cannot fail
	ctype, disposition := documentType(head)
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", disposition+`; filename="`+id.String()+`"`)
	h.Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, body) // the headers are sent: a failed write is the browser gone, with no one left to tell
}

// documentType is how a document is served: its type, and inline or as
// a download.
func documentType(head []byte) (string, string) {
	switch ct := http.DetectContentType(head); {
	case ct == "application/pdf", ct == "image/png", ct == "image/jpeg":
		return ct, "inline"
	case strings.HasPrefix(ct, "text/xml"), strings.HasPrefix(ct, "text/plain"):
		return "text/plain; charset=utf-8", "inline"
	default:
		return "application/octet-stream", "attachment"
	}
}
