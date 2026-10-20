package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// parallelEnv is setup with the parallel ledgers activated: company 2000
// keeps 0L and 2L, both open for 2026; 1000 keeps 0L alone.
type parallelEnv struct {
	env
	leading, local string // ledger ids
}

func setupParallel(tb testing.TB) parallelEnv {
	tb.Helper()
	e := setup(tb)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	if err := apitest.Committed(tb, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, parallel(tb)) }); err != nil {
		tb.Fatal(err)
	}
	pgtest.Exec(tb, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND fiscal_year = 2026 AND period <= 12
		AND status = 'closed'`, e.ten.ID)
	p := parallelEnv{env: e}
	p.leading, p.local = p.ledger(tb, "0L"), p.ledger(tb, "2L")
	return p
}

func (p parallelEnv) ledger(tb testing.TB, code string) string {
	tb.Helper()
	var id string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT id::text FROM ledger.ledger WHERE tenant_id = $1 AND code = $2`, p.ten.ID, code).Scan(&id); err != nil {
		tb.Fatalf("ledger %s: %v", code, err)
	}
	return id
}

// group is the ledgers, in ledger code order, and the document numbers of
// the entries of the posting group whose first entry is first.
func (p parallelEnv) group(tb testing.TB, first string) (ledgers, numbers string) {
	tb.Helper()
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT string_agg(l.code, ',' ORDER BY l.code), string_agg(DISTINCT e.document_number, ',')
		FROM ledger.journal_entry e JOIN ledger.ledger l ON l.id = e.ledger_id WHERE e.posting_group_id = $1`, first).Scan(&ledgers, &numbers); err != nil {
		tb.Fatalf("posting group %s: %v", first, err)
	}
	return ledgers, numbers
}

// with puts fields into a journal entry body.
func with(body, fields string) string {
	return strings.Replace(body, `"currency"`, fields+`, "currency"`, 1)
}

// TestParallelPostings: a posting goes to every ledger of its company as
// one posting group under one document number, or to the ledger or group
// it names, each ledger's period checked; a reversal reverses every copy
// (T52b, ADR-0045).
func TestParallelPostings(t *testing.T) {
	p := setupParallel(t)
	p.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	bank, revenue := p.account(t, "2000", "100000"), p.account(t, "2000", "400000")
	usBank, usRevenue := p.account(t, "1000", "100000"), p.account(t, "1000", "400000")
	body := entry(p.de, "2026-03-15", "EUR", line{bank, "10.00", ""}, line{revenue, "-10.00", ""})
	us := entry(p.us, "2026-03-15", "USD", line{usBank, "10.00", ""}, line{usRevenue, "-10.00", ""})

	for _, c := range []struct {
		name, body, ledger, group string // ledger returned and the group's ledgers
	}{
		{"every ledger of the company", body, "0L", "0L,2L"},
		{"a company with one ledger", us, "0L", "0L"},
		{"a ledger group", with(body, `"ledger_group": "ALL"`), "0L", "0L,2L"},
		{"a group of the parallel ledger", with(body, `"ledger_group": "LOC"`), "2L", "2L"},
		{"one ledger", with(body, `"ledger_id": "`+p.local+`"`), "2L", "2L"},
	} {
		t.Run(c.name, func(t *testing.T) {
			je, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", c.body, "")
			if want := map[string]string{"0L": p.leading, "2L": p.local}[c.ledger]; je["ledger_id"] != want {
				t.Errorf("ledger: got %v, want %s (%s)", je["ledger_id"], want, c.ledger)
			}
			if je["posting_group_id"] != je["id"] {
				t.Errorf("posting group: got %v, want the entry %v", je["posting_group_id"], je["id"])
			}
			ledgers, numbers := p.group(t, je["id"].(string))
			if ledgers != c.group {
				t.Errorf("posted to: got %s, want %s", ledgers, c.group)
			}
			if numbers != je["document_number"] {
				t.Errorf("document numbers: got %s, want %v alone", numbers, je["document_number"])
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		for _, c := range []struct{ name, body, code string }{
			{"a ledger the company does not keep", with(us, `"ledger_id": "`+p.local+`"`), "ledger.ledger_not_kept"},
			{"a group with a ledger the company does not keep", with(us, `"ledger_group": "LOC"`), "ledger.ledger_not_kept"},
			{"an unknown group", with(body, `"ledger_group": "ZZ"`), "ledger.unknown_ledger_group"},
			{"a ledger and a group", with(body, `"ledger_id": "`+p.local+`", "ledger_group": "LOC"`), "ledger.ledger_or_group"},
		} {
			t.Run(c.name, func(t *testing.T) {
				p.in(t).Problem(422, c.code, "POST", "/v1/journal-entries", c.body, "")
				p.in(t).Problem(422, c.code, "POST", "/v1/parked-journal-entries", c.body, "")
			})
		}
	})
	t.Run("periods per ledger", func(t *testing.T) { periodsPerLedger(t, p, bank, revenue) })
	t.Run("events, projection and audit", func(t *testing.T) { postedOnce(t, p, body) })
	t.Run("reversal", func(t *testing.T) { reverseGroup(t, p, body) })
	t.Run("parked", func(t *testing.T) { parkedLedgers(t, p, body) })
	t.Run("open items", func(t *testing.T) { leadingOpenItems(t, p) })
}

func periodsPerLedger(t *testing.T, p parallelEnv, bank, revenue string) {
	t.Helper()
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND ledger_id = $2
		AND fiscal_year = 2026 AND period = 4`, p.de, p.local)
	april := entry(p.de, "2026-04-15", "EUR", line{bank, "10.00", ""}, line{revenue, "-10.00", ""})
	if got := p.in(t).Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", april, ""); !strings.Contains(got["detail"].(string), "in ledger 2L") {
		t.Errorf("detail: got %v, want it to name ledger 2L", got["detail"])
	}
	p.in(t).Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", with(april, `"ledger_group": "LOC"`), "")
	je, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", with(april, `"ledger_id": "`+p.leading+`"`), "")
	if ledgers, _ := p.group(t, je["id"].(string)); ledgers != "0L" {
		t.Errorf("0L alone in April: got %s", ledgers)
	}
	pgtest.Exec(t, `DELETE FROM ledger.period_status WHERE legal_entity_id = $1 AND ledger_id = $2 AND fiscal_year = 2026 AND period = 5`, p.de, p.local)
	p.in(t).Problem(422, "ledger.no_fiscal_period", "POST", "/v1/journal-entries", entry(p.de, "2026-05-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
}

// postedOnce: each entry of a group publishes its JournalEntryPosted and
// is projected; the posting is audited once.
func postedOnce(t *testing.T, p parallelEnv, body string) {
	t.Helper()
	je, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", with(body, `"reference": "once"`), "")
	group := je["id"].(string)
	var posted int
	for _, ev := range events(t, p.ten, "journal_entry_posted") {
		if ev["posting_group_id"] == group {
			posted++
		}
	}
	if posted != 2 {
		t.Errorf("JournalEntryPosted of the group: got %d, want 2", posted)
	}
	count := func(name, sql string, want int) {
		var n int
		if err := pgtest.Admin(t).QueryRow(context.Background(), sql, group).Scan(&n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if n != want {
			t.Errorf("%s: got %d, want %d", name, n, want)
		}
	}
	count("queued for the projector", `SELECT count(*) FROM ledger.balance_projection q JOIN ledger.journal_entry e ON e.id = q.journal_entry_id
		WHERE e.posting_group_id = $1`, 2)
	count("audited", `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND action = 'ledger.journal_entry.post'`, 1)
}

func reverseGroup(t *testing.T, p parallelEnv, body string) {
	t.Helper()
	je, tag := p.in(t).Must(201, "POST", "/v1/journal-entries", body, "")
	var copyID string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id::text FROM ledger.journal_entry WHERE posting_group_id = $1 AND id <> $1`, je["id"]).Scan(&copyID); err != nil {
		t.Fatalf("copy: %v", err)
	}
	cp, cpTag := p.in(t).Must(200, "GET", "/v1/journal-entries/"+copyID, "", "")
	if cp["posting_group_id"] != je["id"] || cp["document_number"] != je["document_number"] {
		t.Errorf("copy: got group %v number %v, want %v and %v", cp["posting_group_id"], cp["document_number"], je["id"], je["document_number"])
	}
	p.in(t).Problem(409, "ledger.entry_is_copy", "POST", "/v1/journal-entries/"+copyID+":reverse", "", cpTag)
	rev, _ := p.in(t).Must(200, "POST", "/v1/journal-entries/"+je["id"].(string)+":reverse", "", tag)
	var reversed string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT string_agg(reversal_of_id::text, ',' ORDER BY reversal_of_id = $2 DESC) FROM ledger.journal_entry
		WHERE posting_group_id = $1`, rev["id"], je["id"]).Scan(&reversed); err != nil {
		t.Fatalf("reversal copies: %v", err)
	}
	if want := je["id"].(string) + "," + copyID; reversed != want {
		t.Errorf("reversal copies reverse: got %s, want %s", reversed, want)
	}
}

func parkedLedgers(t *testing.T, p parallelEnv, body string) {
	t.Helper()
	for _, c := range []struct {
		name, fields, ledger string
		specific             bool
		group                any
	}{
		{"every ledger", `"reference": "parked"`, "0L,2L", false, nil},
		{"a group", `"ledger_group": "LOC"`, "2L", false, "LOC"},
		{"a ledger", `"ledger_id": "` + p.local + `"`, "2L", true, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			pe, tag := p.in(t).Must(201, "POST", "/v1/parked-journal-entries", with(body, c.fields), "")
			if pe["ledger_specific"] != c.specific {
				t.Errorf("ledger_specific: got %v, want %v", pe["ledger_specific"], c.specific)
			}
			if pe["ledger_group"] != c.group {
				t.Errorf("ledger_group: got %v, want %v", pe["ledger_group"], c.group)
			}
			posted, _ := p.in(t).Must(200, "POST", "/v1/parked-journal-entries/"+pe["id"].(string)+":post", "", tag)
			if ledgers, _ := p.group(t, posted["journal_entry_id"].(string)); ledgers != c.ledger {
				t.Errorf("posted to: got %s, want %s", ledgers, c.ledger)
			}
		})
	}
}

