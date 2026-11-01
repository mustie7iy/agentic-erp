package payables_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// withTaxSettings activates a configuration whose company 2000 has the
// tax settings s, as applying a country pack would (ADR-0049 §6, §8).
func (e env) withTaxSettings(t *testing.T, s config.TaxSettings) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, jsonb_set(v.document, '{companies,1,tax_settings}', $2::jsonb), v.hash, 'rollback',
			v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`,
		e.Tenant.ID, string(b))
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, e.Tenant.ID)
}

// tiers are Canada's documentary tiers (SOR/91-45 s. 3) and the UAE's
// (pending sign-off), in the company's currency.
func tiers() []config.DocumentaryRequirement {
	return []config.DocumentaryRequirement{
		{AtLeast: "100", Requires: []string{config.RequiresSupplierTaxID}, Source: "SOR/91-45 s. 3(b)"},
		{AtLeast: "500", Requires: []string{config.RequiresSupplierTaxID, config.RequiresItemDescriptions}, Source: "SOR/91-45 s. 3(c)"},
	}
}

// warnings are an invoice's documentary warnings as "code threshold
// currency inclusive pending", one per warning.
func warnings(inv map[string]any) []string {
	var out []string
	ws, _ := inv["documentary_warnings"].([]any)
	for _, w := range ws {
		m := w.(map[string]any)
		th := m["threshold"].(map[string]any)
		out = append(out, fmt.Sprintf("%v %v %v %v %v", m["code"], th["amount"], th["currency"], m["threshold_inclusive"], m["pending_sign_off"]))
	}
	return out
}

// TestDocumentaryWarnings: an invoice claiming input tax posts with a
// warning for each documentary requirement of the company's tiers its
// total reaches that it does not meet (ADR-0049 §6): the supplier's tax
// identifier, an item's description; never a refusal. The warnings are
// stored with the invoice, read back as posted, and empty before posting.
func TestDocumentaryWarnings(t *testing.T) {
	e := setup(t)
	e.withTaxSettings(t, config.TaxSettings{DocumentaryRequirements: tiers()})
	undescribed := strings.Replace(accountItem(e.expense, "500.00"), `"description": "Fees", `, "", 1)
	cases := []struct {
		name, gross, tax string
		item             string
		taxID            bool
		want             []string
	}{
		{"below the first tier", "99.96", "15.96", accountItem(e.expense, "84.00"), false, nil},
		{"at the first tier", "100.00", "15.97", accountItem(e.expense, "84.03"), false, []string{"supplier_tax_id_missing 100.00 EUR true false"}},
		{"both tiers", "595.00", "95.00", undescribed, false,
			[]string{"supplier_tax_id_missing 100.00 EUR true false", "item_description_missing 500.00 EUR true false"}},
		{"with a tax identifier", "595.00", "95.00", undescribed, true, []string{"item_description_missing 500.00 EUR true false"}},
		{"described, with a tax identifier", "595.00", "95.00", accountItem(e.expense, "500.00"), true, nil},
		{"no tax claimed", "500.00", "0.00", untaxedItem(e.expense, "500.00"), false, nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			supplier := e.Supplier
			if tc.taxID {
				supplier = e.Vendor
				pgtest.Exec(t, `INSERT INTO partner.tax_identifier (tenant_id, partner_id, scheme, value_cipher, value_fingerprint, value_last4, created_by, updated_by)
					VALUES ($1, $2, 'DE_VAT', '\x00', repeat('a', 64), '6789', $3, $3) ON CONFLICT DO NOTHING`, e.Tenant.ID, e.Vendor, e.Tenant.Human.ID)
			}
			inv, _ := e.in(t).Must(201, "POST", "/v1/supplier-invoices", e.invoice(supplier, fmt.Sprintf("DOC-%d", i), tc.gross, tc.tax, tc.item), "")
			if got := warnings(inv); len(got) != 0 {
				t.Errorf("parked: got %v, want none", got)
			}
			posted := e.in(t).act(t, 200, id(inv), "post", `{"posting_date": "2026-03-21"}`)
			if got := warnings(posted); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("posted: got %v, want %v", got, tc.want)
			}
			read, _ := e.in(t).Must(200, "GET", "/v1/supplier-invoices/"+id(inv), "", "")
			if got := warnings(read); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("read back: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDocumentaryWarningAbove: a tier stated as "above" an amount (the
// UAE's AED 10,000, pending sign-off) leaves out a total equal to it, and
// the warning says what is missing, from which total, under which law.
func TestDocumentaryWarningAbove(t *testing.T) {
	e := setup(t)
	e.withTaxSettings(t, config.TaxSettings{DocumentaryRequirements: []config.DocumentaryRequirement{{Above: "100", Requires: []string{config.RequiresSupplierTaxID},
		Source: "UAE Executive Regulation Art. 59", PendingSignOff: true}}})
	at, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "AT", "100.00", "15.97", accountItem(e.expense, "84.03")), "")
	if got := warnings(e.act(t, 200, id(at), "post", `{"posting_date": "2026-03-21"}`)); len(got) != 0 {
		t.Errorf("a total equal to the threshold: got %v, want none", got)
	}
	above, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "ABOVE", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	posted := e.act(t, 200, id(above), "post", `{"posting_date": "2026-03-21"}`)
	if got, want := warnings(posted), []string{"supplier_tax_id_missing 100.00 EUR false true"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("above the threshold: got %v, want %v", got, want)
	}
	msg := posted["documentary_warnings"].([]any)[0].(map[string]any)["message"]
	if want := "The supplier has no tax identifier; its registration number must be on the document for the input tax of a document above 100.00 EUR " +
		"to be recovered (UAE Executive Regulation Art. 59)."; msg != want {
		t.Errorf("message: got %v, want %s", msg, want)
	}
}

// TestTaxSettingsChangeTheResult: the rate type and rounding a company's
// tax settings name (from its country pack) change what an invoice posts
// (ADR-0049 §8): its tax converted at that type's rate of the tax date,
// none recorded refused with 422 ledger.no_exchange_rate; ten lines of
// 0.07 at 7% are 0.05 of tax rounded per document, nothing per line.
func TestTaxSettingsChangeTheResult(t *testing.T) {
	e := setup(t)
	inputCodes(t, e)
	e.GRIR(t) // the fx_gain and fx_loss accounts
	usd := func(ref string) map[string]any {
		body := strings.ReplaceAll(e.invoice(e.Supplier, ref, "119.00", "19.00", accountItem(e.expense, "100.00")), `"EUR"`, `"USD"`)
		inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", body, "")
		return inv
	}
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'average', 'USD', 'EUR', '2026-01-01', 0.9, 'ECB', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	noRate := usd("NO-RATE")
	e.withTaxSettings(t, config.TaxSettings{RateType: "bank_selling"})
	e.Problem(422, "ledger.no_exchange_rate", "POST", "/v1/supplier-invoices/"+id(noRate)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(noRate)))
	pgtest.Exec(t, `INSERT INTO ledger.exchange_rate (tenant_id, id, rate_type, from_currency, to_currency, valid_from, rate, source, created_by)
		VALUES ($1, gen_random_uuid(), 'bank_selling', 'USD', 'EUR', '2026-01-01', 0.8, 'Bank', $2)`, e.Tenant.ID, e.Tenant.Human.ID)
	entry := e.postedEntry(t, usd("SELLING"))
	var items string
	pgtest.Scan(t, `SELECT string_agg(base_lc || '/' || tax_lc, '; ') FROM ledger.tax_item WHERE journal_entry_id = $1`, []any{entry}, &items)
	if items != "8000/1520" {
		t.Errorf("items' local base and tax: got %s, want 8000/1520 at the bank selling rate 0.8, not the average 0.9", items)
	}
	lines := make([]string, 10)
	for i := range lines {
		lines[i] = coded(e, "0.07", "V7")
	}
	perDocument, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "DOC-ROUND", "0.75", "0.05", lines...), "")
	e.postedEntry(t, perDocument)
	e.withTaxSettings(t, config.TaxSettings{Rounding: config.TaxRoundLine})
	perLine, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "LINE-ROUND", "0.75", "0.05", lines...), "")
	e.Problem(422, "payables.tax_mismatch", "POST", "/v1/supplier-invoices/"+id(perLine)+":post", `{"posting_date": "2026-03-21"}`, e.tag(t, id(perLine)))
	untaxed, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "LINE-NONE", "0.70", "0.00", lines...), "")
	e.postedEntry(t, untaxed)
}

// TestDocumentaryDatabaseFailures: every database read the documentary
// check makes at posting, at a rate type of its own, reports its failure.
func TestDocumentaryDatabaseFailures(t *testing.T) {
	e := setup(t)
	e.withTaxSettings(t, config.TaxSettings{RateType: "bank_selling", DocumentaryRequirements: tiers()})
	inv, _ := e.Must(201, "POST", "/v1/supplier-invoices", e.invoice(e.Supplier, "FAIL-DOC", "119.00", "19.00", accountItem(e.expense, "100.00")), "")
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "post with tiers", func(ctx context.Context) error {
		_, err := endpoints(nil).PostInvoice(ctx, uuid.MustParse(id(inv)), func(int32) error { return nil }, nil)
		return err
	})
}
