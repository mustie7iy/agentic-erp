package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// worklist reads company 2000's clearing items of 2026 with the query, one
// line per item: its entry's document number, line, amount, used and
// remaining amounts, whether reversed, and its acquisitions' assets.
func (e assetEnv) worklist(t *testing.T, query string) ([]string, map[string]any) {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/asset-clearing-items?legal_entity_id="+e.DE.String()+"&fiscal_year=2026"+query, "", "")
	var out []string
	for _, it := range page["items"].([]any) {
		it := it.(map[string]any)
		var assets []string
		for _, a := range it["acquisitions"].([]any) {
			a := a.(map[string]any)
			assets = append(assets, fmt.Sprint(a["asset_number"], ":", a["amount"].(map[string]any)["amount"], ":", a["status"]))
		}
		out = append(out, fmt.Sprintf("%s/%v %s used %s left %s %s %s reversed %v [%s]", it["document_number"], it["line_no"], moneyOf(it["amount"]),
			moneyOf(it["used"]), moneyOf(it["remaining"]), moneyOf(it["remaining_lc"]), moneyOf(it["remaining_gc"]), it["source_reversed"],
			strings.Join(assets, " ")))
	}
	return out, page
}

// number is a journal entry's document number.
func (e assetEnv) number(t *testing.T, entry uuid.UUID) string {
	t.Helper()
	var n string
	pgtest.Scan(t, `SELECT document_number FROM ledger.journal_entry WHERE id = $1`, []any{entry}, &n)
	return n
}

