package cash_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// docsOf reads the documents behind Maple's variance with the query.
func (e varianceEnv) docsOf(t *testing.T, query string) map[string]any {
	t.Helper()
	d, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/documents?legal_entity_id="+e.le.String()+query, "", "")
	return d
}

// names are the scenario's documents by id: its documents, the runs'
// payments and the statements.
func (e varianceEnv) names(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, id := range e.docs {
		out[id.String()] = name
	}
	out[e.paymentOf(t, e.docs["run"]).String()] = "run's payment"
	out[e.paymentOf(t, e.docs["run2"]).String()] = "run2's payment"
	out[e.transit.String()] = "transit"
	for _, cur := range []string{"CAD", "USD"} {
		var s uuid.UUID
		pgtest.Scan(t, `SELECT id FROM payments.bank_statement WHERE legal_entity_id = $1 AND currency = $2 AND opening_date = '2026-04-17'`,
			[]any{e.le, cur}, &s)
		out[s.String()] = cur + " statement"
	}
	return out
}

// entryLines renders the documents' entries as "side attribution
// currency item value-date (bucket) amount: kind name, ... of count
// [#line]", a document of no known name by its kind alone.
func entryLines(d map[string]any, names map[string]string) []string {
	var out []string
	for _, it := range d["items"].([]any) {
		en := it.(map[string]any)
		var refs []string
		for _, r := range en["documents"].([]any) {
			ref := r.(map[string]any)
			name, ok := names[fmt.Sprint(ref["id"])]
			if !ok {
				name = "?"
			}
			if _, ok := ref["number"]; ok {
				name += "#"
			}
			if on, ok := ref["occurrence_date"]; ok {
				name += "@" + fmt.Sprint(on)
			}
			refs = append(refs, fmt.Sprint(ref["kind"], " ", name))
		}
		line := fmt.Sprintf("%v %v %v %v %v (%v) %s: %s of %v", en["side"], en["attribution"], en["currency"], en["liquidity_item"], en["value_date"],
			en["bucket_start"], strings.Fields(amount(en["amount"]))[0], strings.Join(refs, ", "), en["document_count"])
		if no, ok := en["line_no"]; ok {
			line += fmt.Sprint(" #", no, " ", en["bank_reference"])
		}
		if s, ok := en["supplier"].(map[string]any); ok {
			line += fmt.Sprint(" to ", s["name"])
		}
		out = append(out, line)
	}
	return out
}

