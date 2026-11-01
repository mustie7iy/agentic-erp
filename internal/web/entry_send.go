package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A new entry is parked (parkJournalEntry), or posted at once
// (postJournalEntry, T73h2), each with the form's idempotency key. A post
// above the company's manual entry limit is refused by the API
// (ledger.approval_required: an entry the person posts at once is within
// the limit), and the same entry is then parked for them, to be posted
// from its page, which asks another person's approval (T35). A new
// entry may also be parked with a CSV file's lines in place of those
// typed, under the form's header. Its ledgers are every ledger the
// company keeps, one of them or a ledger group (ADR-0045), offered when
// the company keeps more than one.

// limitSpace names the idempotency key of the park that follows a post
// refused for the company's limit: derived from the form's key, so the
// same form sent again parks the entry once.
var limitSpace = uuid.MustParse("0193f2a8-6c1d-7b2e-9f4a-3c5d6e7f8a9b")

// sendNew parks or posts the new entry, as the form's button asks:
// parked, the person goes to the parked entry's page; posted, to the
// journal entry's; above the limit, parked and so said; refused, the
// form says why and keeps what was typed.
func (s *Server) sendNew(w http.ResponseWriter, r *http.Request, p *person, m me, key string, t entryTyped, body gen.JournalEntryCreate, ch chart) {
	ctx := r.Context()
	done := "parked"
	if t.op == ui.OpPost {
		res, err := s.API.PostJournalEntry(ctx, &body, gen.PostJournalEntryParams{IdempotencyKey: key})
		if je, ok := res.(*gen.JournalEntryHeaders); ok {
			http.Redirect(w, r, entryPath(je.Response.ID)+"?done=posted", http.StatusSeeOther)
			return
		}
		if prob, ok := problemOf(res); !ok || prob.Code != "ledger.approval_required" {
			s.newRefused(w, r, p, m, "postJournalEntry", res, err, t, ch)
			return
		}
		key, done = uuid.NewSHA1(limitSpace, []byte(key)).String(), "limit"
	}
	res, err := s.API.ParkJournalEntry(ctx, &body, gen.ParkJournalEntryParams{IdempotencyKey: key})
	if pe, ok := res.(*gen.ParkedJournalEntryHeaders); ok {
		http.Redirect(w, r, parkedPath(pe.Response.ID)+"?done="+done, http.StatusSeeOther)
		return
	}
	s.newRefused(w, r, p, m, "parkJournalEntry", res, err, t, ch)
}

// newRefused renders the new entry's form with the API's refusal, the
// fields it names on their lines.
func (s *Server) newRefused(w http.ResponseWriter, r *http.Request, p *person, m me, operation string, res any, err error, t entryTyped, ch chart) {
	if prob, show := s.refusal(w, r, p, operation, res, err); show {
		t.refusal = explainIn(i18n.EntryProblems, i18n.EntryRefusedTitle, prob, m)
		markRefused(prob, t.lines)
		s.showNewEntry(w, r, p, m, int(prob.Status), t, fillNone, &ch)
	}
}

// parkFile parks a new entry with a CSV file's lines under the form's
// header (parkJournalEntry): parked, the person goes to its page; the
// header, the file or its rows wrong, or the API's refusal, the form says
// why, each wrong row by its line in the file, and keeps the lines typed.
func (s *Server) parkFile(w http.ResponseWriter, r *http.Request, p *person) {
	key, ok := s.formKey(w, r, ui.NewEntryPath, i18n.ParkedBack)
	if !ok {
		return
	}
	m, ok := s.whoami(w, r, p)
	if !ok {
		return
	}
	t, _ := readEntryForm(r)
	t.file = &ui.LinesUpload{Again: true}
	t.pad()
	if t.switched() {
		t.changed = true
		s.showNewEntry(w, r, p, m, http.StatusUnprocessableEntity, t, fillChosen, nil)
		return
	}
	ctx := r.Context()
	company, ch, known := s.companyOf(ctx, m, &t)
	sent := t.checkHeader()
	lines, fileErr := uploadedLines(r, t.currency)
	t.file.FileError = fileErr
	// The codes are looked up once the header and every row are right.
	if !known || len(t.errs) > 0 || fileErr != "" || !shaped(lines) || !s.resolve(ctx, company, ch, lines) {
		t.file.Rows, t.file.More = rowErrors(lines)
		s.showNewEntry(w, r, p, m, http.StatusUnprocessableEntity, t, fillNone, &ch)
		return
	}
	sent.lines = linesSent(lines)
	body := createOf(company, t, sent)
	res, err := s.API.ParkJournalEntry(ctx, &body, gen.ParkJournalEntryParams{IdempotencyKey: key})
	if pe, ok := res.(*gen.ParkedJournalEntryHeaders); ok {
		http.Redirect(w, r, parkedPath(pe.Response.ID)+"?done=parked", http.StatusSeeOther)
		return
	}
	if prob, show := s.refusal(w, r, p, "parkJournalEntry", res, err); show {
		t.refusal = explainIn(i18n.EntryProblems, i18n.EntryRefusedTitle, prob, m)
		markRefused(prob, lines)
		t.file.Rows, t.file.More = rowErrors(lines)
		s.showNewEntry(w, r, p, m, int(prob.Status), t, fillNone, &ch)
	}
}

