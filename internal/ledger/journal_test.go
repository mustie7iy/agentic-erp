package ledger_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func (e env) rate(tb testing.TB, from, to, validFrom, rate string) {
	tb.Helper()
	e.Must(201, "POST", "/v1/exchange-rates", fmt.Sprintf(`{"rate_type": "average", "from_currency": %q, "to_currency": %q, "valid_from": %q, "rate": %q, "source": "ECB"}`,
		from, to, validFrom, rate), "")
}

func amounts(tb testing.TB, je map[string]any, field string) string {
	tb.Helper()
	var out []string
	for _, l := range je["lines"].([]any) {
		out = append(out, l.(map[string]any)[field].(map[string]any)["amount"].(string))
	}
	return strings.Join(out, ",")
}

// Rounding differences of converted amounts go to the largest line, in local
// and in group currency; a rate maintained only the other way is inverted.
func TestConversion(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "1000", "100000"), e.account(t, "1000", "400000")
	// Without a USD rate there is no group currency amount.
	e.Problem(422, "ledger.no_exchange_rate", "POST", "/v1/journal-entries", entry(e.us, "2026-03-15", "USD", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	e.rate(t, "GBP", "USD", "2026-01-01", "1.5")
	e.rate(t, "EUR", "USD", "2026-01-01", "1.25")
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.us, "2026-03-15", "GBP", line{bank, "0.01", ""}, line{bank, "0.01", ""},
		line{revenue, "-0.02", ""}), "")
	if got := amounts(t, je, "amount_lc"); got != "0.02,0.02,-0.04" {
		t.Errorf("converted amount_lc: got %s, want 0.02,0.02,-0.04", got)
	}
	if got := amounts(t, je, "amount_gc"); got != "0.02,0.02,-0.04" {
		t.Errorf("converted amount_gc: got %s, want 0.02,0.02,-0.04", got)
	}
	for field, want := range map[string]string{"rate_tc_lc": "1.500000000000", "rate_lc_gc": "0.800000000000", "local_currency": "USD"} {
		if je[field] != want {
			t.Errorf("converted %s: got %v, want %s", field, je[field], want)
		}
	}
	// Zero-digit and three-digit currencies scale both ways.
	deBank, deRevenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	e.rate(t, "JPY", "EUR", "2026-01-01", "0.0062")
	e.rate(t, "KWD", "EUR", "2026-01-01", "3")
	yen, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "JPY", line{deBank, "1000", ""}, line{deRevenue, "-1000", ""}), "")
	dinar, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "KWD", line{deBank, "1.000", ""}, line{deRevenue, "-1.000", ""}), "")
	if got := amounts(t, yen, "amount_lc"); got != "6.20,-6.20" {
		t.Errorf("yen amount_lc: got %s, want 6.20,-6.20", got)
	}
	if got := amounts(t, dinar, "amount_lc"); got != "3.00,-3.00" {
		t.Errorf("dinar amount_lc: got %s, want 3.00,-3.00", got)
	}
	if got := amounts(t, dinar, "amount"); got != "1.000,-1.000" {
		t.Errorf("dinar amount: got %s, want 1.000,-1.000", got)
	}
	// A later rate in the other direction wins over an older direct one.
	e.rate(t, "USD", "GBP", "2026-03-01", "0.5")
	later, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.us, "2026-03-15", "GBP", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
	if later["rate_tc_lc"] != "2.000000000000" {
		t.Fatalf("inverse rate: %v", later["rate_tc_lc"])
	}
	e.Problem(422, "ledger.no_exchange_rate", "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "CHF", line{deBank, "1.00", ""}, line{deRevenue, "-1.00", ""}), "")
	e.rate(t, "CHF", "EUR", "2026-01-01", "1000")
	e.Problem(422, "ledger.amount_too_large", "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "CHF",
		line{deBank, "900000000000000.00", ""}, line{deRevenue, "-900000000000000.00", ""}), "")
	// Group currency overflows too.
	e.rate(t, "USD", "EUR", "2026-04-01", "1000")
	e.Problem(422, "ledger.amount_too_large", "POST", "/v1/journal-entries", entry(e.us, "2026-04-15", "USD",
		line{bank, "900000000000000.00", ""}, line{revenue, "-900000000000000.00", ""}), "")
}