// TestVarianceDocuments: the documents behind TestForecastVariance's
// attribution by week, each contribution with the documents it is known
// by: the second run's payment forecast on the 22nd and paid on the 29th
// (timing both sides), the receipt paid short (amount), the series'
// occurrence (amount, by its occurrence), the credit memo and the planned
// payment never paid (missing), the invoice posted after the snapshot
// (new), the receipt not matched (unclassified); of one row, of the
// unclassified row (none), and of a row with its descendants.
func TestVarianceDocuments(t *testing.T) {
	e := setupVariance(t)
	e.Tenant.Grant(t, "payments.statement.read", e.Tenant.Human)
	names := e.names(t)
	d := e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week")
	want(t, "header", fmt.Sprint(d["company_code"], " ", d["from"], " ", d["to"], " ", d["lag"], " ", d["grain"], " ", d["snapshot_id"], " ",
		d["snapshot_date"], " ", d["document_level_until"], " ", d["unclassified"]),
		"3000 2026-04-17 2026-05-21 0 week "+e.snapshot+" 2026-04-17 2026-05-17 false")
	got := entryLines(d, names)
	sort.Strings(got)
	want(t, "entries", got, []string{
		"actual amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00: payment run's payment#, journal_entry ?, supplier_invoice ?, bank_statement CAD statement# of 4 #1 B to BP90000001",
		"actual amount CAD OP_SUPPLIERS 2026-04-28 (2026-04-24) -250.00: payment ?#, down_payment_request request#, bank_statement CAD statement# of 3 #10 B to BP90000001",
		"actual amount CAD OP_SUPPLIERS 2026-04-30 (2026-04-24) -1000.00: payment ?#, journal_entry item#, supplier_invoice ?, bank_statement CAD statement# of 4 #5 B to BP90000001",
		"actual amount CAD OP_SUPPLIERS 2026-05-07 (2026-05-01) -300.00: payment ?#, journal_entry ?, supplier_invoice parked#, bank_statement CAD statement# of 4 #8 B to BP90000001",
		"actual amount CAD REC_CUSTOMERS 2026-04-23 (2026-04-17) 1900.00: memo_record receipt#, bank_statement CAD statement# of 2 #2 B",
		// The series' occurrence by its override, then by the series and the day.
		"actual amount CAD REC_CUSTOMERS 2026-04-24 (2026-04-24) 100.00: memo_record ?, memo_record series@2026-04-24, bank_statement CAD statement# of 3 #3 B",
		"actual amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00: payment transit#, bank_statement USD statement# of 2 #1 B to BP90000001",
		"actual new CAD OP_SUPPLIERS 2026-05-05 (2026-05-01) -400.00: payment ?#, journal_entry ?, supplier_invoice ?, bank_statement CAD statement# of 4 #6 B to BP90000001",
		// The second run's payment a week late, one part per item it paid.
		"actual timing CAD OP_SUPPLIERS 2026-04-29 (2026-04-24) -100.00: payment run2's payment#, journal_entry ?, supplier_invoice ?, bank_statement CAD statement# of 4 #4 B to BP90000001",
		"actual timing CAD OP_SUPPLIERS 2026-04-29 (2026-04-24) -200.00: payment run2's payment#, journal_entry ?, supplier_invoice ?, bank_statement CAD statement# of 4 #4 B to BP90000001",
		"actual unclassified CAD REC_CUSTOMERS 2026-05-06 (2026-05-01) 500.00: bank_statement CAD statement# of 1 #7 B",
		"forecast amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00: payment run's payment# of 1 to BP90000001",
		"forecast amount CAD OP_SUPPLIERS 2026-04-28 (2026-04-24) -250.00: down_payment_request request# of 1 to BP90000001",
		"forecast amount CAD OP_SUPPLIERS 2026-04-30 (2026-04-24) -1000.00: journal_entry item# of 1 to BP90000001",
		"forecast amount CAD OP_SUPPLIERS 2026-05-07 (2026-05-01) -300.00: supplier_invoice parked# of 1 to BP90000001",
		"forecast amount CAD REC_CUSTOMERS 2026-04-23 (2026-04-17) 2000.00: memo_record receipt# of 1",
		"forecast amount CAD REC_CUSTOMERS 2026-04-24 (2026-04-24) 100.00: memo_record series@2026-04-24 of 1",
		"forecast amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00: payment transit# of 1 to BP90000001",
		"forecast missing CAD OP_SUPPLIERS 2026-04-30 (2026-04-24) 100.00: journal_entry credit# of 1 to BP90000001",
		"forecast missing CAD OP_SUPPLIERS 2026-05-12 (2026-05-08) -500.00: memo_record ? of 1",
		"forecast missing USD OP_SUPPLIERS 2026-04-22 (2026-04-17) -0.02: memo_record ? of 1",
		"forecast missing USD OP_SUPPLIERS 2026-04-24 (2026-04-24) -0.02: memo_record ? of 1",
		"forecast timing CAD OP_SUPPLIERS 2026-04-22 (2026-04-17) -300.00: payment run2's payment# of 1 to BP90000001",
	})
	row := entryLines(e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&liquidity_item=REC_CUSTOMERS"), names)
	want(t, "the receipts' row, the forecast's then the actuals', by day", row, []string{
		"forecast amount CAD REC_CUSTOMERS 2026-04-23 (2026-04-17) 2000.00: memo_record receipt# of 1",
		"forecast amount CAD REC_CUSTOMERS 2026-04-24 (2026-04-24) 100.00: memo_record series@2026-04-24 of 1",
		"actual amount CAD REC_CUSTOMERS 2026-04-23 (2026-04-17) 1900.00: memo_record receipt#, bank_statement CAD statement# of 2 #2 B",
		"actual amount CAD REC_CUSTOMERS 2026-04-24 (2026-04-24) 100.00: memo_record ?, memo_record series@2026-04-24, bank_statement CAD statement# of 3 #3 B",
		"actual unclassified CAD REC_CUSTOMERS 2026-05-06 (2026-05-01) 500.00: bank_statement CAD statement# of 1 #7 B",
	})
	want(t, "the unclassified row", entryLines(e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&unclassified=true"), names), []string(nil))
	parent := entryLines(e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&liquidity_item=OPERATING"), names)
	sort.Strings(parent)
	want(t, "a parent's row, its descendants'", parent, got)
	want(t, "an item of no entries", entryLines(e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&liquidity_item=TAX"), names), []string(nil))
	receipt := e.docsOf(t, "&from=2026-04-17&to=2026-04-23&lag=0&side=actual&attribution=amount&currency=CAD&liquidity_item=REC_CUSTOMERS")
	want(t, "narrowed to the receipt's line", fmt.Sprint(receipt["count"], " ", len(receipt["items"].([]any))), "1 1")
	en := receipt["items"].([]any)[0].(map[string]any)
	docs := en["documents"].([]any)
	var ref string
	pgtest.Scan(t, `SELECT statement_ref FROM payments.bank_statement WHERE id = $1`, []any{docs[1].(map[string]any)["id"]}, &ref)
	want(t, "the numbers: the memo record's reference, the statement's", fmt.Sprint(docs[0].(map[string]any)["number"], " ", docs[1].(map[string]any)["number"],
		" ", en["bank_reference"]), "CUST-1 "+ref+" B")
	var seen []string
	query := "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&page_size=7"
	for pages := 0; ; pages++ {
		page := e.docsOf(t, query)
		seen = append(seen, entryLines(page, names)...)
		want(t, "count over every page", page["count"], float64(len(got)))
		next, ok := page["next_page_token"]
		if !ok {
			want(t, "pages", pages, 3)
			break
		}
		query = "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&page_size=7&page_token=" + fmt.Sprint(next)
	}
	sort.Strings(seen)
	want(t, "the pages, every entry once", seen, got)
	// A line read between two pages moves no other entry: every entry of
	// the first read is listed once over the pages either side of it.
	first := e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&page_size=7")
	a := actualsEnv{forecastEnv: e.forecastEnv}
	late := e.statement(t, e.main, "CAD", "2026-04-17", "2026-04-17", 0, 0, nil, "imported")
	a.actualLine(t, late, 1, "2026-04-17", 4200, "unmatched", "", target{})
	rest := entryLines(first, names)
	for query := "&page_token=" + fmt.Sprint(first["next_page_token"]); ; {
		page := e.docsOf(t, "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&page_size=7"+query)
		rest = append(rest, entryLines(page, names)...)
		next, ok := page["next_page_token"]
		if !ok {
			break
		}
		query = "&page_token=" + fmt.Sprint(next)
	}
	counted := map[string]int{}
	for _, l := range rest {
		counted[l]++
	}
	for _, l := range got {
		want(t, "listed once across a line read between pages: "+l, counted[l], 1)
	}
	want(t, "and the new line", len(rest), len(got)+1)
	e.pagedOnceAround(t, a, late, names)
	e.twoLinesOfOneEntry(t, a, names)
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/liquidity-forecast-variance/documents?legal_entity_id="+e.le.String()+
		"&from=2026-04-17&to=2026-05-21&lag=0&page_token=nope", "", "")
	p := e.Problem(422, "request.validation_failed", "GET", "/v1/liquidity-forecast-variance/documents?legal_entity_id="+e.le.String()+
		"&from=2026-04-17&to=2026-05-21&lag=0&liquidity_item=OPERATING&unclassified=true", "", "")
	want(t, "an item and the unclassified row", fmt.Sprint(p["errors"].([]any)[0].(map[string]any)["field"]), "unclassified")
	e.Problem(409, "cash.snapshot_not_taken", "GET", "/v1/liquidity-forecast-variance/documents?legal_entity_id="+e.le.String()+
		"&from=2026-04-17&to=2026-05-21", "", "")
}

// allPages lists the entries of every page of the query at the page size,
// failing once it has paged more pages than there are entries: a cursor
// that does not move on would page for ever.
func (e varianceEnv) allPages(t *testing.T, query string, size int, names map[string]string) []string {
	t.Helper()
	var seen []string
	for pages, token := 1, ""; ; pages++ {
		page := e.docsOf(t, fmt.Sprintf("%s&page_size=%d%s", query, size, token))
		seen = append(seen, entryLines(page, names)...)
		next, ok := page["next_page_token"]
		if !ok {
			return seen
		}
		if pages > int(page["count"].(float64)) {
			t.Fatalf("paged %d pages of %v entries at %d a page, want the pages to end", pages, page["count"], size)
		}
		token = "&page_token=" + fmt.Sprint(next)
	}
}

// listedOnce checks that every entry of the list is among the seen once.
func listedOnce(t *testing.T, what string, list, seen []string) {
	t.Helper()
	counted := map[string]int{}
	for _, l := range seen {
		counted[l]++
	}
	for _, l := range list {
		want(t, what+": "+l, counted[l], 1)
	}
}

// pagedOnceAround: a line read between two pages of the actuals, sorting
// before the first page's last entry, moves no entry of the first read
// (round 2 keyed a piece by its position among all, which the new line
// moved): each is listed once over the pages either side of it.
func (e varianceEnv) pagedOnceAround(t *testing.T, a actualsEnv, statement uuid.UUID, names map[string]string) {
	t.Helper()
	query := "&from=2026-04-17&to=2026-05-21&lag=0&grain=week&side=actual"
	all := entryLines(e.docsOf(t, query), names)
	first := e.docsOf(t, query+"&page_size=4")
	seen := entryLines(first, names)
	want(t, "the first page's last entry, after the new line's day", strings.Fields(seen[3])[4], "2026-04-23")
	a.actualLine(t, statement, 2, "2026-04-17", 4300, "unmatched", "", target{})
	for token := "&page_token=" + fmt.Sprint(first["next_page_token"]); ; {
		page := e.docsOf(t, query+"&page_size=4"+token)
		seen = append(seen, entryLines(page, names)...)
		next, ok := page["next_page_token"]
		if !ok {
			break
		}
		token = "&page_token=" + fmt.Sprint(next)
	}
	listedOnce(t, "listed once across an actual read before the cursor", all, seen)
	want(t, "and nothing else", len(seen), len(all))
}

// twoLinesOfOneEntry: a payment settling two open-item lines of one
// journal entry is two entries, keyed by the item's ledger line (the
// payments context's payment_item key), never one key for both; paged
// one and two at a time, every entry is listed once and the pages end.
func (e varianceEnv) twoLinesOfOneEntry(t *testing.T, a actualsEnv, names map[string]string) {
	t.Helper()
	entry := e.item(t, e.Supplier, "2026-05-01", "CAD", -10000, nil)
	p := a.paid(t, e.Supplier, 10000, nil)
	e.exec(t, `INSERT INTO payments.payment_item (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no, amount, due_date)
		VALUES ($1, $2, 2026, $3, 1, 6000, '2026-05-11'), ($1, $2, 2026, $3, 2, 4000, '2026-05-11')`, e.Tenant.ID, p, entry)
	s := e.statement(t, e.main, "CAD", "2026-05-11", "2026-05-11", 0, 0, nil, "imported")
	a.actualLine(t, s, 1, "2026-05-11", -10000, "posted", "matched", target{payments: []uuid.UUID{p}})
	query := "&from=2026-04-17&to=2026-05-21&lag=0&grain=week"
	all := entryLines(e.docsOf(t, query), names)
	var ofEntry []string
	for _, l := range all {
		if strings.Contains(l, "2026-05-11") {
			ofEntry = append(ofEntry, strings.SplitN(l, ":", 2)[0])
		}
	}
	sort.Strings(ofEntry)
	want(t, "the payment's two items of one entry", ofEntry, []string{
		"actual new CAD OP_SUPPLIERS 2026-05-11 (2026-05-08) -40.00",
		"actual new CAD OP_SUPPLIERS 2026-05-11 (2026-05-08) -60.00",
	})
	for _, size := range []int{1, 2} {
		t.Run(fmt.Sprint("page size ", size), func(t *testing.T) {
			seen := e.allPages(t, query, size, names)
			listedOnce(t, "listed once", all, seen)
			want(t, "and nothing else", len(seen), len(all))
		})
	}
}

// TestVarianceDocumentsGated: each document shows only to a caller who
// may use its context's read permission, a line's number only with its
// statement; the entries and their counts show to every reader of the
// forecast.
func TestVarianceDocumentsGated(t *testing.T) {
	e := setupVariance(t)
	names := e.names(t)
	query := "&from=2026-04-17&to=2026-04-23&lag=0&liquidity_item=OP_SUPPLIERS"
	// Lines of one day are in the snapshot's order, by a digest of ids.
	sorted := func(l []string) []string { sort.Strings(l); return l }
	want(t, "no statement read, nor its line", sorted(entryLines(e.docsOf(t, query), names)), []string{
		"actual amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00: payment run's payment#, journal_entry ?, supplier_invoice ? of 4 to BP90000001",
		"actual amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00: payment transit# of 2 to BP90000001",
		"forecast amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00: payment run's payment# of 1 to BP90000001",
		"forecast amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00: payment transit# of 1 to BP90000001",
		"forecast missing USD OP_SUPPLIERS 2026-04-22 (2026-04-22) -0.02: memo_record ? of 1",
		"forecast timing CAD OP_SUPPLIERS 2026-04-22 (2026-04-22) -300.00: payment run2's payment# of 1 to BP90000001",
	})
	for _, p := range drillPerms {
		e.Tenant.Revoke(t, p, e.Tenant.Human)
	}
	want(t, "the forecast's read alone", sorted(entryLines(e.docsOf(t, query), names)), []string{
		"actual amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00:  of 4",
		"actual amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00:  of 2",
		"forecast amount CAD OP_SUPPLIERS 2026-04-17 (2026-04-17) -800.00:  of 1",
		"forecast amount USD OP_SUPPLIERS 2026-04-17 (2026-04-17) -300.00:  of 1",
		"forecast missing USD OP_SUPPLIERS 2026-04-22 (2026-04-22) -0.02:  of 1",
		"forecast timing CAD OP_SUPPLIERS 2026-04-22 (2026-04-22) -300.00:  of 1",
	})
}
