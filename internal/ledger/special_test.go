package ledger_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// downPayments gives company 2000 a down payment account, 159000 (an asset
// vendor reconciliation account, ADR-0037), and returns the document.
func downPayments(t *testing.T, e env) *config.Config {
	t.Helper()
	doc := document(t)
	de := doc.Company("2000")
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	de.AccountDetermination.DownPayment = "159000"
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	return doc
}

// special is a line's special G/L indicator, or "" without one.
func special(l ledger.Line) string {
	if l.SpecialGL == nil {
		return ""
	}
	return *l.SpecialGL
}

// TestDownPaymentItems: a down payment posts as the supplier's special
// G/L item A on the down payment account (ADR-0037, T46a), which the
// payable items leave out and the open items and the entry show; its
// reversal is a special item too. A line on the down payment account
// without the indicator, or with it on another account, is refused, and
// the down payment account cannot change once activated. A manual
// clearing of the down payment is refused, and the down payment account is
// no partner's own reconciliation account.
func TestDownPaymentItems(t *testing.T) {
	e := setup(t)
	doc := downPayments(t, e)
	dp, bank, ap := settleAccount(t, e, "2000", "159000"), settleAccount(t, e, "2000", "100000"), settleAccount(t, e, "2000", "211000")
	a := "A"
	post := func(account uuid.UUID, special *string) (ledger.Entry, error) {
		var entry ledger.Entry
		err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			ref := "DP-1"
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"),
				Currency: "EUR", Reference: &ref, SourceContext: "payments", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
					{AccountID: account, Amount: money.Minor(5000), PartnerID: &e.supplier, SpecialGL: special}, {AccountID: bank, Amount: -5000}}})
			return err
		})
		return entry, err
	}
	entry, err := post(dp, &a)
	if err != nil {
		t.Fatal(err)
	}
	if got := special(entry.Lines[0]); got != "A" {
		t.Errorf("posted line's special G/L indicator: got %q, want A", got)
	}
	var payable []ledger.PayableItem
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		payable, err = ledger.OpenItems{}.Payable(ctx, e.de, "EUR", date("2026-12-31"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, it := range payable {
		if it.EntryID == entry.ID {
			t.Errorf("payable items: got the down payment %v, want it left out", it.ItemRef)
		}
	}
	// The migration's reconciliation reads the down payment account from the
	// chart and sums the down payments apart from the open payables (T72c).
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		chart, err := ledger.Accounts{}.Chart(ctx, e.de)
		if err != nil {
			return err
		}
		if !chart["159000"].DownPayment || chart["211000"].DownPayment {
			t.Errorf("down payment accounts in the chart: got 159000 %t and 211000 %t, want true and false", chart["159000"].DownPayment, chart["211000"].DownPayment)
		}
		down, err := ledger.Reports{}.OpenDownPayments(ctx, e.de, 2026)
		if err != nil {
			return err
		}
		open, err := ledger.Reports{}.OpenPayables(ctx, e.de, 2026)
		if got := down[e.supplier]; got != 5000 {
			t.Errorf("open down payments of the supplier: got %d, want 5000", got)
		}
		if got, ok := open[e.supplier]; ok {
			t.Errorf("open payables of the supplier: got %d, want none (the down payment is apart)", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.supplier.String(), "", "")
	items := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("open items: got %d, want the down payment", len(items))
	}
	if got := items[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("open item special G/L indicator: got %v, want A", got)
	}
	je, _ := e.Must(200, "GET", "/v1/journal-entries/"+entry.ID.String(), "", "")
	if got := je["lines"].([]any)[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("journal entry line special G/L indicator: got %v, want A", got)
	}
	posted := events(t, e.ten, "journal_entry_posted")
	if got := posted[len(posted)-1]["lines"].([]any)[0].(map[string]any)["special_gl"]; got != "A" {
		t.Errorf("JournalEntryPosted line special G/L indicator: got %v, want A", got)
	}
	// A down payment is not netted by hand either: it leaves its account
	// only by clearing against the invoice or by a reversal.
	code, m, _ := e.in(t).Do("POST", "/v1/clearings", clearing(e.de, "2026-03-31", ref{je, 1}, ref{je, 2}), "")
	if code != 409 {
		t.Errorf("clearing the down payment: got status %d, want 409", code)
	}
	if m["code"] != "ledger.special_gl_item" {
		t.Errorf("clearing the down payment: got code %v, want ledger.special_gl_item", m["code"])
	}
	var reversal ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		reversal, err = postings().Reverse(ctx, entry.ID, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("reversal of the down payment: %v", err)
	}
	if got := special(reversal.Lines[0]); got != "A" {
		t.Errorf("reversal line's special G/L indicator: got %q, want A", got)
	}
	for name, c := range map[string]struct {
		account uuid.UUID
		special *string
		code    string
	}{
		"the down payment account without the indicator": {dp, nil, "ledger.special_gl_required"},
		"the indicator on the payables account":          {ap, &a, "ledger.special_gl_account"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := post(c.account, c.special)
			pgtest.WantErr(t, err, c.code)
		})
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		ok, err := ledger.Accounts{}.IsReconciliationAccount(ctx, e.de, dp, "supplier")
		if ok {
			t.Error("down payment account as a supplier's reconciliation account: got true, want false")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: "journal_line_special_gl_account"})
	if p.Code != "ledger.special_gl_account" {
		t.Errorf("trigger journal_line_special_gl_account code: got %s, want ledger.special_gl_account", p.Code)
	}
	if p.Status != 422 {
		t.Errorf("trigger journal_line_special_gl_account status: got %d, want 422", p.Status)
	}
	changed := document(t) // the account stays in the chart, no longer the down payment account
	changed.Company("2000").ChartOfAccounts = append(changed.Company("2000").ChartOfAccounts,
		config.Account{Number: "159000", Name: "Down payments made", Type: config.Asset, VendorReconciliation: true})
	err = apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		fs, err := ledger.Setup{LegalEntities: tenancy.Directory{}}.Check(ctx, doc, changed)
		var paths []string
		for _, f := range fs {
			paths = append(paths, f.Path)
		}
		if !slices.Contains(paths, "companies[2000].account_determination.down_payment") {
			t.Errorf("removing the down payment account: got findings %v, want one on companies[2000].account_determination.down_payment", paths)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPayDownPayment: a down payment request's payment posts the down
// payment on the down payment account as special G/L item A, net of its
// input tax, the tax, and the bank (T46c); posting the same payment again
// returns its entry. A legal entity without a down payment account, or a
// tax code not valid on the posting date, is refused.
func TestPayDownPayment(t *testing.T) {
	e := setup(t)
	doc := downPayments(t, e)
	bank := settleAccount(t, e, "2000", "100000")
	v1 := "V1"
	in := ledger.DownPaymentPayment{LegalEntityID: e.de, SupplierID: e.supplier, BankAccountID: bank, DocumentType: "SA", PostingDate: date("2026-03-20"),
		Currency: "EUR", Amount: money.Minor(11900), TaxCode: &v1, Tax: money.Minor(1900), SourceDocumentID: uuid.New()}
	var first, again ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		first, err = postings().PayDownPayment(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		amount  money.Minor
		special string
	}{{10000, "A"}, {1900, ""}, {-11900, ""}} {
		t.Run(fmt.Sprintf("line %d", i+1), func(t *testing.T) {
			if got := first.Lines[i].TC; got != want.amount {
				t.Errorf("amount: got %v, want %v", got, want.amount)
			}
			if got := special(first.Lines[i]); got != want.special {
				t.Errorf("special G/L: got %q, want %q", got, want.special)
			}
		})
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		again, err = postings().PayDownPayment(ctx, in)
		if err != nil {
			return err
		}
		untaxed := in
		untaxed.SourceDocumentID, untaxed.Tax = uuid.New(), 0
		entry, err := postings().PayDownPayment(ctx, untaxed)
		if err != nil {
			return err
		}
		if len(entry.Lines) != 2 {
			t.Errorf("a payment without tax: got %d lines, want 2", len(entry.Lines))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Errorf("posting again: got entry %s, want %s", again.ID, first.ID)
	}
	// GST and a non-deductible PST: one tax line cannot post it (T61d).
	fixture.TaxCodes(t, e.ten, doc, config.TaxCode{Code: "V2", Description: "GST + PST", Components: []config.TaxComponent{
		{Key: "gst", AccountKey: config.KeyInput, Account: "140000", Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}},
		{Key: "pst", AccountKey: config.KeyNonDeductible, Rates: []config.TaxRate{{RatePercent: "7", ValidFrom: "2026-01-01"}}}}})
	unknown, parts := "ZZ", "V2"
	for name, c := range map[string]struct {
		mutate func(*ledger.DownPaymentPayment)
		code   string
	}{
		"no down payment account": {func(p *ledger.DownPaymentPayment) { p.LegalEntityID = e.us }, "ledger.no_down_payment_account"},
		"unknown tax code":        {func(p *ledger.DownPaymentPayment) { p.TaxCode = &unknown }, "ledger.unknown_tax_code"},
		"a code with components":  {func(p *ledger.DownPaymentPayment) { p.TaxCode = &parts }, "ledger.tax_code_components"},
		"tax of the whole amount": {func(p *ledger.DownPaymentPayment) { p.Tax = p.Amount }, "ledger.invalid_down_payment"},
	} {
		t.Run(name, func(t *testing.T) {
			p := in
			p.SourceDocumentID = uuid.New()
			c.mutate(&p)
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, err := postings().PayDownPayment(ctx, p)
				return err
			})
			pgtest.WantErr(t, err, c.code)
		})
	}
	call := func(ctx context.Context) error {
		p := in
		p.SourceDocumentID = uuid.New()
		_, err := postings().PayDownPayment(ctx, p)
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "pay down payment", call)
	apitest.EveryFailure(t, e.ten, e.ten.Human, "pay down payment again", func(ctx context.Context) error {
		_, err := postings().PayDownPayment(ctx, in)
		return err
	})
}

