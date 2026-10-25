package web

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A parked entry's page reads the entry, the companies, the company's
// accounts, who parked and owns it, and, while it waits for approval, its
// approval request (for the withdrawal's ETag): with getCurrentPrincipal
// six calls (ADR-0052 §10).
//
// The decisions on it (T35): its owner posts it (postParkedJournalEntry,
// people's alone), which within the company's manual entry limit posts it
// at once and above it asks another person's approval, decided in the
// approvals inbox; cancels it for good with a reason; changes it or
// replaces its lines from a CSV file; and, while it waits, withdraws the
// approval request with a reason (cancelApprovalRequest), which returns
// it to parked. An agent's proposal no person owns is offered to every
// person who may act on it: acting on it takes it on (ADR-0036). Each is
// a real form with the entry's ETag (the request's, for the withdrawal)
// as If-Match and the form's own idempotency key. Done, the page says so;
// refused, it is rendered again, fresh from the API, with the refusal and
// what was typed.

// parkedTyped is what the person sent in a parked entry's form that the
// API refused or that was not valid: the verb, its reason and its error,
// and a file's rows that were wrong.
type parkedTyped struct {
	verb, text string
	textErr    i18n.Key
	upload     *ui.LinesUpload
}

// parkedEntry is a parked entry's page.
func (s *Server) parkedEntry(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	s.showParked(w, r, p, m, id, http.StatusOK, parkedTyped{}, nil)
}

// readParked reads the parked entry (getParkedJournalEntry) and its ETag.
// ok is false when the answer is already written: not found, or the
// refusal on a page of its own.
func (s *Server) readParked(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID) (gen.ParkedJournalEntry, string, bool) {
	res, err := s.API.GetParkedJournalEntry(r.Context(), gen.GetParkedJournalEntryParams{ID: id})
	if got, ok := res.(*gen.GetParkedJournalEntryOKHeaders); ok {
		return got.Response, got.ETag.Or(etag.Format(got.Response.Version)), true
	}
	s.pageRefused(w, r, p, m, "getParkedJournalEntry", res, err, i18n.ParkedTitle, i18n.ParkedRefusedTitle, ui.JournalPath, i18n.ParkedBack)
	return gen.ParkedJournalEntry{}, "", false
}

// showParked reads the entry and renders its page with status: the
// forms fresh holding what the person typed, and the API's refusal of
// what they asked.
func (s *Server) showParked(w http.ResponseWriter, r *http.Request, p *person, m me, id uuid.UUID, status int, t parkedTyped, refused *ui.Refusal) {
	pe, tag, ok := s.readParked(w, r, p, m, id)
	if !ok {
		return
	}
	ctx := r.Context()
	pp := ui.ParkedPage{Header: s.parkedHeader(ctx, p, m, pe), Lines: parkedLines(pe.Lines, s.chartOf(ctx, m, pe.LegalEntityID).names()),
		Done: parkedDone[r.URL.Query().Get("done")], Refusal: refused, Back: ui.JournalPath}
	if req, ok := pe.ApprovalRequestID.Get(); ok {
		pp.Approval = ui.ApprovalsPath + "/" + req.String()
	}
	if je, ok := pe.JournalEntryID.Get(); ok {
		pp.Journal = entryPath(je)
	}
	switch pe.Status {
	case "parked":
		parkedForms(&pp, pe, m, p, tag, t)
	case "pending_approval":
		s.withdrawForm(ctx, &pp, pe, m, p, t)
	}
	s.render(w, r, status, ui.ParkedEntry(s.page(i18n.ParkedTitle, ui.JournalPath, p, m), pp))
}

// parkedHeader is a parked entry's summary, its company named and who
// parked and owns it.
func (s *Server) parkedHeader(ctx context.Context, p *person, m me, pe gen.ParkedJournalEntry) ui.ParkedHeader {
	ids := []uuid.UUID{pe.CreatedBy}
	if owner, ok := pe.OwnerID.Get(); ok {
		ids = append(ids, owner)
	}
	names := s.names(ctx, p, ids)
	amounts := make([]gen.Money, 0, len(pe.Lines))
	for _, l := range pe.Lines {
		amounts = append(amounts, l.Amount)
	}
	h := ui.ParkedHeader{Status: labelOf(parkedStatuses, pe.Status), Company: s.companies(ctx, m)[pe.LegalEntityID], DocType: pe.DocumentType,
		DocDate: pe.DocumentDate, Posting: pe.PostingDate, Currency: pe.Currency, Reference: pe.Reference.Or(""), Header: pe.HeaderText.Or(""),
		Debits: debits(amounts), CreatedBy: names[pe.CreatedBy], Created: pe.CreatedAt}
	if d, ok := pe.TaxDate.Get(); ok {
		h.TaxDate = &d
	}
	if owner, ok := pe.OwnerID.Get(); ok {
		name := names[owner]
		h.Owner = &name
	}
	return h
}

