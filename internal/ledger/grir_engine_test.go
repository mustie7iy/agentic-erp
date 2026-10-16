package ledger_test

import (
	"context"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// grirEnv is company 2000 (EUR) valuing receipts through GR/IR, with the
// accounts its postings use.
type grirEnv struct {
	env
	grir, diff, expense, ap, fxGain, fxLoss uuid.UUID
}

// price is an order price: minor units per price unit.
type price struct {
	unit money.Minor
	per  int32
}

func grirSetup(t *testing.T) grirEnv {
	t.Helper()
	e := setup(t)
	doc := withGRIR(t)
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "480000", Name: "FX gains", Type: config.Revenue},
		config.Account{Number: "690000", Name: "FX losses", Type: config.Expense})
	de.AccountDetermination.FXGain, de.AccountDetermination.FXLoss = "480000", "690000"
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		return ledger.Setup{LegalEntities: tenancy.Directory{}}.Apply(ctx, doc)
	}); err != nil {
		t.Fatal(err)
	}
	id := func(number string) uuid.UUID { return uuid.MustParse(e.account(t, "2000", number)) }
	return grirEnv{env: e, grir: id("219000"), diff: id("590000"), expense: id("520000"), ap: id("211000"), fxGain: id("480000"), fxLoss: id("690000")}
}

// receiptPosting is a goods movement of qty (signed like the GR/IR line:
// negative received) of item 10 of the order on the expense account.
func (g grirEnv) receiptPosting(day, currency string, order uuid.UUID, qty string, p price) ledger.Posting {
	return ledger.Posting{LegalEntityID: g.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day), Currency: currency,
		SourceContext: "procurement", SourceDocumentID: uuid.New(),
		GRIR: []ledger.GRIRItem{{PurchaseOrderID: order, POItemNo: 10, Quantity: qty, Unit: "EA", UnitPrice: p.unit, PriceUnit: p.per, OffsetAccountID: &g.expense}}}
}

// invoicePosting invoices qty of item 10 of the order for net, owed to
// the supplier.
func (g grirEnv) invoicePosting(day, currency string, order uuid.UUID, qty string, p price, net money.Minor) ledger.Posting {
	return ledger.Posting{LegalEntityID: g.de, DocumentType: "SA", DocumentDate: date(day), PostingDate: date(day), Currency: currency,
		SourceContext: "payables", SourceDocumentID: uuid.New(),
		Lines: []ledger.PostingLine{{AccountID: g.ap, Amount: -net, PartnerID: &g.supplier}},
		GRIR:  []ledger.GRIRItem{{PurchaseOrderID: order, POItemNo: 10, Quantity: qty, Unit: "EA", UnitPrice: p.unit, PriceUnit: p.per, Invoiced: &net}}}
}

// post posts in a committed transaction.
func (g grirEnv) post(t *testing.T, in ledger.Posting) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) (err error) {
		entry, err = postings().Post(ctx, in)
		return err
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	return entry
}

// position is an order item's GR/IR position: net quantity and balance.
type position struct {
	qty        string
	tc, lc, gc int64
}

