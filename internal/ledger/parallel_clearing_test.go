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
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// copyOf is the copy in 2L of the posting group whose first entry is first.
func (p parallelEnv) copyOf(tb testing.TB, first any) string {
	tb.Helper()
	var id string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT id::text FROM ledger.journal_entry WHERE posting_group_id = $1 AND ledger_id = $2`,
		first, p.local).Scan(&id); err != nil {
		tb.Fatalf("copy of %v in 2L: %v", first, err)
	}
	return id
}

// clearedLines are the entry's cleared line numbers, comma-separated.
func clearedLines(tb testing.TB, entry string) string {
	tb.Helper()
	var lines string
	if err := pgtest.Admin(tb).QueryRow(context.Background(), `SELECT coalesce(string_agg(line_no::text, ',' ORDER BY line_no), '')
		FROM ledger.clearing_item WHERE journal_entry_id = $1`, entry).Scan(&lines); err != nil {
		tb.Fatalf("cleared lines of %s: %v", entry, err)
	}
	return lines
}

// wantCleared checks the cleared lines of each entry.
func wantCleared(t *testing.T, want map[string]string, names map[string]string) {
	t.Helper()
	for entry, lines := range want {
		if got := clearedLines(t, entry); got != lines {
			t.Errorf("cleared lines of %s: got %q, want %q", names[entry], got, lines)
		}
	}
}

// parallelDocs posts, in company 2000, a supplier invoice to every ledger
// and to the leading ledger alone, and a payment of the first to every
// ledger.
type parallelDocs struct {
	invoice, leadingInvoice, payment map[string]any
	bank, expense, ap, sup           string
}

func (p parallelEnv) docs(t *testing.T, number string) parallelDocs {
	t.Helper()
	d := parallelDocs{bank: p.account(t, "2000", "100000"), expense: p.account(t, "2000", "520000"), ap: p.account(t, "2000", "211000")}
	d.sup = p.newSupplier(t, number).String()
	d.invoice, _ = p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-01", "EUR", line{d.expense, "100.00", ""}, line{d.ap, "-100.00", d.sup}), "")
	d.leadingInvoice, _ = p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-02", "EUR", line{d.expense, "30.00", ""},
		line{d.ap, "-30.00", d.sup}), "")
	// A supplier line is never posted to one ledger alone (ADR-0045); the
	// copy is taken away, as an item posted to the leading ledger alone
	// before that rule has none.
	pgtest.Unguarded(t, `WITH c AS (SELECT id FROM ledger.journal_entry WHERE posting_group_id = $1 AND id <> $1),
		l AS (DELETE FROM ledger.journal_line WHERE journal_entry_id IN (SELECT id FROM c)),
		b AS (DELETE FROM ledger.balance_projection WHERE journal_entry_id IN (SELECT id FROM c))
		DELETE FROM ledger.journal_entry WHERE id IN (SELECT id FROM c)`, uuid.MustParse(d.leadingInvoice["id"].(string)))
	d.payment, _ = p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-20", "EUR", line{d.ap, "100.00", d.sup}, line{d.bank, "-100.00", ""}), "")
	return d
}

// TestParallelClearing: a clearing is applied to the copies of its lines
// in every ledger, its own entry narrowed to the ledgers its items were
// posted in; the API shows the leading ledger's clearing and journal
// (T52c, ADR-0045).
func TestParallelClearing(t *testing.T) {
	p := setupParallel(t)
	t.Run("clearing in every ledger", func(t *testing.T) { clearEveryLedger(t, p) })
	t.Run("narrowed to the items' ledgers", func(t *testing.T) { clearNarrowed(t, p) })
	t.Run("settlement", func(t *testing.T) { settleEveryLedger(t, p) })
	t.Run("reversal", func(t *testing.T) { reverseClearsCopies(t, p) })
	t.Run("migrated clearing", func(t *testing.T) { migratedEveryLedger(t, p) })
	t.Run("journal list by ledger", func(t *testing.T) { listByLedger(t, p) })
}

func clearEveryLedger(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000081")
	body := clearing(p.de, "2026-03-31", ref{d.invoice, 2}, ref{d.payment, 1})
	c, _ := p.in(t).Must(201, "POST", "/v1/clearings", body, "")
	entry := c["clearing_entry_id"].(string)
	if ledgers, _ := p.group(t, entry); ledgers != "0L,2L" {
		t.Errorf("clearing entry posted to: got %s, want 0L,2L", ledgers)
	}
	invoice, payment, own := p.copyOf(t, d.invoice["id"]), p.copyOf(t, d.payment["id"]), p.copyOf(t, entry)
	want := map[string]string{invoice: "2", payment: "1", own: "1,2"}
	wantCleared(t, want, map[string]string{invoice: "the invoice's copy", payment: "the payment's copy", own: "the clearing entry's copy"})
	// Run twice: a retried clearing is refused and clears no copy again.
	p.in(t).Problem(409, "ledger.item_already_cleared", "POST", "/v1/clearings", body, "")
	wantCleared(t, want, map[string]string{invoice: "the invoice's copy", payment: "the payment's copy", own: "the clearing entry's copy"})

	page, _ := p.in(t).Must(200, "GET", "/v1/clearings?legal_entity_id="+p.de.String()+"&page_size=100", "", "")
	shown := 0
	for _, it := range apitest.List(t, page, "items") {
		switch it.(map[string]any)["clearing_entry_id"] {
		case entry:
			shown++
		case own:
			t.Error("clearings: the copy's clearing is listed")
		}
	}
	if shown != 1 {
		t.Errorf("clearings by the entry: got %d, want 1", shown)
	}
	var announced int
	for _, ev := range events(t, p.ten, "items_cleared") {
		if ev["clearing_entry_id"] == entry || ev["clearing_entry_id"] == own {
			announced++
		}
	}
	if announced != 1 {
		t.Errorf("items_cleared of the clearing: got %d, want 1", announced)
	}
}

func clearNarrowed(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000082")
	pay, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", entry(p.de, "2026-03-21", "EUR", line{d.ap, "30.00", d.sup}, line{d.bank, "-30.00", ""}), "")
	c, _ := p.in(t).Must(201, "POST", "/v1/clearings", clearing(p.de, "2026-03-31", ref{d.leadingInvoice, 2}, ref{pay, 1}), "")
	if ledgers, _ := p.group(t, c["clearing_entry_id"].(string)); ledgers != "0L" {
		t.Errorf("clearing entry of a 0L-only item posted to: got %s, want 0L", ledgers)
	}
	if got := clearedLines(t, p.copyOf(t, pay["id"])); got != "" {
		t.Errorf("cleared lines of the payment's copy: got %q, want none", got)
	}
}

// settleEveryLedger: a settlement posts to every ledger; in 2L it clears
// the copy of an item posted there with its offset, and leaves the offset
// of an item 2L never had.
func settleEveryLedger(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000083")
	in := settlementOf(t, p.env, p.de, "2000", "EUR", "2026-03-25",
		ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.MustParse(d.invoice["id"].(string)), LineNo: 2},
		ledger.ItemRef{FiscalYear: 2026, EntryID: uuid.MustParse(d.leadingInvoice["id"].(string)), LineNo: 2})
	var settled ledger.Entry
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) (err error) {
		settled, err = postings().Settle(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ledgers, _ := p.group(t, settled.ID.String()); ledgers != "0L,2L" {
		t.Errorf("settlement posted to: got %s, want 0L,2L", ledgers)
	}
	invoice, own := p.copyOf(t, d.invoice["id"]), p.copyOf(t, settled.ID)
	wantCleared(t, map[string]string{invoice: "2", own: "1"}, map[string]string{invoice: "the invoice's copy", own: "the settlement's copy"})
	// A settlement of items 2L never had clears nothing there.
	alone := settlementOf(t, p.env, p.de, "2000", "EUR", "2026-03-26", ledger.ItemRef{FiscalYear: 2026,
		EntryID: uuid.MustParse(p.docs(t, "BP00000088").leadingInvoice["id"].(string)), LineNo: 2})
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) (err error) {
		settled, err = postings().Settle(ctx, alone)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := clearedLines(t, p.copyOf(t, settled.ID)); got != "" {
		t.Errorf("cleared lines of the copy of a settlement of 0L items: got %q, want none", got)
	}
}

func reverseClearsCopies(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000084")
	_, tag := p.in(t).Must(200, "GET", "/v1/journal-entries/"+d.invoice["id"].(string), "", "")
	rev, _ := p.in(t).Must(200, "POST", "/v1/journal-entries/"+d.invoice["id"].(string)+":reverse", "", tag)
	invoice, reversal := p.copyOf(t, d.invoice["id"]), p.copyOf(t, rev["id"])
	wantCleared(t, map[string]string{invoice: "2", reversal: "2"}, map[string]string{invoice: "the invoice's copy", reversal: "the reversal's copy"})
}

func migratedEveryLedger(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000085")
	pair := migratedPair{uuid.MustParse(d.invoice["id"].(string)), uuid.MustParse(d.payment["id"].(string))}
	payCopy := uuid.MustParse(p.copyOf(t, d.payment["id"]))
	clear := func(by uuid.UUID, day string) error {
		return apitest.InRequest(t, p.ten, p.ten.Human, func(ctx context.Context) error {
			_, err := postings().Clear(ctx, p.de, date(day), ledger.ItemRef{FiscalYear: 2026, EntryID: by, LineNo: 1}, pair.items())
			return err
		})
	}
	if got := problem.From(clear(payCopy, "2026-03-31")); got.Code != "ledger.invalid_clearing_entry" || !strings.Contains(got.Detail, "is a copy") {
		t.Errorf("by a copy: got %s %q, want ledger.invalid_clearing_entry naming the copy", got.Code, got.Detail)
	}
	// An entry of the parallel ledger alone holds no open item to clear by.
	alone, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", with(entry(p.de, "2026-03-03", "EUR", line{d.bank, "5.00", ""},
		line{d.expense, "-5.00", ""}), `"ledger_id": "`+p.local+`"`), "")
	if got := problem.From(clear(uuid.MustParse(alone["id"].(string)), "2026-03-31")); got.Code != "ledger.invalid_clearing_entry" ||
		!strings.Contains(got.Detail, "not in the leading ledger") {
		t.Errorf("by an entry of 2L alone: got %s %q, want ledger.invalid_clearing_entry naming the leading ledger", got.Code, got.Detail)
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND ledger_id = $2
		AND fiscal_year = 2026 AND period = 6`, p.de, p.local)
	if got := problem.From(clear(pair.payment, "2026-06-30")); got.Code != "ledger.period_not_open" || !strings.Contains(got.Detail, "in ledger 2L") {
		t.Errorf("2L closed: got %s %q, want ledger.period_not_open in ledger 2L", got.Code, got.Detail)
	}
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) error {
		_, err := postings().Clear(ctx, p.de, date("2026-03-31"), ledger.ItemRef{FiscalYear: 2026, EntryID: pair.payment, LineNo: 1}, pair.items())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	invoice := p.copyOf(t, d.invoice["id"])
	wantCleared(t, map[string]string{invoice: "2", payCopy.String(): "1"}, map[string]string{invoice: "the invoice's copy", payCopy.String(): "the payment's copy"})
}

