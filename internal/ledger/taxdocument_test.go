package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// A year-end adjustment's tax document (ADR-0060 §3 and question 2; the
// UAE tax specialist's answer C10): a posting into a special period
// carries a tax code when its company is in the UAE and it names the tax
// invoice or tax credit note its VAT rests on, whose date is its tax date.

// setupUAE is setupSpecial with company 2000 in the United Arab Emirates.
func setupUAE(t *testing.T) specialEnv {
	t.Helper()
	s := setupSpecial(t)
	pgtest.Exec(t, `UPDATE tenancy.legal_entity SET country_code = 'AE', version = version + 1 WHERE tenant_id = $1 AND id = $2`, s.ten.ID, s.de)
	return s
}

// creditNote is a tax document as a request names it.
const creditNote = `"tax_document": {"kind": "tax_credit_note", "number": "TCN-2026-0042", "date": "2026-08-15"}`

// taxedAdjustment is a purchase of company 2000 dated day with 19.00 of
// input tax V1, with the fields before its currency: a year-end
// adjustment when they name a special period.
func (s specialEnv) taxedAdjustment(t *testing.T, day, fields string) string {
	t.Helper()
	if fields != "" {
		fields += ", "
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, %s"currency": "EUR", "lines": [
		{"account_id": %q, "amount": {"amount": "100.00", "currency": "EUR"}, "tax_code": "V1"},
		{"account_id": %q, "amount": {"amount": "19.00", "currency": "EUR"}, "tax_code": "V1"},
		{"account_id": %q, "amount": {"amount": "-119.00", "currency": "EUR"}}]}`, s.de, day, day, fields,
		s.account(t, "2000", "520000"), s.account(t, "2000", "140000"), s.bank)
}

// documentOf is an entry's tax document as "kind number date", or "".
func documentOf(m map[string]any) string {
	d, ok := m["tax_document"].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprint(d["kind"], " ", d["number"], " ", d["date"])
}

// taxDates are the tax dates of the tax items of the entry.
func (s specialEnv) taxDates(t *testing.T, entry any) string {
	t.Helper()
	var dates string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT coalesce(string_agg(DISTINCT tax_date::text, ','), '') FROM ledger.tax_item
		WHERE tenant_id = $1 AND journal_entry_id = $2`, s.ten.ID, entry).Scan(&dates); err != nil {
		t.Fatalf("tax items of %v: %v", entry, err)
	}
	return dates
}

// TestTaxDocumentRefusals: a UAE company's year-end adjustment with a tax
// code is refused without its tax document; a tax document is refused of
// an unknown kind, on an entry that names no special period or carries no
// tax code, or beside another tax date; and another country's company
// keeps the refusal of tax codes, document or not.
func TestTaxDocumentRefusals(t *testing.T) {
	s := setupUAE(t)
	plain := with(s.adjustment("2026-12-31", 13), creditNote)
	for _, c := range []struct {
		name, body, code, field string
		status                  int
	}{
		{"no tax document", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13`), "ledger.special_period_tax_code", "lines[0].tax_code", 422},
		{"a document of another kind", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, "tax_document": {"kind": "receipt", "number": "R-1", "date": "2026-08-15"}`),
			"ledger.invalid_tax_document", "tax_document.kind", 422},
		{"a document in a regular period", s.taxedAdjustment(t, "2026-12-31", creditNote), "ledger.invalid_tax_document", "tax_document", 422},
		{"a document without a tax code", plain, "ledger.invalid_tax_document", "tax_document", 422},
		{"another tax date", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, "tax_date": "2026-12-31", `+creditNote), "ledger.invalid_tax_document", "tax_date", 422},
		{"a blank number", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, "tax_document": {"kind": "tax_invoice", "number": " ", "date": "2026-08-15"}`),
			"request.invalid_body", "", 400},
		// The contract's pattern knows Unicode blanks as the database's check does.
		{"a number ending in an em space", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, "tax_document": {"kind": "tax_invoice", "number": "TI-1\u2003", "date": "2026-08-15"}`),
			"request.invalid_body", "", 400},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := s.In(t).Problem(c.status, c.code, "POST", "/v1/journal-entries", c.body, "")
			if c.field == "" {
				return
			}
			if got := fmt.Sprint(apitest.List(t, p, "errors")[0].(map[string]any)["field"]); got != c.field {
				t.Errorf("the refused field: got %s, want %s", got, c.field)
			}
		})
	}
	t.Run("a company of another country", func(t *testing.T) {
		pgtest.Exec(t, `UPDATE tenancy.legal_entity SET country_code = 'CA', version = version + 1 WHERE tenant_id = $1 AND id = $2`, s.ten.ID, s.de)
		s.In(t).Problem(422, "ledger.special_period_tax_code", "POST", "/v1/journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, `+creditNote), "")
	})
	if n := s.journalEntries(t, 13); n != 0 {
		t.Errorf("entries in period 13 after the refusals: got %d, want 0", n)
	}
}