// TestClearingItems: the worklist lists company 2000's lines on its
// clearing account in a year with what is used and left, names the
// supplier invoice behind a line, leaves out a reversed entry and its
// reversal while nothing was capitalised from it, shows a reversed line
// with acquisitions, keeps only lines with something left with
// remaining=true, pages by posting date, entry and line, and has nothing
// for a company the caller cannot act in or one without asset accounts.
func TestClearingItems(t *testing.T) {
	e := newAcquisitions(t)
	cc := fmt.Sprintf(`, "cost_center_id": %q`, e.c.Admin)
	a, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	b, _ := e.create(t, e.body(e.DE, "MACHINES", cc))
	split := e.post(t, "2000", "2026-06-01", "EUR", nil, line{"169000", "300.00"}, line{"100000", "-300.00"})
	gone := e.post(t, "2000", "2026-06-02", "EUR", nil, line{"169000", "40.00"}, line{"100000", "-40.00"})
	_, tag := e.Must(200, "GET", "/v1/journal-entries/"+gone.String(), "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+gone.String()+":reverse", "", tag)
	held := e.post(t, "2000", "2026-06-03", "EUR", nil, line{"169000", "70.00"}, line{"100000", "-70.00"})
	e.acquire(t, b["id"], held, 1, "")
	_, tag = e.Must(200, "GET", "/v1/journal-entries/"+held.String(), "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+held.String()+":reverse", "", tag)
	invoice := e.invoice(t, "MILL-77", "2026-06-04")
	e.acquire(t, a["id"], split, 1, `, "amount": {"amount": "100.00", "currency": "EUR"}`)
	// A payables entry whose document is no supplier invoice names none.
	var payablesEntry uuid.UUID
	if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		day := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
		posted, err := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}}.Post(ctx,
			ledger.Posting{LegalEntityID: e.DE, DocumentType: "SA", DocumentDate: day, PostingDate: day, Currency: "EUR", SourceContext: "payables",
				SourceDocumentID: uuid.Must(uuid.NewV7()), Lines: []ledger.PostingLine{{AccountID: e.Account(t, "2000", "169000"), Amount: 500},
					{AccountID: e.Account(t, "2000", "100000"), Amount: -500}}})
		payablesEntry = posted.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sp, hd := e.number(t, split), e.number(t, held)
	got, page := e.worklist(t, "")
	want(t, "the worklist", strings.Join(got, "\n"), strings.Join([]string{
		sp + "/1 300.00 EUR used 100.00 EUR left 200.00 EUR 200.00 EUR 200.00 EUR reversed false [1:100.00:posted]",
		hd + "/1 70.00 EUR used 70.00 EUR left 0.00 EUR 0.00 EUR 0.00 EUR reversed true [2:70.00:posted]",
		e.number(t, invoice) + "/1 100.00 EUR used 0.00 EUR left 100.00 EUR 100.00 EUR 100.00 EUR reversed false []",
		e.number(t, payablesEntry)+"/1 5.00 EUR used 0.00 EUR left 5.00 EUR 5.00 EUR 5.00 EUR reversed false []",
	}, "\n"))
	items := page["items"].([]any)
	first, last := items[0].(map[string]any), items[2].(map[string]any)
	want(t, "the account", fmt.Sprint(first["account_number"], " ", first["account_name"], " ", first["source_context"]), "169000 Asset clearing ledger")
	inv := last["supplier_invoice"].(map[string]any)
	want(t, "the supplier invoice", fmt.Sprint(last["source_context"], " ", inv["supplier_reference"], " ", inv["supplier_name"], " ", inv["supplier_id"]),
		fmt.Sprint("payables MILL-77 BP90000001 ", e.Supplier))

	e.acquire(t, b["id"], split, 1, "")
	got, _ = e.worklist(t, "&remaining=true")
	want(t, "with something left", strings.Join(got, "\n"), e.number(t, invoice)+"/1 100.00 EUR used 0.00 EUR left 100.00 EUR 100.00 EUR 100.00 EUR reversed false []\n"+
		e.number(t, payablesEntry)+"/1 5.00 EUR used 0.00 EUR left 5.00 EUR 5.00 EUR 5.00 EUR reversed false []")

	var pages []string
	for token := ""; ; {
		got, page := e.worklist(t, "&page_size=1"+token)
		pages = append(pages, got...)
		next, ok := page["next_page_token"].(string)
		if !ok {
			break
		}
		token = "&page_token=" + next
	}
	want(t, "pages", len(pages), 4)
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/asset-clearing-items?legal_entity_id="+e.DE.String()+"&fiscal_year=2025&page_token=x", "", "")

	reader := e.Client
	reader.As = e.Reader(t, "reader", e.US)
	page, _ = reader.Must(200, "GET", "/v1/asset-clearing-items?legal_entity_id="+e.DE.String()+"&fiscal_year=2026", "", "")
	want(t, "a company the caller cannot act in", len(page["items"].([]any)), 0)
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Acme Ltd', 'GB', 'GBP', gen_random_uuid(), 'active', $3, $3)`, e.Tenant.ID, other, e.Tenant.Human.ID)
	page, _ = e.Must(200, "GET", "/v1/asset-clearing-items?legal_entity_id="+other.String()+"&fiscal_year=2026", "", "")
	want(t, "a company the configuration has not", len(page["items"].([]any)), 0)
	page, _ = e.Must(200, "GET", "/v1/asset-clearing-items?legal_entity_id="+uuid.NewString()+"&fiscal_year=2026", "", "")
	want(t, "an unknown company", len(page["items"].([]any)), 0)
}

// invoice posts a supplier invoice of company 2000 on the day with an
// item of EUR 100.00 and input tax V1 on the clearing account, and
// returns its entry.
func (e assetEnv) invoice(t *testing.T, ref, day string) uuid.UUID {
	t.Helper()
	item := fmt.Sprintf(`{"account_id": %q, "description": "Milling machine", "quantity": {"value": "1", "unit": "EA"}, "amount": {"amount": "100.00", "currency": "EUR"},
		"tax_code": "V1"}`, e.Account(t, "2000", "169000"))
	id := e.SupplierInvoice(t, e.Supplier, ref, "119.00", "19.00", item)
	_, tag := e.Must(200, "GET", "/v1/supplier-invoices/"+id.String(), "", "")
	inv, _ := e.Must(200, "POST", "/v1/supplier-invoices/"+id.String()+":post", fmt.Sprintf(`{"posting_date": %q}`, day), tag)
	return uuid.MustParse(inv["journal_entry_id"].(string))
}