// parkedLines are a parked entry's lines, their accounts named.
func parkedLines(lines []gen.ParkedJournalLine, accounts map[uuid.UUID]string) []ui.JournalLine {
	out := make([]ui.JournalLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, ui.JournalLine{No: l.LineNo, Account: accountText(accounts, l.AccountID), Amount: moneyOf(l.Amount),
			CostCenter: l.CostCenterCode.Or(""), ProfitCenter: l.ProfitCenterCode.Or(""), Segment: l.SegmentCode.Or(""), Partner: idText(l.PartnerID),
			TaxCode: l.TaxCode.Or(""), Text: l.LineText.Or("")})
	}
	return out
}

// parkedForms are the person's decisions on a parked entry (advisory,
// ADR-0052 §7: the API decides each call): its owner's, or every
// person's on an agent's proposal no person owns, each offered by its
// x-permission; without any, why.
func parkedForms(pp *ui.ParkedPage, pe gen.ParkedJournalEntry, m me, p *person, tag string, t parkedTyped) {
	owner, owned := pe.OwnerID.Get()
	if owned && owner != m.ID {
		pp.Why = i18n.ParkedOthers
		return
	}
	pp.TakeOn = !owned
	if m.may(permParkEntries) {
		pp.Edit = parkedPath(pe.ID) + "/edit"
		pp.Upload = &ui.LinesUpload{Action: parkedPath(pe.ID) + "/lines", CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
		if t.upload != nil {
			pp.Upload.FileError, pp.Upload.Rows, pp.Upload.More, pp.Upload.Again = t.upload.FileError, t.upload.Rows, t.upload.More, true
		}
	}
	for _, verb := range []string{ui.ParkedPost, ui.ParkedCancel} {
		if m.may(parkedOps[verb].HiddenBy) {
			pp.Actions = append(pp.Actions, parkedForm(pe.ID, verb, p, tag, t))
		}
	}
	if len(pp.Actions) == 0 && pp.Upload == nil {
		pp.Why, pp.TakeOn = i18n.ParkedReadOnly, false
	}
}

// parkedForm is a decision's form, holding what the person typed when
// they sent it.
func parkedForm(id uuid.UUID, verb string, p *person, tag string, t parkedTyped) ui.ParkedForm {
	f := ui.ParkedForm{Verb: verb, Action: parkedPath(id) + "/" + verb, CSRF: p.CSRF, ETag: tag, Key: uuid.NewString()}
	if t.verb == verb {
		f.Text, f.TextError = t.text, t.textErr
	}
	return f
}

// withdrawForm is the withdrawal of the approval request an entry waits
// for, offered to its owner, the requester, at the request's ETag; for
// anyone else, why there is nothing to do. A request that cannot be read
// is no reason to refuse the page: it is logged, and nothing is offered.
func (s *Server) withdrawForm(ctx context.Context, pp *ui.ParkedPage, pe gen.ParkedJournalEntry, m me, p *person, t parkedTyped) {
	pp.Why = i18n.ParkedWaits
	request, ok := pe.ApprovalRequestID.Get()
	if owner, owned := pe.OwnerID.Get(); !ok || !owned || owner != m.ID || !m.may(permCancelRequests) {
		return
	}
	res, err := s.API.GetApprovalRequest(ctx, gen.GetApprovalRequestParams{ID: request})
	got, ok := res.(*gen.ApprovalRequestHeaders)
	if !ok {
		s.Log.WarnContext(ctx, "web: a parked entry's approval request could not be read", "answer", res, "error", err)
		return
	}
	if got.Response.Status != "pending" {
		return
	}
	pp.Why = i18n.ParkedWaitsYours
	f := parkedForm(pe.ID, ui.ParkedWithdraw, p, got.ETag.Or(etag.Format(got.Response.Version)), t)
	f.Request = request.String()
	pp.Actions = []ui.ParkedForm{f}
}

// decideParked takes the decision verb on the parked entry.
func (s *Server) decideParked(verb string) func(http.ResponseWriter, *http.Request, *person) {
	return func(w http.ResponseWriter, r *http.Request, p *person) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			s.notFound(w, r)
			return
		}
		ifMatch, key, ok := s.formFields(w, r, parkedPath(id), i18n.ParkedBack)
		if !ok {
			return
		}
		m, ok := s.whoami(w, r, p)
		if !ok {
			return
		}
		t := parkedTyped{verb: verb, text: r.PostForm.Get(ui.FieldComment)}
		if t.textErr = reasonError(t.text, ui.ParkedRequires(verb)); t.textErr != "" {
			s.showParked(w, r, p, m, id, http.StatusUnprocessableEntity, t, nil)
			return
		}
		res, written, err := s.sendParked(w, r, p, m, verb, id, ifMatch, key, t)
		if written {
			return
		}
		switch got := res.(type) {
		case *gen.UpdateParkedJournalEntryOKHeaders:
			http.Redirect(w, r, parkedPath(id)+"?done="+parkedDoneOf(verb, got.Response), http.StatusSeeOther)
			return
		case *gen.ApproveApprovalRequestOKHeaders:
			http.Redirect(w, r, parkedPath(id)+"?done=withdrawn", http.StatusSeeOther)
			return
		}
		if prob, show := s.refusal(w, r, p, parkedOps[verb].ID, res, err); show {
			s.showParked(w, r, p, m, id, int(prob.Status), t, explainIn(i18n.EntryProblems, i18n.ParkedDecisionRefusedTitle, prob, m))
		}
	}
}

