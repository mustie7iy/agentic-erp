package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Parked manual entries (T35c): people and agents park a checked entry,
// which nothing posts yet.

// asAgent is e acting as the tenant's agent, in every legal entity.
func (e env) asAgent(tb testing.TB) env {
	tb.Helper()
	e.ten.AllCompanies(tb, e.ten.Agent)
	e.As = e.ten.Agent
	return e
}

// want checks the fields of a representation.
func want(t *testing.T, what string, got map[string]any, fields map[string]any) {
	t.Helper()
	for field, w := range fields {
		if got[field] != w {
			t.Errorf("%s %s: got %v, want %v", what, field, got[field], w)
		}
	}
}

func TestParkJournalEntry(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	body := entry(e.de, "2026-03-15", "EUR", line{bank, "1200.00", ""}, line{revenue, "-1200.00", ""})
	pe, tag := e.Must(201, "POST", "/v1/parked-journal-entries", body, "")
	want(t, "parked entry", pe, map[string]any{"status": "parked", "created_by_kind": "human", "owner_id": e.ten.Human.ID.String(),
		"legal_entity_id": e.de.String(), "currency": "EUR", "document_type": "SA", "posting_date": "2026-03-15", "version": float64(1),
		"approval_request_id": nil, "journal_entry_id": nil})
	if tag != `"1"` {
		t.Errorf("parked entry ETag: got %s, want \"1\"", tag)
	}
	if got := amounts(t, pe, "amount"); got != "1200.00,-1200.00" {
		t.Errorf("parked lines: got %s, want 1200.00,-1200.00", got)
	}
	got, tag := e.Must(200, "GET", "/v1/parked-journal-entries/"+pe["id"].(string), "", "")
	if got["id"] != pe["id"] || tag != `"1"` {
		t.Errorf("read back: got %v %s, want %v \"1\"", got["id"], tag, pe["id"])
	}
	// Nothing is posted and no number is drawn.
	if page, _ := e.Must(200, "GET", "/v1/journal-entries", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("journal entries after parking: got %v, want none", page["items"])
	}
	posted, _ := e.Must(201, "POST", "/v1/journal-entries", body, "")
	if posted["document_number"] != "100000" {
		t.Errorf("first posting after parking: got number %v, want 100000", posted["document_number"])
	}
	changed := events(t, e.ten, "parked_journal_entry_changed")
	if len(changed) != 1 {
		t.Fatalf("parked events: got %d %v, want 1", len(changed), changed)
	}
	want(t, "parked event", changed[0], map[string]any{"parked_entry_id": pe["id"], "status": "parked", "previous_status": nil,
		"created_by_kind": "human", "owner_id": e.ten.Human.ID.String(), "posting_date": "2026-03-15", "approval_request_id": nil, "journal_entry_id": nil})
	if amount := changed[0]["amount"].(map[string]any); amount["amount"] != "1200.00" || amount["currency"] != "EUR" {
		t.Errorf("parked event amount: got %v, want the debit total 1200.00 EUR", amount)
	}
	var audited int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT count(*) FROM audit.audit_event
		WHERE tenant_id = $1 AND action = 'ledger.parked_entry.park' AND object_id = $2`, e.ten.ID, uuid.MustParse(pe["id"].(string))).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Errorf("audit records of parking: got %d, want 1", audited)
	}
}

// TestAgentsPropose: an agent parks an entry as its proposal, which no
// person owns yet.
func TestAgentsPropose(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	agent := e.asAgent(t)
	pe, _ := agent.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "10.00", ""}, line{revenue, "-10.00", ""}), "")
	want(t, "proposal", pe, map[string]any{"status": "parked", "created_by_kind": "agent", "owner_id": nil, "created_by": e.ten.Agent.ID.String()})
	if changed := events(t, e.ten, "parked_journal_entry_changed"); len(changed) != 1 || changed[0]["owner_id"] != nil {
		t.Errorf("proposal event: got %v, want one without an owner", changed)
	}
}

// TestParkingChecks: parking checks what posting does except the period
// and the rates, which depend on when the entry is posted.
func TestParkingChecks(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1
		WHERE tenant_id = $1 AND legal_entity_id = $2 AND fiscal_year = 2026 AND period = 5`, e.ten.ID, e.de)
	for name, c := range map[string]struct {
		body string
		code string // empty: parked
	}{
		"in a closed period":  {entry(e.de, "2026-05-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), ""},
		"without a rate":      {entry(e.de, "2026-03-15", "CHF", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), ""},
		"unbalanced":          {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-2.00", ""}), "ledger.unbalanced"},
		"an unknown currency": {entry(e.de, "2026-03-15", "XXX", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "ledger.unknown_currency"},
		"another chart":       {entry(e.de, "2026-03-15", "EUR", line{e.account(t, "1000", "100000"), "1.00", ""}, line{revenue, "-1.00", ""}), "ledger.account_not_postable"},
		"a partner off recon": {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", e.supplier.String()}, line{revenue, "-1.00", ""}), "ledger.partner_not_allowed"},
		"debits out of range": {entry(e.de, "2026-03-15", "EUR", line{bank, "90000000000000000.00", ""}, line{revenue, "-90000000000000000.00", ""},
			line{bank, "90000000000000000.00", ""}, line{revenue, "-90000000000000000.00", ""}), "ledger.amount_too_large"},
		"an unknown entity": {entry(uuid.New(), "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "request.reference_not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			if c.code == "" {
				e.Must(201, "POST", "/v1/parked-journal-entries", c.body, "")
				return
			}
			e.Problem(422, c.code, "POST", "/v1/parked-journal-entries", c.body, "")
		})
	}
}

func TestListParkedJournalEntries(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	usBank, usRevenue := e.account(t, "1000", "100000"), e.account(t, "1000", "400000")
	park := func(c apitest.Client, le uuid.UUID, a, b string) string {
		pe, _ := c.Must(201, "POST", "/v1/parked-journal-entries", entry(le, "2026-03-15", "EUR", line{a, "1.00", ""}, line{b, "-1.00", ""}), "")
		return pe["id"].(string)
	}
	first := park(e.Client, e.de, bank, revenue)
	second := park(e.Client, e.us, usBank, usRevenue)
	proposal := park(e.asAgent(t).Client, e.de, bank, revenue)
	list := func(query string) []string {
		page, _ := e.Must(200, "GET", "/v1/parked-journal-entries"+query, "", "")
		var ids []string
		for _, it := range page["items"].([]any) {
			ids = append(ids, it.(map[string]any)["id"].(string))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			ids = append(ids, "next:"+tok)
		}
		return ids
	}
	for query, w := range map[string][]string{
		"":                                     {proposal, second, first},
		"?legal_entity_id=" + e.de.String():    {proposal, first},
		"?created_by_kind=agent":               {proposal},
		"?status=parked&created_by_kind=human": {second, first},
		"?status=posted":                       nil,
	} {
		t.Run(query, func(t *testing.T) {
			if got := list(query); fmt.Sprint(got) != fmt.Sprint(w) {
				t.Errorf("list %s: got %v, want %v", query, got, w)
			}
		})
	}
	page := list("?page_size=2")
	if len(page) != 3 || page[0] != proposal || page[1] != second {
		t.Fatalf("page 1: got %v, want %s, %s and a next page token", page, proposal, second)
	}
	if rest := list("?page_size=2&page_token=" + page[2][len("next:"):]); fmt.Sprint(rest) != fmt.Sprint([]string{first}) {
		t.Errorf("page 2: got %v, want [%s]", rest, first)
	}
	// A principal without company access sees none and finds none.
	stranger := e.Client
	stranger.As = e.ten.Service
	if got, _ := stranger.Must(200, "GET", "/v1/parked-journal-entries", "", ""); len(got["items"].([]any)) != 0 {
		t.Errorf("without company access: got %v, want none", got["items"])
	}
	stranger.Problem(404, "request.not_found", "GET", "/v1/parked-journal-entries/"+first, "", "")
	e.Problem(404, "request.not_found", "GET", "/v1/parked-journal-entries/"+uuid.NewString(), "", "")
}

// TestPostedParkedEntry: a parked entry posted as a journal entry (T35e)
// names it and its fiscal year.
func TestPostedParkedEntry(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	pe, _ := e.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	id := uuid.MustParse(pe["id"].(string))
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		je, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"),
			Currency: "EUR", SourceContext: "ledger", SourceDocumentID: id, Lines: []ledger.PostingLine{
				{AccountID: uuid.MustParse(bank), Amount: 100}, {AccountID: uuid.MustParse(revenue), Amount: -100}}})
		if err != nil {
			return err
		}
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE ledger.parked_entry SET status = 'posted', journal_fiscal_year = $2, journal_entry_id = $3, version = version + 1
			WHERE id = $1`, id, je.FiscalYear, je.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.Must(200, "GET", "/v1/parked-journal-entries/"+id.String(), "", "")
	want(t, "posted entry", got, map[string]any{"status": "posted", "journal_fiscal_year": float64(2026)})
	if got["journal_entry_id"] == nil {
		t.Errorf("posted entry journal_entry_id: got none, want the journal entry")
	}
}