// leadingOpenItems: open items are the leading ledger's; a copy's line is
// no open item to clear.
func leadingOpenItems(t *testing.T, p parallelEnv) {
	t.Helper()
	expense, ap := p.account(t, "2000", "520000"), p.account(t, "2000", "211000")
	sup := p.newSupplier(t, "BP00000077").String()
	invoice, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-01", "EUR", line{expense, "100.00", ""}, line{ap, "-100.00", sup}), "")
	page, _ := p.in(t).Must(200, "GET", "/v1/open-items?partner_id="+sup, "", "")
	if got := len(page["items"].([]any)); got != 1 {
		t.Fatalf("open items: got %d, want the leading ledger's alone", got)
	}
	var copyID string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT id::text FROM ledger.journal_entry WHERE posting_group_id = $1 AND id <> $1`, invoice["id"]).Scan(&copyID); err != nil {
		t.Fatalf("copy: %v", err)
	}
	copied := map[string]any{"id": copyID, "fiscal_year": invoice["fiscal_year"]}
	payment, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-20", "EUR", line{ap, "100.00", sup}, line{p.account(t, "2000", "100000"), "-100.00", ""}), "")
	p.in(t).Problem(422, "ledger.not_an_open_item", "POST", "/v1/clearings", clearing(p.de, "2026-03-31", ref{copied, 2}, ref{payment, 1}), "")
	c, _ := p.in(t).Must(201, "POST", "/v1/clearings", clearing(p.de, "2026-03-31", ref{invoice, 2}, ref{payment, 1}), "")
	if ledgers, _ := p.group(t, c["clearing_entry_id"].(string)); ledgers != "0L,2L" {
		t.Errorf("clearing entry posted to: got %s, want 0L,2L", ledgers)
	}
}

// TestParallelDatabaseFailures fails each database statement of postings
// to several ledgers in turn: none is hidden (T52b).
func TestParallelDatabaseFailures(t *testing.T) {
	p := setupParallel(t)
	h := endpoints()
	bank, revenue := uuid.MustParse(p.account(t, "2000", "100000")), uuid.MustParse(p.account(t, "2000", "400000"))
	create := func(ledger *uuid.UUID, group string) *gen.JournalEntryCreate {
		req := &gen.JournalEntryCreate{LegalEntityID: p.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"), Currency: "EUR",
			Lines: []gen.JournalLineInput{{AccountID: bank, Amount: gen.Money{Amount: "1.00", Currency: "EUR"}},
				{AccountID: revenue, Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}}}}
		if ledger != nil {
			req.LedgerID.SetTo(*ledger)
		}
		if group != "" {
			req.LedgerGroup.SetTo(group)
		}
		return req
	}
	local := uuid.MustParse(p.local)
	je, _ := p.Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-15", "EUR", line{bank.String(), "1.00", ""}, line{revenue.String(), "-1.00", ""}), "")
	for name, call := range map[string]func(ctx context.Context) error{
		"post to every ledger": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, create(nil, ""), gen.PostJournalEntryParams{})
			return err
		},
		"post to a group": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, create(nil, "ALL"), gen.PostJournalEntryParams{})
			return err
		},
		"post to a ledger": func(ctx context.Context) error {
			_, err := h.PostJournalEntry(ctx, create(&local, ""), gen.PostJournalEntryParams{})
			return err
		},
		"park for a group": func(ctx context.Context) error {
			_, err := h.ParkJournalEntry(ctx, create(nil, "LOC"), gen.ParkJournalEntryParams{})
			return err
		},
		"reverse a posting group": func(ctx context.Context) error {
			_, err := h.ReverseJournalEntry(ctx, gen.OptJournalEntryReverse{}, gen.ReverseJournalEntryParams{ID: uuid.MustParse(je["id"].(string)), IfMatch: "*"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, p.ten, p.ten.Human, name, call) })
	}
}