func (g grirEnv) position(t *testing.T, order uuid.UUID) position {
	t.Helper()
	var p position
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT coalesce(trim_scale(sum(quantity)), 0)::text, coalesce(sum(amount_tc), 0)::bigint,
		coalesce(sum(amount_lc), 0)::bigint, coalesce(sum(amount_gc), 0)::bigint FROM ledger.journal_line
		WHERE tenant_id = $1 AND purchase_order_id = $2 AND po_item_no = 10`, g.ten.ID, order).Scan(&p.qty, &p.tc, &p.lc, &p.gc); err != nil {
		t.Fatalf("read the position: %v", err)
	}
	return p
}

// on is the entry's lines on the account, as (TC, LC, GC) triples.
func on(entry ledger.Entry, account uuid.UUID) [][3]money.Minor {
	var out [][3]money.Minor
	for _, l := range entry.Lines {
		if l.AccountID == account {
			out = append(out, [3]money.Minor{l.TC, l.LC, l.GC})
		}
	}
	return out
}

// one is the single line of the entry on the account, or fails.
func one(t *testing.T, entry ledger.Entry, account uuid.UUID, what string) [3]money.Minor {
	t.Helper()
	lines := on(entry, account)
	if len(lines) != 1 {
		t.Fatalf("%s: got %d lines, want 1: %+v", what, len(lines), entry.Lines)
	}
	return lines[0]
}

// TestGRIRRounding: each goods movement moves the position to the order
// price of the net quantity, so rounding never adds up: three receipts of
// one at 10.00 per 3 credit 3.33, 3.34 and 3.33, and invoicing the three
// clears exactly 10.00. The item's account mirrors each GR/IR line.
func TestGRIRRounding(t *testing.T) {
	g := grirSetup(t)
	order, p := uuid.New(), price{1000, 3}
	for i, want := range []money.Minor{-333, -334, -333} {
		entry := g.post(t, g.receiptPosting("2026-03-02", "EUR", order, "-1", p))
		if got := one(t, entry, g.grir, "receipt")[0]; got != want {
			t.Errorf("receipt %d: GR/IR got %d, want %d", i+1, got, want)
		}
		grir := one(t, entry, g.grir, "receipt")
		if got, want := one(t, entry, g.expense, "receipt"), [3]money.Minor{-grir[0], -grir[1], -grir[2]}; got != want {
			t.Errorf("receipt %d: expense got %v, want %v, the GR/IR line negated", i+1, got, want)
		}
	}
	entry := g.post(t, g.invoicePosting("2026-03-03", "EUR", order, "3", p, 1000))
	if got := one(t, entry, g.grir, "invoice")[0]; got != 1000 {
		t.Errorf("invoice: GR/IR got %d, want 1000", got)
	}
	if got := on(entry, g.diff); len(got) != 0 {
		t.Errorf("invoice at the order price: got price difference lines %v, want none", got)
	}
	if got := g.position(t, order); got != (position{"0", 0, 0, 0}) {
		t.Errorf("position: got %+v, want zero", got)
	}
}

// TestGRIRPartialAndEarlyInvoices: partial receipts and invoices leave the
// position at the order price of what is received and not invoiced (or
// invoiced and not received); the invoice's price difference is a line of
// its own; an invoice before its receipt and a receipt beyond what was
// invoiced cross zero; equal quantities leave zero.
func TestGRIRPartialAndEarlyInvoices(t *testing.T) {
	g := grirSetup(t)
	order, p := uuid.New(), price{500, 1}
	steps := []struct {
		name     string
		posting  ledger.Posting
		grir     money.Minor
		diff     []money.Minor
		position position
	}{
		{"receive 6", g.receiptPosting("2026-03-02", "EUR", order, "-6", p), -3000, nil, position{"-6", -3000, -3000, -3000}},
		{"invoice 4 at 5.20", g.invoicePosting("2026-03-03", "EUR", order, "4", p, 2080), 2000, []money.Minor{80}, position{"-2", -1000, -1000, -1000}},
		{"receive 4", g.receiptPosting("2026-03-04", "EUR", order, "-4", p), -2000, nil, position{"-6", -3000, -3000, -3000}},
		{"invoice 6", g.invoicePosting("2026-03-05", "EUR", order, "6", p, 3000), 3000, nil, position{"0", 0, 0, 0}},
		{"invoice 2 before receipt", g.invoicePosting("2026-03-06", "EUR", order, "2", p, 1000), 1000, nil, position{"2", 1000, 1000, 1000}},
		{"receive 3", g.receiptPosting("2026-03-07", "EUR", order, "-3", p), -1500, nil, position{"-1", -500, -500, -500}},
		{"invoice 1 at 4.90", g.invoicePosting("2026-03-08", "EUR", order, "1", p, 490), 500, []money.Minor{-10}, position{"0", 0, 0, 0}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			entry := g.post(t, s.posting)
			if got := one(t, entry, g.grir, s.name); got != [3]money.Minor{s.grir, s.grir, s.grir} {
				t.Errorf("GR/IR line: got %v, want %d in every currency", got, s.grir)
			}
			var diffs []money.Minor
			for _, d := range on(entry, g.diff) {
				diffs = append(diffs, d[0])
			}
			if !slices.Equal(diffs, s.diff) {
				t.Errorf("price differences: got %v, want %v", diffs, s.diff)
			}
			if got := g.position(t, order); got != s.position {
				t.Errorf("position: got %+v, want %+v", got, s.position)
			}
		})
	}
}

// TestGRIRForeignCurrency: an order in USD of a company in EUR. A receipt
// is valued at its date's rate; an invoice clears the position at the
// local value it was received at, by quantity, and the difference to the
// invoice's rate is an exchange difference; an invoice beyond the receipts
// clears them and values the rest at its own rate; the posting that
// brings the quantity to zero leaves zero in every currency.
func TestGRIRForeignCurrency(t *testing.T) {
	g := grirSetup(t)
	settleRate(t, g.env, "USD", "2026-03-02", "0.9")
	settleRate(t, g.env, "USD", "2026-03-10", "0.92")
	settleRate(t, g.env, "USD", "2026-03-20", "0.95")
	order, p := uuid.New(), price{100, 1}
	steps := []struct {
		name     string
		posting  ledger.Posting
		grir     [3]money.Minor
		fxLoss   money.Minor
		position position
	}{
		{"receive 100 at 0.90", g.receiptPosting("2026-03-02", "USD", order, "-100", p), [3]money.Minor{-10000, -9000, -9000}, 0, position{"-100", -10000, -9000, -9000}},
		{"receive 100 at 0.92", g.receiptPosting("2026-03-10", "USD", order, "-100", p), [3]money.Minor{-10000, -9200, -9200}, 0, position{"-200", -20000, -18200, -18200}},
		{"invoice 50 at 0.95", g.invoicePosting("2026-03-20", "USD", order, "50", p, 5000), [3]money.Minor{5000, 4550, 4550}, 200, position{"-150", -15000, -13650, -13650}},
		{"invoice 200 at 0.95", g.invoicePosting("2026-03-20", "USD", order, "200", p, 20000), [3]money.Minor{20000, 18400, 18400}, 600, position{"50", 5000, 4750, 4750}},
		{"receive 50 at 0.95", g.receiptPosting("2026-03-20", "USD", order, "-50", p), [3]money.Minor{-5000, -4750, -4750}, 0, position{"0", 0, 0, 0}},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			entry := g.post(t, s.posting)
			if got := one(t, entry, g.grir, s.name); got != s.grir {
				t.Errorf("GR/IR line: got %v, want %v", got, s.grir)
			}
			var loss money.Minor
			for _, l := range on(entry, g.fxLoss) {
				loss += l[1]
			}
			if loss != s.fxLoss {
				t.Errorf("exchange loss: got %d, want %d (lines %+v)", loss, s.fxLoss, entry.Lines)
			}
			if got := g.position(t, order); got != s.position {
				t.Errorf("position: got %+v, want %+v", got, s.position)
			}
		})
	}
}

// TestGRIRReversalResidue: a reversal mirrors the invoice it reverses;
// where rounding leaves a balance on an item whose quantity it brings to
// zero, the reversal clears it against price differences. At 0.01 per 3,
// invoices of one debit 0.00 and then 0.01, and a receipt of one credits
// 0.01; reversing the second invoice leaves -0.01 on a zero quantity.
func TestGRIRReversalResidue(t *testing.T) {
	g := grirSetup(t)
	reverse := func(t *testing.T, entry ledger.Entry) ledger.Entry {
		t.Helper()
		var rev ledger.Entry
		if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) (err error) {
			rev, err = postings().Reverse(ctx, entry.ID, nil, nil)
			return err
		}); err != nil {
			t.Fatalf("reverse: %v", err)
		}
		return rev
	}
	t.Run("no residue", func(t *testing.T) {
		order := uuid.New()
		entry := g.post(t, g.invoicePosting("2026-03-02", "EUR", order, "2", price{1, 3}, 1))
		if rev := reverse(t, entry); len(rev.Lines) != len(entry.Lines) {
			t.Errorf("reversal: got %d lines, want the %d it mirrors", len(rev.Lines), len(entry.Lines))
		}
		if got := g.position(t, order); got != (position{"0", 0, 0, 0}) {
			t.Errorf("position: got %+v, want zero", got)
		}
	})
	t.Run("quantity left", func(t *testing.T) {
		order, p := uuid.New(), price{1000, 3}
		g.post(t, g.receiptPosting("2026-03-02", "EUR", order, "-3", p))
		entry := g.post(t, g.invoicePosting("2026-03-03", "EUR", order, "2", p, 667))
		if rev := reverse(t, entry); len(rev.Lines) != len(entry.Lines) {
			t.Errorf("reversal: got %d lines, want the %d it mirrors", len(rev.Lines), len(entry.Lines))
		}
		if got := g.position(t, order); got != (position{"-3", -1000, -1000, -1000}) {
			t.Errorf("position: got %+v, want -3 at -10.00", got)
		}
	})
	t.Run("no GR/IR line", func(t *testing.T) {
		manual := g.post(t, ledger.Posting{LegalEntityID: g.de, DocumentType: "SA", DocumentDate: date("2026-03-02"), PostingDate: date("2026-03-02"),
			Currency: "EUR", SourceContext: "integration", SourceDocumentID: uuid.New(),
			Lines: []ledger.PostingLine{{AccountID: g.expense, Amount: 100}, {AccountID: g.diff, Amount: -100}}})
		if rev := reverse(t, manual); len(rev.Lines) != 2 {
			t.Errorf("reversal: got %d lines, want 2", len(rev.Lines))
		}
	})
	t.Run("residue", func(t *testing.T) {
		order, p := uuid.New(), price{1, 3}
		g.post(t, g.invoicePosting("2026-03-02", "EUR", order, "1", p, 1))
		second := g.post(t, g.invoicePosting("2026-03-03", "EUR", order, "1", p, 1))
		g.post(t, g.receiptPosting("2026-03-04", "EUR", order, "-1", p))
		rev := reverse(t, second)
		n := len(second.Lines)
		if len(rev.Lines) != n+2 {
			t.Fatalf("reversal: got %d lines, want %d and an adjustment pair: %+v", len(rev.Lines), n, rev.Lines)
		}
		adj, against := rev.Lines[n], rev.Lines[n+1]
		zero := "0"
		unit, item := "EA", int32(10)
		want := ledger.Line{LineNo: adj.LineNo, AccountID: g.grir, TC: 1, LC: 1, GC: 1, Quantity: &zero, Unit: &unit, PurchaseOrderID: &order, POItemNo: &item}
		if !reflect.DeepEqual(adj, want) {
			t.Errorf("adjustment: got %+v, want 0.01 on GR/IR for the order item, quantity 0", adj)
		}
		if got := [3]any{against.AccountID, against.TC, against.LC}; got != [3]any{g.diff, money.Minor(-1), money.Minor(-1)} {
			t.Errorf("against: got %v, want -0.01 on price differences", got)
		}
		if got := g.position(t, order); got != (position{"0", 0, 0, 0}) {
			t.Errorf("position: got %+v, want zero", got)
		}
	})
}

// TestGRIRPositionIsOnGRIR: a line that names an order item on another
// account than GR/IR (a migration run may load one) is no part of its
// position.
func TestGRIRPositionIsOnGRIR(t *testing.T) {
	g := grirSetup(t)
	order := uuid.New()
	g.insertWithOrderItem(t, order) // 4 EA, 10.00 on expense 520000
	entry := g.post(t, g.receiptPosting("2026-03-11", "EUR", order, "-1", price{500, 1}))
	if got := one(t, entry, g.grir, "receipt")[0]; got != -500 {
		t.Errorf("GR/IR line: got %d, want -500", got)
	}
}

// TestGRIRSeveralOrders: one receipt of items of two orders moves each
// position, and its reversal through the owner's port moves both back.
func TestGRIRSeveralOrders(t *testing.T) {
	g := grirSetup(t)
	first, second := uuid.New(), uuid.New()
	in := g.receiptPosting("2026-03-02", "EUR", second, "-2", price{500, 1})
	in.GRIR = append(in.GRIR, g.receiptPosting("2026-03-02", "EUR", first, "-1", price{700, 1}).GRIR...)
	entry := g.post(t, in)
	if got := g.position(t, second); got != (position{"-2", -1000, -1000, -1000}) {
		t.Errorf("second order: got %+v, want -2 at -10.00", got)
	}
	if got := g.position(t, first); got != (position{"-1", -700, -700, -700}) {
		t.Errorf("first order: got %+v, want -1 at -7.00", got)
	}
	if err := apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) error {
		_, err := postings().Reverse(ctx, entry.ID, nil, nil)
		return err
	}); err != nil {
		t.Fatalf("reverse: %v", err)
	}
	for _, order := range []uuid.UUID{first, second} {
		if got := g.position(t, order); got != (position{"0", 0, 0, 0}) {
			t.Errorf("order %s after the reversal: got %+v, want zero", order, got)
		}
	}
}

// TestGRIRConcurrentReceipts: receipts of one order item posted at once
// take turns on its position, so each moves it from where the last left
// it: six receipts of one at 10.00 per 3 credit exactly 20.00.
func TestGRIRConcurrentReceipts(t *testing.T) {
	g := grirSetup(t)
	order := uuid.New()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2 {
				errs <- apitest.Committed(t, g.ten, g.ten.Human, func(ctx context.Context) error {
					_, err := postings().Post(ctx, g.receiptPosting("2026-03-02", "EUR", order, "-1", price{1000, 3}))
					return err
				})
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("receipt: %v", err)
		}
	}
	if got := g.position(t, order); got != (position{"-6", -2000, -2000, -2000}) {
		t.Errorf("position: got %+v, want -6 at -20.00", got)
	}
}

// TestGRIRRefusals: a company without a GR/IR account, an order item
// posted in another currency, an unbalanced invoice, the checks every
// posting passes (tax codes, partners, amounts in range) and a malformed
// item are refused.
func TestGRIRRefusals(t *testing.T) {
	g := grirSetup(t)
	settleRate(t, g.env, "USD", "2026-03-02", "0.9")
	order := uuid.New()
	g.post(t, g.receiptPosting("2026-03-02", "EUR", order, "-1", price{100, 1}))
	noGRIR := g.receiptPosting("2026-03-02", "USD", uuid.New(), "-1", price{100, 1})
	noGRIR.LegalEntityID = g.us
	unbalanced := g.invoicePosting("2026-03-02", "EUR", uuid.New(), "1", price{100, 1}, 100)
	unbalanced.Lines[0].Amount = -90
	taxed := g.invoicePosting("2026-03-02", "EUR", uuid.New(), "1", price{100, 1}, 100)
	unknown := "ZZ"
	taxed.Lines[0].TaxCode = &unknown
	noPartner := g.invoicePosting("2026-03-02", "EUR", uuid.New(), "1", price{100, 1}, 100)
	noPartner.Lines[0].PartnerID = nil
	for _, c := range []struct {
		name    string
		posting ledger.Posting
		code    string
	}{
		{"no GR/IR account", noGRIR, "ledger.no_gr_ir_account"},
		{"another currency", g.receiptPosting("2026-03-02", "USD", order, "-1", price{100, 1}), "ledger.gr_ir_currency_mismatch"},
		{"unbalanced", unbalanced, "ledger.unbalanced"},
		{"unknown tax code", taxed, "ledger.unknown_tax_code"},
		{"supplier line without its supplier", noPartner, "ledger.partner_required"},
		{"too large", g.receiptPosting("2026-03-02", "EUR", uuid.New(), "-10", price{math.MaxInt64, 1}), "ledger.amount_too_large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := apitest.InRequest(t, g.ten, g.ten.Human, func(ctx context.Context) error {
				_, err := postings().Post(ctx, c.posting)
				return err
			})
			if got := problem.From(err).Code; got != c.code {
				t.Errorf("got %v (%s), want %s", err, got, c.code)
			}
		})
	}
	noAccount := g.receiptPosting("2026-03-02", "EUR", uuid.New(), "-1", price{100, 1})
	noAccount.GRIR[0].OffsetAccountID = nil
	badQuantity := g.receiptPosting("2026-03-02", "EUR", uuid.New(), "-1,5", price{100, 1})
	for name, bad := range map[string]ledger.Posting{"neither account nor invoiced amount": noAccount, "malformed quantity": badQuantity} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, g.ten, g.ten.Human, func(ctx context.Context) error {
				_, err := postings().Post(ctx, bad)
				return err
			})
			if problem.From(err).Status != 500 {
				t.Errorf("got %v, want an internal error", err)
			}
		})
	}
}

// TestGRIRWithoutPriceDifferenceAccount: an invoice, or a reversal that
// must clear a residue, in a legal entity whose account determination
// lost its price difference account is refused rather than posted
// without one. (Configuration requires it with gr_ir; the row is removed
// here behind its back.)
func TestGRIRWithoutPriceDifferenceAccount(t *testing.T) {
	g := grirSetup(t)
	order, p := uuid.New(), price{1, 3}
	g.post(t, g.invoicePosting("2026-03-02", "EUR", order, "1", p, 1))
	second := g.post(t, g.invoicePosting("2026-03-03", "EUR", order, "1", p, 1))
	g.post(t, g.receiptPosting("2026-03-04", "EUR", order, "-1", p))
	pgtest.Exec(t, `DELETE FROM ledger.account_determination WHERE tenant_id = $1 AND key = 'price_difference'`, g.ten.ID)
	for name, call := range map[string]func(ctx context.Context) error{
		"invoice": func(ctx context.Context) error {
			_, err := postings().Post(ctx, g.invoicePosting("2026-03-05", "EUR", uuid.New(), "1", p, 1))
			return err
		},
		"reversal with a residue": func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, second.ID, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := problem.From(apitest.InRequest(t, g.ten, g.ten.Human, call)).Code; got != "ledger.no_price_difference_account" {
				t.Errorf("got %s, want ledger.no_price_difference_account", got)
			}
		})
	}
}

// TestGRIRDatabaseFailures: every statement of a GR/IR posting and of a
// reversal that adjusts a position reports its failure, and neither runs
// outside a unit of work.
func TestGRIRDatabaseFailures(t *testing.T) {
	g := grirSetup(t)
	settleRate(t, g.env, "USD", "2026-03-02", "0.9")
	order, p := uuid.New(), price{1, 3}
	g.post(t, g.invoicePosting("2026-03-02", "EUR", order, "1", p, 1))
	second := g.post(t, g.invoicePosting("2026-03-03", "EUR", order, "1", p, 1))
	g.post(t, g.receiptPosting("2026-03-04", "EUR", order, "-1", p))
	usd := uuid.New()
	g.post(t, g.receiptPosting("2026-03-02", "USD", usd, "-100", price{100, 1}))
	for name, call := range map[string]func(ctx context.Context) error{
		"receipt": func(ctx context.Context) error {
			_, err := postings().Post(ctx, g.receiptPosting("2026-03-05", "EUR", uuid.New(), "-1", p))
			return err
		},
		"invoice with an exchange difference": func(ctx context.Context) error {
			_, err := postings().Post(ctx, g.invoicePosting("2026-03-20", "USD", usd, "50", price{100, 1}, 5100))
			return err
		},
		"reversal with an adjustment": func(ctx context.Context) error {
			_, err := postings().Reverse(ctx, second.ID, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, g.ten, g.ten.Human, name, call)
		})
	}
}