// sendParked calls the verb's operation: the withdrawal cancels the
// approval request the form names, once it is read and found to be this
// entry's. written is true when the answer is written instead: the
// request could not be read, or is not this entry's.
func (s *Server) sendParked(w http.ResponseWriter, r *http.Request, p *person, m me, verb string, id uuid.UUID, ifMatch, key string,
	t parkedTyped) (any, bool, error) {
	ctx := r.Context()
	switch verb {
	case ui.ParkedPost:
		res, err := s.API.PostParkedJournalEntry(ctx, gen.PostParkedJournalEntryParams{ID: id, IfMatch: ifMatch, IdempotencyKey: key})
		return res, false, err
	case ui.ParkedCancel:
		res, err := s.API.CancelParkedJournalEntry(ctx, &gen.ReasonInput{Reason: t.text}, gen.CancelParkedJournalEntryParams{ID: id, IfMatch: ifMatch,
			IdempotencyKey: key})
		return res, false, err
	}
	// ui.ParkedWithdraw: the routes are the verbs of parkedOps.
	request, err := uuid.Parse(r.PostForm.Get(ui.FieldRequest))
	if err != nil {
		request = uuid.Nil // the form names no request: none is this entry's
	}
	res, err := s.API.GetApprovalRequest(ctx, gen.GetApprovalRequestParams{ID: request})
	got, ok := res.(*gen.ApprovalRequestHeaders)
	if !ok {
		if refused, show := s.inPlace(w, r, p, m, i18n.EntryProblems, "getApprovalRequest", res, err, i18n.ParkedDecisionRefusedTitle, ""); show {
			s.showParked(w, r, p, m, id, refusedStatus(res), t, refused)
		}
		return nil, true, nil
	}
	if got.Response.ObjectType != "manual_entry" || got.Response.ObjectID != id {
		s.notFound(w, r) // not this entry's request
		return nil, true, nil
	}
	cres, err := s.API.CancelApprovalRequest(ctx, &gen.CommentRequiredInput{Comment: t.text}, gen.CancelApprovalRequestParams{ID: request,
		IfMatch: ifMatch, IdempotencyKey: key})
	return cres, false, err
}

// parkedDoneOf is the notice of a decision done: a post said by what it
// did, posted at once or asked approval for.
func parkedDoneOf(verb string, pe gen.ParkedJournalEntry) string {
	switch {
	case verb == ui.ParkedCancel:
		return "cancelled"
	case pe.Status == "pending_approval":
		return "submitted"
	default:
		return "posted"
	}
}

// uploadLines replaces a parked entry's lines with a CSV file's
// (updateParkedJournalEntry, its lines alone): done, the entry's page
// says so; the file or its rows wrong, or the API's refusal, the page
// says why, each wrong row by its line in the file.
func (s *Server) uploadLines(w http.ResponseWriter, r *http.Request, p *person) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	ifMatch, key, ok := s.formFields(w, r, parkedPath(id), i18n.ParkedBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	pe, _, ok := s.readParked(w, r, p, m, id)
	if !ok {
		return
	}
	ctx := r.Context()
	up := &ui.LinesUpload{}
	data, fileErr := formFile(r, i18n.CSVFileNeeded, i18n.CSVTooLarge)
	var lines []entryLine
	if fileErr == "" {
		lines, fileErr = csvLines(data)
	}
	if up.FileError = fileErr; fileErr != "" {
		s.showParked(w, r, p, m, id, http.StatusUnprocessableEntity, parkedTyped{upload: up}, nil)
		return
	}
	loc, _ := i18n.From(ctx) // every request has its locale (localized)
	sent := make([]gen.JournalLineInput, 0, len(lines))
	for i := range lines {
		lines[i].check(loc, pe.Currency)
	}
	if !s.resolve(ctx, pe.LegalEntityID, s.chartOf(ctx, m, pe.LegalEntityID), lines) {
		up.Rows, up.More = rowErrors(lines)
		s.showParked(w, r, p, m, id, http.StatusUnprocessableEntity, parkedTyped{upload: up}, nil)
		return
	}
	for _, l := range lines {
		sent = append(sent, l.sent)
	}
	res, err := s.API.UpdateParkedJournalEntry(ctx, &gen.ParkedJournalEntryUpdate{Lines: sent}, gen.UpdateParkedJournalEntryParams{ID: id,
		IfMatch: ifMatch, IdempotencyKey: key})
	if _, ok := res.(*gen.UpdateParkedJournalEntryOKHeaders); ok {
		http.Redirect(w, r, parkedPath(id)+"?done=uploaded", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "updateParkedJournalEntry", res, err); show {
		markRefused(prob, lines)
		up.Rows, up.More = rowErrors(lines)
		s.showParked(w, r, p, m, id, int(prob.Status), parkedTyped{upload: up}, explainIn(i18n.EntryProblems, i18n.UploadLinesRefusedTitle, prob, m))
	}
}