// TestTaxDocumentPosting: a UAE company's year-end adjustment naming its
// tax document posts into the special period with the document's date as
// its tax date, so its tax items are the document's period's, and shows
// the document; a tax date given beside it is the document's date. Its
// reversal into the special period names the document too and takes the
// tax back on the same date; a taxed entry without a document is still
// not reversed there.
func TestTaxDocumentPosting(t *testing.T) {
	s := setupUAE(t)
	je, tag := s.Must(201, "POST", "/v1/journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, `+creditNote), "")
	if got := fmt.Sprint(je["fiscal_year"], "/", je["fiscal_period"], " ", je["posting_date"], " ", je["tax_date"]); got != "2026/13 2026-12-31 2026-08-15" {
		t.Errorf("the adjustment's period, posting date and tax date: got %s, want 2026/13 2026-12-31 2026-08-15", got)
	}
	if got := documentOf(je); got != "tax_credit_note TCN-2026-0042 2026-08-15" {
		t.Errorf("the adjustment's tax document: got %q, want tax_credit_note TCN-2026-0042 2026-08-15", got)
	}
	if got := s.taxDates(t, je["id"]); got != "2026-08-15" {
		t.Errorf("the tax dates of its items: got %q, want 2026-08-15", got)
	}
	same, sameTag := s.Must(201, "POST", "/v1/journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, "tax_date": "2026-08-15", `+creditNote), "")
	if got := same["tax_date"]; got != "2026-08-15" {
		t.Errorf("the tax date given beside the document: got %v, want 2026-08-15", got)
	}
	sameRev, _ := s.Must(200, "POST", "/v1/journal-entries/"+same["id"].(string)+":reverse", `{}`, sameTag)
	if got := fmt.Sprint(sameRev["fiscal_period"], " ", sameRev["tax_date"], " ", documentOf(sameRev)); got != "12 2026-08-15 tax_credit_note TCN-2026-0042 2026-08-15" {
		t.Errorf("a reversal into the regular period, its period, tax date and document: got %s, want 12 2026-08-15 tax_credit_note TCN-2026-0042 2026-08-15", got)
	}
	regular, regularTag := s.Must(201, "POST", "/v1/journal-entries", s.taxedAdjustment(t, "2026-12-15", ""), "")
	if got := documentOf(regular); got != "" {
		t.Errorf("a regular entry's tax document: got %q, want none", got)
	}
	reverse := func(m map[string]any) string { return "/v1/journal-entries/" + m["id"].(string) + ":reverse" }
	s.Problem(422, "ledger.special_period_tax_code", "POST", reverse(regular), `{"posting_date": "2026-12-31", "fiscal_period": 13}`, regularTag)
	rev, _ := s.Must(200, "POST", reverse(je), `{"fiscal_period": 13}`, tag)
	if got := fmt.Sprint(rev["fiscal_period"], " ", rev["tax_date"], " ", documentOf(rev)); got != "13 2026-08-15 tax_credit_note TCN-2026-0042 2026-08-15" {
		t.Errorf("the reversal's period, tax date and document: got %s, want 13 2026-08-15 tax_credit_note TCN-2026-0042 2026-08-15", got)
	}
	if got := s.taxDates(t, rev["id"]); got != "2026-08-15" {
		t.Errorf("the tax dates of the reversal's items: got %q, want 2026-08-15", got)
	}
}

// TestParkedTaxDocument: a parked entry keeps the tax document it names,
// which PATCH replaces and removes, is held to the rules when parked and
// changed, and is posted with the document and its date as the tax date.
func TestParkedTaxDocument(t *testing.T) {
	s := setupUAE(t)
	s.Problem(422, "ledger.special_period_tax_code", "POST", "/v1/parked-journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13`), "")
	pe, tag := s.Must(201, "POST", "/v1/parked-journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, `+creditNote), "")
	path := "/v1/parked-journal-entries/" + pe["id"].(string)
	if got := documentOf(pe); got != "tax_credit_note TCN-2026-0042 2026-08-15" {
		t.Errorf("the parked entry's tax document: got %q, want tax_credit_note TCN-2026-0042 2026-08-15", got)
	}
	for _, c := range []struct{ name, patch, code string }{
		{"the document removed", `{"tax_document": null}`, "ledger.special_period_tax_code"},
		{"the special period removed", `{"fiscal_period": null}`, "ledger.invalid_tax_document"},
		{"another tax date", `{"tax_date": "2026-12-31"}`, "ledger.invalid_tax_document"},
		{"a document of another kind", `{"tax_document": {"kind": "receipt", "number": "R-1", "date": "2026-08-15"}}`, "ledger.invalid_tax_document"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s.In(t).Problem(422, c.code, "PATCH", path, c.patch, tag)
		})
	}
	pe, tag = s.Must(200, "PATCH", path, `{"tax_document": {"kind": "tax_invoice", "number": "TI-77", "date": "2026-09-01"}}`, tag)
	if got := documentOf(pe); got != "tax_invoice TI-77 2026-09-01" {
		t.Errorf("the document after the change: got %q, want tax_invoice TI-77 2026-09-01", got)
	}
	pe, tag = s.Must(200, "PATCH", path, `{"header_text": "Audit adjustment"}`, tag)
	if got := documentOf(pe); got != "tax_invoice TI-77 2026-09-01" {
		t.Errorf("the document after another change: got %q, want tax_invoice TI-77 2026-09-01 kept", got)
	}
	posted, _ := s.Must(200, "POST", path+":post", "", tag)
	je, _ := s.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	if got := fmt.Sprint(je["fiscal_period"], " ", je["tax_date"], " ", documentOf(je)); got != "13 2026-09-01 tax_invoice TI-77 2026-09-01" {
		t.Errorf("the posted entry's period, tax date and document: got %s, want 13 2026-09-01 tax_invoice TI-77 2026-09-01", got)
	}
	// Without the tax code the document goes too, and the entry is an
	// ordinary adjustment again.
	pe, tag = s.Must(201, "POST", "/v1/parked-journal-entries", s.taxedAdjustment(t, "2026-12-31", `"fiscal_period": 13, `+creditNote), "")
	path = "/v1/parked-journal-entries/" + pe["id"].(string)
	untaxed := fmt.Sprintf(`{"tax_document": null, "lines": [{"account_id": %q, "amount": {"amount": "1.00", "currency": "EUR"}},
		{"account_id": %q, "amount": {"amount": "-1.00", "currency": "EUR"}}]}`, s.bank, s.revenue)
	pe, _ = s.Must(200, "PATCH", path, untaxed, tag)
	if got := documentOf(pe); got != "" {
		t.Errorf("the document after it was removed: got %q, want none", got)
	}
}
