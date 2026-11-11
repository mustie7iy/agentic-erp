package taxfiling_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// A year-end adjustment in the VAT 201 (ADR-0060 §3 and question 2; the
// UAE tax specialist's answer C10): its VAT is reported in the return of
// the period its tax invoice or tax credit note is dated in, not the
// period of its posting date.

// yearEndCredit is a year-end adjustment of company 5000 posted into
// special period 13 of 2026, dated 2026-12-31: a credit of AED 100.00 of
// Dubai sales with 5.00 of VAT, resting on the tax document (a request's
// tax_document member, or none).
func (g vat201) yearEndCredit(t *testing.T, document string) string {
	t.Helper()
	if document != "" {
		document += ", "
	}
	line := func(account, amount, tax string) string {
		return fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "AED"}%s}`, g.e.Account(t, "5000", account), amount, tax)
	}
	dubai := `, "tax_code": "S5", "tax_jurisdiction": "AE-DU"`
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-12-31", "posting_date": "2026-12-31", "fiscal_period": 13, %s"currency": "AED",
		"lines": [%s, %s, %s]}`, g.le, document, line("100000", "-105.00", ""), line("400000", "100.00", dubai), line("240000", "5.00", dubai))
}

// TestVAT201YearEndAdjustment: Maple Gulf Trading's year-end adjustment
// with VAT is refused without its tax document. Naming a tax credit note
// of 15 August, it is in the third quarter's VAT 201, not the second's,
// though posted on 31 December. One whose document is dated in the first
// quarter, whose return is prepared, is posted all the same, where a
// manual entry choosing that tax date is held: its VAT is the quarter's
// late item, in the second quarter's return as an earlier period's.
func TestVAT201YearEndAdjustment(t *testing.T) {
	e, le := vat201Env(t)
	g := vat201{e: e, le: le}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'open', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period = 13`, e.Tenant.ID, le)
	reg := id(t, e, "VAT")
	e.Problem(422, "ledger.special_period_tax_code", "POST", "/v1/journal-entries", g.yearEndCredit(t, ""), "")
	je, _ := e.Must(201, "POST", "/v1/journal-entries",
		g.yearEndCredit(t, `"tax_document": {"kind": "tax_credit_note", "number": "TCN-2026-0042", "date": "2026-08-15"}`), "")
	want(t, "the adjustment's period, posting date and tax date", fmt.Sprint(je["fiscal_period"], " ", je["posting_date"], " ", je["tax_date"]),
		"13 2026-12-31 2026-08-15")
	for _, c := range []struct{ name, start, box, want string }{
		{"the third quarter's VAT", "2026-07-01", "1b_vat", "-5.00/0.00#1"},
		{"the third quarter's supplies", "2026-07-01", "1b_amount", "-100.00/0.00#1"},
		{"the third quarter's tax due", "2026-07-01", "12", "-5.00/0.00"},
		{"the second quarter's VAT", "2026-04-01", "1b_vat", "0.00/0.00#0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			draft, _ := e.In(t).Must(200, "GET", compute(reg, c.start), "", "")
			if got := boxOf(draft, c.box); got != c.want {
				t.Errorf("box %s, amount/prior part[#items]: got %s, want %s", c.box, got, c.want)
			}
		})
	}
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE tenant_id = $1 AND legal_entity_id = $2
		AND fiscal_year = 2026 AND period BETWEEN 1 AND 3`, e.Tenant.ID, le)
	e.Must(201, "POST", returnsOf(reg), `{"period_start": "2026-01-01"}`, "")
	held := fmt.Sprintf(`{"legal_entity_id": %q, "document_date": "2026-05-05", "posting_date": "2026-05-05", "tax_date": "2026-02-15", "currency": "AED",
		"lines": [{"account_id": %q, "amount": {"amount": "105.00", "currency": "AED"}},
			{"account_id": %q, "amount": {"amount": "-100.00", "currency": "AED"}, "tax_code": "S5", "tax_jurisdiction": "AE-DU"},
			{"account_id": %q, "amount": {"amount": "-5.00", "currency": "AED"}, "tax_code": "S5", "tax_jurisdiction": "AE-DU"}]}`,
		le, e.Account(t, "5000", "100000"), e.Account(t, "5000", "400000"), e.Account(t, "5000", "240000"))
	e.Problem(409, "ledger.tax_date_in_return", "POST", "/v1/journal-entries", held, "")
	late, _ := e.Must(201, "POST", "/v1/journal-entries", g.yearEndCredit(t, `"tax_document": {"kind": "tax_invoice", "number": "TI-2026-0007", "date": "2026-02-15"}`), "")
	want(t, "the late adjustment's tax date", fmt.Sprint(late["tax_date"]), "2026-02-15")
	q2, _ := e.Must(200, "GET", compute(reg, "2026-04-01"), "", "")
	want(t, "the second quarter's VAT with the late adjustment, an earlier period's", boxOf(q2, "1b_vat"), "-5.00/-5.00#1")
}