// postToOptions are the ledgers a new entry of the company may post to:
// every ledger it keeps (the API's default), each of them, and each
// ledger group with a ledger it keeps (listLedgers, listLedgerGroups);
// none when it keeps one ledger, the person may not read ledgers, or
// they cannot be read (logged: the entry then posts to every ledger).
func (s *Server) postToOptions(ctx context.Context, m me, company uuid.UUID, chosen string) []ui.Option {
	if !m.may(permReadAccounts) || company == uuid.Nil {
		return nil
	}
	res, err := s.API.ListLedgers(ctx, gen.ListLedgersParams{LegalEntityID: gen.NewOptUUID(company), PageSize: gen.NewOptInt32(maxLedgers)})
	page, ok := res.(*gen.LedgerPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the company's ledgers could not be read", "answer", res, "error", err)
		return nil
	}
	if len(page.Items) < 2 {
		return nil
	}
	out := []ui.Option{{Selected: chosen == ""}}
	kept := map[uuid.UUID]bool{}
	for _, l := range page.Items {
		kept[l.ID] = true
		v := ui.PostToLedger + l.ID.String()
		out = append(out, ui.Option{Value: v, Text: l.Code + " " + l.Name, Selected: v == chosen})
	}
	gres, err := s.API.ListLedgerGroups(ctx, gen.ListLedgerGroupsParams{PageSize: gen.NewOptInt32(maxLedgers)})
	groups, ok := gres.(*gen.LedgerGroupPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the ledger groups could not be read", "answer", gres, "error", err)
		return out
	}
	for _, g := range groups.Items {
		if keeps(kept, g.LedgerIds) {
			v := ui.PostToGroup + g.Code
			out = append(out, ui.Option{Value: v, Text: g.Code + " " + g.Name, Selected: v == chosen})
		}
	}
	return out
}

// keeps reports whether any of the ledgers is kept.
func keeps(kept map[uuid.UUID]bool, ledgers []uuid.UUID) bool {
	for _, id := range ledgers {
		if kept[id] {
			return true
		}
	}
	return false
}

// maxPartnerIDs is the most partners listBusinessPartners names at once.
const maxPartnerIDs = 100

// partnerNumbers are the numbers of the partners the lines name by id
// (listBusinessPartners by ids), the first hundred; none when the person
// may not read partners or they cannot be read (logged: the line then
// names its partner by id, which is sent as it is).
func (s *Server) partnerNumbers(ctx context.Context, m me, lines []gen.ParkedJournalLine) map[uuid.UUID]string {
	var ids []uuid.UUID
	seen := map[uuid.UUID]bool{}
	for _, l := range lines {
		if id, ok := l.PartnerID.Get(); ok && !seen[id] && len(ids) < maxPartnerIDs {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || !m.may(permReadSuppliers) {
		return nil
	}
	res, err := s.API.ListBusinessPartners(ctx, gen.ListBusinessPartnersParams{Ids: ids, PageSize: gen.NewOptInt32(maxPartnerIDs)})
	page, ok := res.(*gen.BusinessPartnerPage)
	if !ok {
		s.Log.WarnContext(ctx, "web: the lines' partners could not be read", "answer", res, "error", err)
		return nil
	}
	out := make(map[uuid.UUID]string, len(page.Items))
	for _, b := range page.Items {
		out[b.ID] = strings.TrimSpace(b.Number)
	}
	return out
}