func TestPostingRefusals(t *testing.T) {
	e := setup(t)
	bank, revenue, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "211000")
	usBank := e.account(t, "1000", "100000")
	ok := []line{{bank, "1.00", ""}, {revenue, "-1.00", ""}}
	blocked := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Blocked', 'DE', 'EUR', gen_random_uuid(), 'blocked', $3, $3)`, e.ten.ID, blocked, e.ten.Human.ID)
	pending := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO partner.business_partner (tenant_id, id, number, kind, name, country_code, status, created_by, updated_by)
		VALUES ($1, $2, 'BP00000009', 'organization', 'Pending', 'DE', 'pending', $3, $3)`, e.ten.ID, pending, e.ten.Human.ID)
	inactive, unset := e.account(t, "2000", "610000"), e.account(t, "2000", "530000")
	pgtest.Exec(t, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1 WHERE id = $1`, inactive)
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE account_id = $1`, unset)

	cases := map[string]struct {
		body   string
		status int
		code   string
	}{
		"unknown legal entity": {entry(uuid.New(), "2026-03-15", "EUR", ok...), 422, "request.reference_not_found"},
		"blocked legal entity": {entry(blocked, "2026-03-15", "EUR", ok...), 422, "ledger.legal_entity_not_active"},
		"unknown ledger": {strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency"`, `"ledger_id": "`+uuid.NewString()+`", "currency"`, 1),
			422, "ledger.unknown_ledger"},
		"unknown currency":  {entry(e.de, "2026-03-15", "XXQ", ok...), 422, "ledger.unknown_currency"},
		"currency mismatch": {strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency": "EUR"}}, {`, `"currency": "USD"}}, {`, 1), 422, "ledger.currency_mismatch"},
		"decimals":          {entry(e.de, "2026-03-15", "EUR", line{bank, "1.0", ""}, line{revenue, "-1.0", ""}), 400, "request.invalid_amount"},
		"zero":              {entry(e.de, "2026-03-15", "EUR", line{bank, "0.00", ""}, line{revenue, "0.00", ""}), 422, "ledger.zero_amount"},
		"unbalanced":        {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{revenue, "-2.00", ""}), 422, "ledger.unbalanced"},
		"unknown account":   {entry(e.de, "2026-03-15", "EUR", line{uuid.NewString(), "1.00", ""}, line{revenue, "-1.00", ""}), 422, "ledger.account_not_postable"},
		"other company":     {entry(e.de, "2026-03-15", "EUR", line{usBank, "1.00", ""}, line{revenue, "-1.00", ""}), 422, "ledger.account_not_postable"},
		"inactive account":  {entry(e.de, "2026-03-15", "EUR", line{inactive, "1.00", ""}, line{revenue, "-1.00", ""}), 422, "ledger.account_not_postable"},
		"blocked account":   {entry(e.de, "2026-03-15", "EUR", line{unset, "1.00", ""}, line{revenue, "-1.00", ""}), 422, "ledger.account_not_postable"},
		"no partner":        {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{ap, "-1.00", ""}), 422, "ledger.partner_required"},
		"partner on bank":   {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", e.supplier.String()}, line{revenue, "-1.00", ""}), 422, "ledger.partner_not_allowed"},
		"pending partner":   {entry(e.de, "2026-03-15", "EUR", line{bank, "1.00", ""}, line{ap, "-1.00", pending.String()}), 422, "ledger.partner_not_postable"},
		"no period":         {entry(e.de, "2030-01-15", "EUR", ok...), 422, "ledger.no_fiscal_period"},
		"special period":    {entry(e.de, "2026-03-15", "EUR", ok...), 0, ""},
		"no number range":   {strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency"`, `"document_type": "KR", "currency"`, 1), 422, "config.no_number_range"},
		"tax code": {strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency": "EUR"}}, {`, `"currency": "EUR"}, "tax_code": "V9"}, {`, 1),
			422, "ledger.unknown_tax_code"},
		"unit": {strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency": "EUR"}}, {`, `"currency": "EUR"}, "quantity": {"value": "1", "unit": "XX"}}, {`, 1),
			422, "ledger.unknown_unit"},
	}
	for name, c := range cases {
		if c.status == 0 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			code, m, _ := e.in(t).Do("POST", "/v1/journal-entries", c.body, "")
			if code != c.status {
				t.Errorf("status: got %d, want %d (%v)", code, c.status, m)
			}
			if m["code"] != c.code {
				t.Errorf("code: got %v, want %s", m["code"], c.code)
			}
		})
	}

	// A period that is not open, an exhausted range, and a tax code that is valid.
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND fiscal_year = 2026 AND period = 2`, e.de)
	e.Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", entry(e.de, "2026-02-10", "EUR", ok...), "")
	pgtest.Exec(t, `INSERT INTO ledger.tax_code (tenant_id, legal_entity_id, code, description, direction, rate_pct, account_id, valid_from, created_by)
		VALUES ($1, $2, 'V9', 'Input', 'input', 19, $3, '2026-01-01', $4)`, e.ten.ID, e.de, e.account(t, "2000", "140000"), e.ten.Human.ID)
	taxed, _ := e.Must(201, "POST", "/v1/journal-entries", strings.Replace(entry(e.de, "2026-03-15", "EUR", ok...), `"currency": "EUR"}}, {`,
		`"currency": "EUR"}, "tax_code": "V9", "line_text": "Fees", "quantity": {"value": "12.500", "unit": "KG"}}, {`, 1), "")
	l := taxed["lines"].([]any)[0].(map[string]any)
	if l["tax_code"] != "V9" {
		t.Errorf("line tax_code: got %v, want V9", l["tax_code"])
	}
	if l["line_text"] != "Fees" {
		t.Errorf("line line_text: got %v, want Fees", l["line_text"])
	}
	if got := apitest.Object(t, l, "quantity")["value"]; got != "12.5" {
		t.Errorf("line quantity: got %v, want 12.5", got)
	}
	pgtest.Exec(t, `UPDATE config.number_range SET next_value = last_value + 1, version = version + 1 WHERE legal_entity_id = $1 AND document_type = 'journal_sa'`, e.de)
	e.Problem(409, "config.number_range_exhausted", "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", ok...), "")
}

func TestReversalRules(t *testing.T) {
	e := setup(t)
	bank, revenue, ap := e.account(t, "2000", "100000"), e.account(t, "2000", "400000"), e.account(t, "2000", "211000")
	body := strings.Replace(entry(e.de, "2026-03-15", "EUR", line{bank, "5.00", ""}, line{ap, "-5.00", e.supplier.String()}),
		`"currency": "EUR"}}, {`, `"currency": "EUR"}, "quantity": {"value": "-3", "unit": "EA"}}, {`, 1)
	je, tag := e.Must(201, "POST", "/v1/journal-entries", strings.Replace(body, `"currency": "EUR", "lines"`, `"currency": "EUR", "reference": "R-1", "lines"`, 1), "")
	path := "/v1/journal-entries/" + je["id"].(string) + ":reverse"
	e.Problem(428, "request.precondition_required", "POST", path, "", "")
	e.Problem(412, "request.version_mismatch", "POST", path, "", `"stale"`)
	e.Problem(404, "request.not_found", "POST", "/v1/journal-entries/"+uuid.NewString()+":reverse", "", "*")

	// The original period closed: the reversal goes to another date.
	pgtest.Exec(t, `UPDATE ledger.period_status SET status = 'closed', version = version + 1 WHERE legal_entity_id = $1 AND period = 3`, e.de)
	e.Problem(409, "ledger.period_not_open", "POST", path, "", tag)
	// A blocked account stops the reversal as it stops postings.
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = true, version = version + 1 WHERE account_id = $1`, bank)
	e.Problem(422, "ledger.account_not_postable", "POST", path, `{"posting_date": "2026-04-01"}`, tag)
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_posting_blocked = false, version = version + 1 WHERE account_id = $1`, bank)
	rev, _ := e.Must(200, "POST", path, `{"posting_date": "2026-04-01", "header_text": "Wrong supplier"}`, tag)
	l := rev["lines"].([]any)[0].(map[string]any)
	for field, want := range map[string]any{"fiscal_period": float64(4), "header_text": "Wrong supplier", "reference": "R-1"} {
		if rev[field] != want {
			t.Errorf("reversal %s: got %v, want %v", field, rev[field], want)
		}
	}
	if got := apitest.Object(t, l, "quantity")["value"]; got != "3" {
		t.Errorf("reversal quantity: got %v, want 3", got)
	}
	if got := amounts(t, rev, "amount"); got != "-5.00,5.00" {
		t.Errorf("reversal amounts: got %s, want -5.00,5.00", got)
	}
	_, now := e.Must(200, "GET", "/v1/journal-entries/"+je["id"].(string), "", "")
	if now == tag {
		t.Fatal("the ETag did not change when the entry was reversed")
	}
	e.Problem(409, "ledger.already_reversed", "POST", path, "", now)
	_, revTag := e.Must(200, "GET", "/v1/journal-entries/"+rev["id"].(string), "", "")
	e.Problem(409, "ledger.cannot_reverse_reversal", "POST", "/v1/journal-entries/"+rev["id"].(string)+":reverse", "", revTag)

	// Quantities are negated both ways.
	pos, posTag := e.Must(201, "POST", "/v1/journal-entries", strings.Replace(entry(e.de, "2026-04-02", "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}),
		`"currency": "EUR"}}, {`, `"currency": "EUR"}, "quantity": {"value": "2", "unit": "EA"}}, {`, 1), "")
	back, _ := e.Must(200, "POST", "/v1/journal-entries/"+pos["id"].(string)+":reverse", "", posTag)
	if back["lines"].([]any)[0].(map[string]any)["quantity"].(map[string]any)["value"] != "-2" {
		t.Fatalf("negated quantity: %v", back)
	}
}

func TestJournalLists(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	var ids []string
	for _, d := range []string{"2026-03-20", "2026-03-10", "2026-04-05"} {
		je, _ := e.Must(201, "POST", "/v1/journal-entries", entry(e.de, d, "EUR", line{bank, "1.00", ""}, line{revenue, "-1.00", ""}), "")
		ids = append(ids, je["id"].(string))
	}
	usBank, usRevenue := e.account(t, "1000", "100000"), e.account(t, "1000", "400000")
	e.rate(t, "USD", "EUR", "2026-01-01", "0.9")
	e.Must(201, "POST", "/v1/journal-entries", entry(e.us, "2026-03-01", "USD", line{usBank, "1.00", ""}, line{usRevenue, "-1.00", ""}), "")
	list := func(q string) []string {
		page, _ := e.Must(200, "GET", "/v1/journal-entries?"+q, "", "")
		var out []string
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["posting_date"].(string))
		}
		if tok, ok := page["next_page_token"].(string); ok {
			out = append(out, "next:"+tok)
		}
		return out
	}
	de := "legal_entity_id=" + e.de.String()
	if got := strings.Join(list(de), ","); got != "2026-03-10,2026-03-20,2026-04-05" {
		t.Fatalf("by posting date: %s", got)
	}
	paged := list(de + "&page_size=2")
	if len(paged) != 3 {
		t.Fatalf("paged by posting date: got %d %v, want 3", len(paged), paged)
	}
	if !strings.HasPrefix(paged[2], "next:") {
		t.Fatalf("paged by posting date: got last %q, want a next page token", paged[2])
	}
	if rest := list(de + "&page_size=2&page_token=" + strings.TrimPrefix(paged[2], "next:")); len(rest) != 1 {
		t.Fatalf("page 2 by posting date: got %d %v, want 1", len(rest), rest)
	} else if rest[0] != "2026-04-05" {
		t.Errorf("page 2 by posting date: got %s, want 2026-04-05", rest[0])
	}
	if got := strings.Join(list(de+"&order_by=posting_date%20desc&posting_date_from=2026-03-15&posting_date_to=2026-04-30&fiscal_year=2026"), ","); got != "2026-04-05,2026-03-20" {
		t.Fatalf("filtered: %s", got)
	}
	if got := list(de + "&document_number=100001&source_context=ledger"); len(got) != 1 {
		t.Fatalf("by number: got %d %v, want 1", len(got), got)
	} else if got[0] != "2026-03-10" {
		t.Errorf("by number: got %s, want 2026-03-10", got[0])
	}
	if got := list("source_document_id=" + uuid.NewString()); len(got) != 0 {
		t.Fatalf("by source document: %v", got)
	}
	first := list(de + "&order_by=document_number&page_size=2")
	if len(first) != 3 {
		t.Fatalf("page 1: got %d %v, want 3", len(first), first)
	}
	if !strings.HasPrefix(first[2], "next:") {
		t.Fatalf("page 1: got last %q, want a next page token", first[2])
	}
	if rest := list(de + "&order_by=document_number&page_size=2&page_token=" + strings.TrimPrefix(first[2], "next:")); len(rest) != 1 {
		t.Fatalf("page 2: got %d %v, want 1", len(rest), rest)
	} else if rest[0] != "2026-04-05" {
		t.Errorf("page 2: got %s, want 2026-04-05", rest[0])
	}
	if got := list(""); len(got) != 4 {
		t.Fatalf("all: %v", got)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/journal-entries/"+uuid.NewString(), "", "")

	// A principal without company access sees nothing.
	agent := e.Client
	agent.As = e.ten.Agent
	if page, _ := agent.Must(200, "GET", "/v1/journal-entries", "", ""); len(page["items"].([]any)) != 0 {
		t.Fatalf("agent sees %v", page)
	}
	agent.Problem(404, "request.not_found", "GET", "/v1/journal-entries/"+ids[0], "", "")
}