// TestClearDownPayments: clearing a down payment against an invoice posts
// the transfer: Cr the down payment (clearing it), Cr its input tax back,
// Dr the supplier's payables account with their total, an open item with
// the invoice's terms (T46d). Only open down payments of the supplier in
// the clearing's currency are cleared, and an entry whose tax two down
// payments share is refused.
func TestClearDownPayments(t *testing.T) {
	e := setup(t)
	downPayments(t, e)
	dp, bank, ap, tax := settleAccount(t, e, "2000", "159000"), settleAccount(t, e, "2000", "100000"), settleAccount(t, e, "2000", "211000"),
		settleAccount(t, e, "2000", "140000")
	v1, a := "V1", "A"
	paid := func(lines ...ledger.PostingLine) ledger.Entry {
		var entry ledger.Entry
		if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
			entry, err = postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"),
				Currency: "EUR", SourceContext: "payments", SourceDocumentID: uuid.New(), Lines: lines})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	taxed := paid(ledger.PostingLine{AccountID: dp, Amount: 10000, PartnerID: &e.supplier, SpecialGL: &a},
		ledger.PostingLine{AccountID: tax, Amount: 1900, TaxCode: &v1}, ledger.PostingLine{AccountID: bank, Amount: -11900})
	shared := paid(ledger.PostingLine{AccountID: dp, Amount: 100, PartnerID: &e.supplier, SpecialGL: &a},
		ledger.PostingLine{AccountID: dp, Amount: 200, PartnerID: &e.supplier, SpecialGL: &a},
		ledger.PostingLine{AccountID: tax, Amount: 57, TaxCode: &v1}, ledger.PostingLine{AccountID: bank, Amount: -357})
	ordinary := paid(ledger.PostingLine{AccountID: ap, Amount: 500, PartnerID: &e.supplier}, ledger.PostingLine{AccountID: bank, Amount: -500})
	in := ledger.DownPaymentClearing{LegalEntityID: e.de, SupplierID: e.supplier, APAccountID: ap, DocumentType: "SA", PostingDate: date("2026-03-20"),
		Currency: "EUR", Items: []ledger.ItemRef{{FiscalYear: taxed.FiscalYear, EntryID: taxed.ID, LineNo: 1}}, SourceContext: "payables", SourceDocumentID: uuid.New(),
		Terms: &ledger.ItemTerms{PaymentTermCode: "NT30", BaselineDate: date("2026-03-15"), NetDays: 30}}
	var transfer ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		transfer, err = postings().ClearDownPayments(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		account uuid.UUID
		amount  money.Minor
		special string
	}{{dp, -10000, "A"}, {tax, -1900, ""}, {ap, 11900, ""}} {
		t.Run(fmt.Sprintf("transfer line %d", i+1), func(t *testing.T) {
			l := transfer.Lines[i]
			if l.AccountID != want.account {
				t.Errorf("account: got %s, want %s", l.AccountID, want.account)
			}
			if l.TC != want.amount {
				t.Errorf("amount: got %v, want %v", l.TC, want.amount)
			}
			if got := special(l); got != want.special {
				t.Errorf("special G/L: got %q, want %q", got, want.special)
			}
		})
	}
	page, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+e.supplier.String(), "", "")
	for _, it := range page["items"].([]any) {
		m := it.(map[string]any)
		if m["journal_entry_id"] == taxed.ID.String() {
			t.Errorf("open items: got the cleared down payment %v", m)
		}
		if m["journal_entry_id"] == transfer.ID.String() && m["payment_terms"] == nil {
			t.Errorf("transfer's payables debit: got no payment terms, want the invoice's")
		}
	}
	// The supplier's down payments alone, which an invoice's clearing
	// chooses from (T73e4): the shared entry's two, not the ordinary items
	// nor the one cleared.
	page, _ = e.Must(200, "GET", "/v1/open-items?special_gl=A&partner_id="+e.supplier.String(), "", "")
	var downs []string
	for _, it := range page["items"].([]any) {
		m := it.(map[string]any)
		downs = append(downs, fmt.Sprintf("%v/%v %v", m["journal_entry_id"], m["line_no"], m["special_gl"]))
	}
	slices.Sort(downs) // the page orders by posting date, then by an id derived from the line
	if want := []string{shared.ID.String() + "/1 A", shared.ID.String() + "/2 A"}; !slices.Equal(downs, want) {
		t.Errorf("open items with special G/L A: got %v, want %v", downs, want)
	}
	for name, c := range map[string]struct {
		mutate func(*ledger.DownPaymentClearing)
		code   string
	}{
		"no items":        {func(c *ledger.DownPaymentClearing) { c.Items = nil }, "ledger.no_items"},
		"an item twice":   {func(c *ledger.DownPaymentClearing) { c.Items = append(c.Items, c.Items[0]) }, "ledger.duplicate_item"},
		"cleared already": {func(*ledger.DownPaymentClearing) {}, "ledger.item_already_cleared"},
		"an ordinary item": {func(c *ledger.DownPaymentClearing) {
			c.Items[0] = ledger.ItemRef{FiscalYear: ordinary.FiscalYear, EntryID: ordinary.ID, LineNo: 1}
		}, "ledger.not_a_down_payment"},
		"another supplier": {func(c *ledger.DownPaymentClearing) { c.SupplierID = uuid.New() }, "ledger.not_a_down_payment"},
		"not an open item": {func(c *ledger.DownPaymentClearing) {
			c.Items[0] = ledger.ItemRef{FiscalYear: taxed.FiscalYear, EntryID: taxed.ID, LineNo: 3}
		}, "ledger.not_an_open_item"},
		"another currency": {func(c *ledger.DownPaymentClearing) {
			c.Items[0], c.Currency = ledger.ItemRef{FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 1}, "USD"
		}, "ledger.mixed_items"},
		"tax of two payments": {func(c *ledger.DownPaymentClearing) {
			c.Items[0] = ledger.ItemRef{FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 1}
		}, "ledger.down_payment_tax_shared"},
	} {
		t.Run(name, func(t *testing.T) {
			c2 := in
			c2.Items = append([]ledger.ItemRef{}, in.Items...)
			c.mutate(&c2)
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, err := postings().ClearDownPayments(ctx, c2)
				return err
			})
			pgtest.WantErr(t, err, c.code)
		})
	}
	call := func(ctx context.Context) error {
		c2 := in
		c2.Items, c2.Terms = []ledger.ItemRef{{FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 1}, {FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 2}}, nil
		_, err := postings().ClearDownPayments(ctx, c2)
		return err
	}
	// A clearing is not dated before its down payments, posted on 1 March
	// (TestClearingDate has the other paths).
	t.Run("dated before the down payments", func(t *testing.T) {
		c2 := in
		c2.Items, c2.Terms = []ledger.ItemRef{{FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 1}, {FiscalYear: shared.FiscalYear, EntryID: shared.ID, LineNo: 2}}, nil
		dated := func(day string) error {
			c2.PostingDate = date(day)
			return apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				_, err := postings().ClearDownPayments(ctx, c2)
				return err
			})
		}
		refusedBefore(t, dated("2026-02-28"), "posting_date", "2026-03-01")
		if err := dated("2026-03-01"); err != nil {
			t.Errorf("dated on the down payments' posting date: got %v, want them cleared", err)
		}
	})
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "clear down payments", call)
}
