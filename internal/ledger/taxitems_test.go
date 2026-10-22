package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// taxItemsOf renders an entry's tax items, each as code/component tax
// date base tax recoverable (local base, tax and recoverable) at the rate.
func taxItemsOf(t *testing.T, e env, entry uuid.UUID) string {
	t.Helper()
	var items []ledger.TaxItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		items, err = ledger.TaxItems{}.Of(ctx, entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return renderTaxItems(items)
}

func renderTaxItems(items []ledger.TaxItem) string {
	var out []string
	for _, i := range items {
		out = append(out, fmt.Sprintf("%s/%s %s %d %d %d (%d %d %d) at %s", i.Code, i.Component, i.TaxDate.Format("2006-01-02"), i.Base, i.Tax,
			i.Recoverable, i.BaseLC, i.TaxLC, i.RecoverableLC, i.RatePct))
	}
	return strings.Join(out, "; ")
}

// taxedEntry is a manual entry body of company 2000: 100.00 expense on V1
// with its 19.00 tax, 50.00 on V0, paid from the bank; fields are added
// to the header.
func taxedEntry(t *testing.T, e env, day, fields string) string {
	t.Helper()
	line := func(account, amount, code string) string {
		c := ""
		if code != "" {
			c = fmt.Sprintf(`, "tax_code": %q`, code)
		}
		return fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, e.account(t, "2000", account), amount, c)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, %s"currency": "EUR", "lines": [%s, %s, %s, %s]}`, e.de, day, day,
		fields, line("520000", "100.00", "V1"), line("140000", "19.00", "V1"), line("520000", "50.00", "V0"), line("100000", "-169.00", ""))
}

// TestTaxItems: an entry with tax codes writes one item per code and
// component (ADR-0049 §5, T61c): the base its other lines carry, the tax
// its lines on the component's account carry, the recoverable part and the
// rate on the tax date, which defaults to the posting date and decides the
// codes' validity; a reversal writes the original's items negated, at
// their tax date; the items are read per entry and per tax date.
func TestTaxItems(t *testing.T) {
	e := setup(t)
	je, _ := e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-10", `"tax_date": "2026-03-01", `), "")
	id := uuid.MustParse(apitest.String(t, je, "id"))
	if je["tax_date"] != "2026-03-01" {
		t.Errorf("entry's tax date: got %v, want 2026-03-01", je["tax_date"])
	}
	want := "V0/tax 2026-03-01 5000 0 0 (5000 0 0) at 0; V1/tax 2026-03-01 10000 1900 1900 (10000 1900 1900) at 19"
	if got := taxItemsOf(t, e, id); got != want {
		t.Errorf("items:\ngot  %s\nwant %s", got, want)
	}
	t.Run("the posting date by default", func(t *testing.T) {
		je, _ := e.in(t).Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-12", ""), "")
		if je["tax_date"] != "2026-03-12" {
			t.Errorf("entry's tax date: got %v, want the posting date 2026-03-12", je["tax_date"])
		}
		if got := taxItemsOf(t, e, uuid.MustParse(apitest.String(t, je, "id"))); !strings.Contains(got, "V1/tax 2026-03-12 10000 1900") {
			t.Errorf("items: got %s, want V1's at 2026-03-12", got)
		}
	})
	t.Run("codes valid on the tax date", func(t *testing.T) {
		e.in(t).Problem(422, "ledger.unknown_tax_code", "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-12", `"tax_date": "2020-01-01", `), "")
	})
	t.Run("a reversal negates them", func(t *testing.T) {
		_, tag := e.in(t).Must(200, "GET", "/v1/journal-entries/"+id.String(), "", "")
		rev, _ := e.in(t).Must(200, "POST", "/v1/journal-entries/"+id.String()+":reverse", `{"posting_date": "2026-04-05"}`, tag)
		if rev["tax_date"] != "2026-03-01" {
			t.Errorf("reversal's tax date: got %v, want the original's 2026-03-01", rev["tax_date"])
		}
		if got, want := taxItemsOf(t, e, uuid.MustParse(apitest.String(t, rev, "id"))), "V0/tax 2026-03-01 -5000 0 0 (-5000 0 0) at 0; V1/tax 2026-03-01 -10000 -1900 -1900 (-10000 -1900 -1900) at 19"; got != want {
			t.Errorf("reversal's items:\ngot  %s\nwant %s", got, want)
		}
	})
	t.Run("codes the ledger cannot itemize", func(t *testing.T) {
		rates := []config.TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}
		fixture.TaxCodes(t, e.ten, document(t),
			config.TaxCode{Code: "P7", Description: "BC PST", Components: []config.TaxComponent{
				{Key: "pst_bc", AccountKey: config.KeyNonDeductible, Rates: rates}}},
			config.TaxCode{Code: "H2", Description: "GST + QST", Components: []config.TaxComponent{
				{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: rates}, {Key: "qst", AccountKey: config.KeyInput, Account: "140000", Rates: rates}}})
		body := func(code string) string {
			return strings.ReplaceAll(strings.ReplaceAll(taxedEntry(t, e, "2026-03-12", ""), `"V1"`, `"`+code+`"`), `"V0"`, `"`+code+`"`)
		}
		e.in(t).Problem(422, "ledger.tax_code_components", "POST", "/v1/journal-entries", body("H2"), "")
		// A code posting to no account has no item until T61d; its entry
		// and its reversal post.
		je, tag := e.in(t).Must(201, "POST", "/v1/journal-entries", body("P7"), "")
		if got := taxItemsOf(t, e, uuid.MustParse(apitest.String(t, je, "id"))); got != "" {
			t.Errorf("items: got %s, want none", got)
		}
		e.in(t).Must(200, "POST", "/v1/journal-entries/"+apitest.String(t, je, "id")+":reverse", "", tag)
	})
	t.Run("by tax date", func(t *testing.T) {
		var items []ledger.TaxItem
		if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			items, err = ledger.TaxItems{}.Dated(ctx, e.de, date("2026-03-01"), date("2026-03-02"))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(items) != 4 {
			t.Errorf("items of 2026-03-01: got %d (%s), want the entry's two and its reversal's", len(items), renderTaxItems(items))
		}
	})
	t.Run("of what the caller may see", func(t *testing.T) {
		// The agent acts in no legal entity.
		err := apitest.InRequest(t, e.ten, e.ten.Agent, func(ctx context.Context) error {
			_, err := ledger.TaxItems{}.Dated(ctx, e.de, date("2026-03-01"), date("2026-04-01"))
			return err
		})
		pgtest.WantErr(t, err, "request.reference_not_found")
		err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := ledger.TaxItems{}.Of(ctx, uuid.New())
			return err
		})
		pgtest.WantErr(t, err, "request.not_found")
	})
}

// TestTaxItemsOfPostingPaths: a down payment's net is its code's base; the
// transfer that clears it takes its items back negated at the transfer's
// tax date; a cash discount's tax correction writes items of its items'
// codes at their rates, base and tax shares of the discount, whose local
// amounts add up to the lines'.
func TestTaxItemsOfPostingPaths(t *testing.T) {
	e := setup(t)
	downPayments(t, e)
	v1 := "V1"
	pay := ledger.DownPaymentPayment{LegalEntityID: e.de, SupplierID: e.supplier, BankAccountID: settleAccount(t, e, "2000", "100000"), DocumentType: "SA",
		PostingDate: date("2026-03-02"), Currency: "EUR", Amount: 11900, TaxCode: &v1, Tax: 1900, SourceDocumentID: uuid.New()}
	var paid ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		paid, err = postings().PayDownPayment(ctx, pay)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := taxItemsOf(t, e, paid.ID), "V1/tax 2026-03-02 10000 1900 1900 (10000 1900 1900) at 19"; got != want {
		t.Errorf("down payment's items:\ngot  %s\nwant %s", got, want)
	}
	var transfer ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		transfer, err = postings().ClearDownPayments(ctx, ledger.DownPaymentClearing{LegalEntityID: e.de, SupplierID: e.supplier,
			APAccountID: settleAccount(t, e, "2000", "211000"), DocumentType: "SA", PostingDate: date("2026-03-20"), Currency: "EUR",
			Items: []ledger.ItemRef{{FiscalYear: paid.FiscalYear, EntryID: paid.ID, LineNo: 1}}, SourceContext: "payables", SourceDocumentID: uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := taxItemsOf(t, e, transfer.ID), "V1/tax 2026-03-20 -10000 -1900 -1900 (-10000 -1900 -1900) at 19"; got != want {
		t.Errorf("transfer's items:\ngot  %s\nwant %s", got, want)
	}

	cash := settleAccount(t, e, "2000", "400000")
	determine(t, e, e.de, "cash_discount", cash)
	item := taxedPost(t, e, "TAXED-ITEM", 10000, map[string]money.Minor{"V1": 1900})
	var settled ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		in := settlementOf(t, e, e.de, "2000", "EUR", "2026-03-25", item)
		in.Discounts = []money.Minor{238}
		settled, err = postings().Settle(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// taxedPost puts no code on the expense: the base is the untaxed rest.
	if got, want := taxItemsOf(t, e, settled.ID), "V1/tax 2026-03-25 0 -38 -38 (0 -38 -38) at 19"; got != want {
		t.Errorf("settlement's items:\ngot  %s\nwant %s", got, want)
	}
}

// TestGivenTaxItemUnknown: a posting giving an item of a component
// without a rate on the tax date is refused, not posted without it.
func TestGivenTaxItemUnknown(t *testing.T) {
	e := setup(t)
	v1 := "V1"
	err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
			PostingDate: date("2026-03-01"), Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 100, TaxCode: &v1},
				{AccountID: settleAccount(t, e, "2000", "140000"), Amount: 19, TaxCode: &v1},
				{AccountID: settleAccount(t, e, "2000", "100000"), Amount: -119}},
			TaxItems: []ledger.PostingTaxItem{{Code: "V1", Component: "tax", Base: 100, Tax: 19, Recoverable: 19, BaseLC: 100, TaxLC: 19, RecoverableLC: 19},
				{Code: "V1", Component: "missing", Base: 100}}})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "1 of 2 tax items") {
		t.Errorf("got %v, want the items refused", err)
	}
}

// TestTaxItemsOfDiscountedBases: a discount on an entry whose expense
// carries its code shares out the net to the code's base on the cash
// discount account, so the settlement's items report base and tax; in a
// foreign currency their local amounts are the converted lines'.
func TestTaxItemsOfDiscountedBases(t *testing.T) {
	e := setup(t)
	cash := settleAccount(t, e, "2000", "400000")
	determine(t, e, e.de, "cash_discount", cash)
	settleRate(t, e, "USD", "2026-01-01", "0.9")
	v1 := "V1"
	var entry ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
			PostingDate: date("2026-03-01"), Currency: "USD", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
				{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 10000, TaxCode: &v1},
				{AccountID: settleAccount(t, e, "2000", "140000"), Amount: 1900, TaxCode: &v1},
				{AccountID: settleAccount(t, e, "2000", "211000"), Amount: -11900, PartnerID: &e.supplier}}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var settled ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		in := settlementOf(t, e, e.de, "2000", "USD", "2026-03-25", ledger.ItemRef{FiscalYear: entry.FiscalYear, EntryID: entry.ID, LineNo: 3})
		in.Discounts = []money.Minor{238}
		settled, err = postings().Settle(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := taxItemsOf(t, e, settled.ID), "V1/tax 2026-03-25 -200 -38 -38 (-180 -34 -34) at 19"; got != want {
		t.Errorf("settlement's items:\ngot  %s\nwant %s", got, want)
	}
}

// TestTaxItemFailures fails each statement of posting and reversing a
// taxed entry and of reading items, and calls the reads outside a unit of
// work.
func TestTaxItemFailures(t *testing.T) {
	e := setup(t)
	je, _ := e.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, e, "2026-03-10", ""), "")
	id := uuid.MustParse(apitest.String(t, je, "id"))
	calls := map[string]func(ctx context.Context) error{
		"post a taxed entry": func(ctx context.Context) error {
			v1 := "V1"
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-01"), Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 100, TaxCode: &v1},
					{AccountID: settleAccount(t, e, "2000", "140000"), Amount: 19, TaxCode: &v1},
					{AccountID: settleAccount(t, e, "2000", "100000"), Amount: -119}}})
			return err
		},
		"reverse a taxed entry": func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, id, nil, nil)
			return err
		},
		"items of an entry": func(ctx context.Context) error {
			_, err := ledger.TaxItems{}.Of(ctx, id)
			return err
		},
		"items by tax date": func(ctx context.Context) error {
			_, err := ledger.TaxItems{}.Dated(ctx, e.de, date("2026-01-01"), date("2027-01-01"))
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
}

// TestNonRecoverable: input tax not recovered is what the recoverable part
// leaves; output tax is owed, none of it recovered or not.
func TestNonRecoverable(t *testing.T) {
	for _, c := range []struct {
		item   ledger.TaxItem
		tc, lc money.Minor
	}{
		{ledger.TaxItem{AccountKey: "input", Tax: 100, Recoverable: 50, TaxLC: 90, RecoverableLC: 45}, 50, 45},
		{ledger.TaxItem{AccountKey: "output", Tax: -100, TaxLC: -90}, 0, 0},
	} {
		t.Run(c.item.AccountKey, func(t *testing.T) {
			if tc, lc := c.item.NonRecoverable(); tc != c.tc || lc != c.lc {
				t.Errorf("non-recoverable: got %d %d, want %d %d", tc, lc, c.tc, c.lc)
			}
		})
	}
}

// groupItems counts the tax items of a posting group's entries by ledger
// code, as code:count.
func (p parallelEnv) groupItems(t *testing.T, first string) string {
	t.Helper()
	var got string
	pgtest.Scan(t, `SELECT string_agg(l.code || ':' || (SELECT count(*) FROM ledger.tax_item i WHERE i.tenant_id = e.tenant_id AND i.fiscal_year = e.fiscal_year
			AND i.journal_entry_id = e.id), ',' ORDER BY l.code)
		FROM ledger.journal_entry e JOIN ledger.ledger l ON l.id = e.ledger_id WHERE e.tenant_id = $1 AND e.posting_group_id = $2`, []any{p.ten.ID, first}, &got)
	return got
}

// TestTaxItemsInParallelLedgers: in a company keeping 2L, a posting's tax
// items are its leading ledger's entry's alone (ADR-0045, ADR-0049 §5):
// returns are statutory and read the leading ledger, so its copy, which
// carries the same tax lines, has none; so for a reversal, a cash
// discount's correction, a down payment and its transfer; and a posting
// leaving out 0L may carry no tax code (422 ledger.tax_code_off_leading).
func TestTaxItemsInParallelLedgers(t *testing.T) {
	p := setupParallel(t)
	je, tag := p.Must(201, "POST", "/v1/journal-entries", taxedEntry(t, p.env, "2026-03-10", ""), "")
	id := apitest.String(t, je, "id")
	if got := p.groupItems(t, id); got != "0L:2,2L:0" {
		t.Errorf("posting group's items: got %s, want 0L's two alone", got)
	}
	rev, _ := p.in(t).Must(200, "POST", "/v1/journal-entries/"+id+":reverse", "", tag)
	if got := p.groupItems(t, apitest.String(t, rev, "id")); got != "0L:2,2L:0" {
		t.Errorf("reversal's items: got %s, want 0L's two alone", got)
	}
	if got := taxItemsOf(t, p.env, uuid.MustParse(apitest.String(t, rev, "id"))); !strings.Contains(got, "V1/tax 2026-03-10 -10000 -1900") {
		t.Errorf("reversal's items: got %s, want V1's negated", got)
	}
	// Tax codes only on postings the returns see (product owner,
	// 2026-10-21): not in 2L alone, by ledger or by group; in a group with
	// 0L, or 0L alone, they post.
	for name, target := range map[string]string{"2L": `"ledger_id": "` + p.local + `"`, "group LOC": `"ledger_group": "LOC"`} {
		t.Run(name, func(t *testing.T) {
			p.in(t).Problem(422, "ledger.tax_code_off_leading", "POST", "/v1/journal-entries", with(taxedEntry(t, p.env, "2026-03-11", ""), target), "")
		})
	}
	for name, c := range map[string]struct{ target, items string }{
		"group ALL": {`"ledger_group": "ALL"`, "0L:2,2L:0"},
		"0L":        {`"ledger_id": "` + p.leading + `"`, "0L:2"},
	} {
		t.Run(name, func(t *testing.T) {
			je, _ := p.in(t).Must(201, "POST", "/v1/journal-entries", with(taxedEntry(t, p.env, "2026-03-11", ""), c.target), "")
			if got := p.groupItems(t, apitest.String(t, je, "id")); got != c.items {
				t.Errorf("items: got %s, want %s", got, c.items)
			}
		})
	}

	determine(t, p.env, p.de, "cash_discount", settleAccount(t, p.env, "2000", "400000"))
	item := taxedPost(t, p.env, "PARALLEL-ITEM", 10000, map[string]money.Minor{"V1": 1900})
	var settled ledger.Entry
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) (err error) {
		in := settlementOf(t, p.env, p.de, "2000", "EUR", "2026-03-25", item)
		in.Discounts = []money.Minor{238}
		settled, err = postings().Settle(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.groupItems(t, settled.ID.String()); got != "0L:1,2L:0" {
		t.Errorf("settlement's items: got %s, want 0L's one alone", got)
	}

	doc := parallel(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	v1 := "V1"
	var paid, transfer ledger.Entry
	if err := apitest.Committed(t, p.ten, p.ten.Human, func(ctx context.Context) (err error) {
		paid, err = postings().PayDownPayment(ctx, ledger.DownPaymentPayment{LegalEntityID: p.de, SupplierID: p.supplier,
			BankAccountID: settleAccount(t, p.env, "2000", "100000"), DocumentType: "SA", PostingDate: date("2026-03-02"), Currency: "EUR", Amount: 11900,
			TaxCode: &v1, Tax: 1900, SourceDocumentID: uuid.New()})
		if err != nil {
			return err
		}
		transfer, err = postings().ClearDownPayments(ctx, ledger.DownPaymentClearing{LegalEntityID: p.de, SupplierID: p.supplier,
			APAccountID: settleAccount(t, p.env, "2000", "211000"), DocumentType: "SA", PostingDate: date("2026-03-20"), Currency: "EUR",
			Items: []ledger.ItemRef{{FiscalYear: paid.FiscalYear, EntryID: paid.ID, LineNo: 1}}, SourceContext: "payables", SourceDocumentID: uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		entry       ledger.Entry
		items, want string
	}{
		"down payment": {paid, "0L:1,2L:0", "V1/tax 2026-03-02 10000 1900 1900 (10000 1900 1900) at 19"},
		"transfer":     {transfer, "0L:1,2L:0", "V1/tax 2026-03-20 -10000 -1900 -1900 (-10000 -1900 -1900) at 19"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := p.groupItems(t, c.entry.ID.String()); got != c.items {
				t.Errorf("items by ledger: got %s, want %s", got, c.items)
			}
			if got := taxItemsOf(t, p.env, c.entry.ID); got != c.want {
				t.Errorf("leading items:\ngot  %s\nwant %s", got, c.want)
			}
		})
	}
}