// listByLedger: the journal list shows the leading ledger's entries unless
// a ledger is asked for.
func listByLedger(t *testing.T, p parallelEnv) {
	t.Helper()
	d := p.docs(t, "BP00000086")
	number := d.invoice["document_number"].(string)
	for name, c := range map[string]struct{ query, want string }{
		"the leading ledger by default": {"", d.invoice["id"].(string)},
		"the parallel ledger asked for": {"&ledger_id=" + p.local, p.copyOf(t, d.invoice["id"])},
		"an unknown ledger":             {"&ledger_id=" + uuid.NewString(), ""},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := p.in(t).Must(200, "GET", "/v1/journal-entries?legal_entity_id="+p.de.String()+"&document_number="+number+c.query, "", "")
			var ids []string
			for _, it := range apitest.List(t, page, "items") {
				ids = append(ids, it.(map[string]any)["id"].(string))
			}
			if got := strings.Join(ids, ","); got != c.want {
				t.Errorf("entries: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestParallelClearingDatabaseFailures fails each database statement of
// clearings applied to several ledgers in turn: none is hidden (T52c).
func TestParallelClearingDatabaseFailures(t *testing.T) {
	p := setupParallel(t)
	h := endpoints()
	d := p.docs(t, "BP00000087")
	inv, pay := uuid.MustParse(d.invoice["id"].(string)), uuid.MustParse(d.payment["id"].(string))
	pair := migratedPair{inv, pay}
	for name, call := range map[string]func(ctx context.Context) error{
		"clear in every ledger": func(ctx context.Context) error {
			_, err := h.CreateClearing(ctx, &gen.ClearingCreate{LegalEntityID: p.de, ClearingDate: date("2026-03-31"), Items: []gen.ClearingItemRef{
				{FiscalYear: 2026, JournalEntryID: inv, LineNo: 2}, {FiscalYear: 2026, JournalEntryID: pay, LineNo: 1}}}, gen.CreateClearingParams{})
			return err
		},
		"migrated clearing in every ledger": func(ctx context.Context) error {
			_, err := postings().Clear(ctx, p.de, date("2026-03-31"), ledger.ItemRef{FiscalYear: 2026, EntryID: pay, LineNo: 1}, pair.items())
			return err
		},
		"list a ledger's journal": func(ctx context.Context) error {
			_, err := h.ListJournalEntries(ctx, gen.ListJournalEntriesParams{LedgerID: gen.NewOptUUID(uuid.MustParse(p.local))})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, p.ten, p.ten.Human, name, call) })
	}
}
