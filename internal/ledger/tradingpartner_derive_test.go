package ledger_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Trading partners derived from business partners (T68c, ADR-0059 §4): a
// line naming a partner that is a group company takes that company, the
// entry's other lines take the one its partner lines share, a line naming
// another than its partner's is refused, offsets of open items keep their
// items' and reversals their originals'.

// derived is a tenant whose supplier is the group company 1000 (us) and
// whose vendor is unlinked, with a third company 3000 (maple).
type derived struct {
	env
	maple uuid.UUID
	names map[uuid.UUID]string
}

func derivedSetup(t *testing.T) derived {
	t.Helper()
	d := derived{env: setup(t), maple: uuid.Must(uuid.NewV7())}
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '3000', 'Maple', 'CA', 'CAD', $3, 'active', $4, $4)`, d.ten.ID, d.maple, d.usCal, d.ten.Human.ID)
	d.link(t, d.supplier, &d.us)
	d.names = map[uuid.UUID]string{d.us: "us", d.de: "de", d.maple: "maple"}
	return d
}

// link makes the partner the group company, or unlinks it (nil).
func (d derived) link(t *testing.T, partnerID uuid.UUID, company *uuid.UUID) {
	t.Helper()
	pgtest.Exec(t, `UPDATE partner.business_partner SET group_company_id = $2, version = version + 1 WHERE id = $1`, partnerID, company)
}

// of is each line's trading partner by name, "-" for none.
func (d derived) of(lines []ledger.Line) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = "-"
		if l.TradingPartnerID != nil {
			out[i] = d.names[*l.TradingPartnerID]
		}
	}
	return strings.Join(out, " ")
}

// ofRep is each line of a journal entry representation's trading partner
// by name.
func (d derived) ofRep(t *testing.T, rep map[string]any) string {
	t.Helper()
	var out []string
	for _, l := range apitest.List(t, rep, "lines") {
		tp, ok := l.(map[string]any)["trading_partner"].(map[string]any)
		if !ok {
			out = append(out, "-")
			continue
		}
		out = append(out, d.names[uuid.MustParse(tp["id"].(string))])
	}
	return strings.Join(out, " ")
}

// post posts through the posting port, committed.
func (d derived) post(t *testing.T, in ledger.Posting) ledger.Entry {
	t.Helper()
	return d.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Post(ctx, in) })
}

func (d derived) committed(t *testing.T, call func(ctx context.Context) (ledger.Entry, error)) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, d.ten, d.ten.Human, func(ctx context.Context) (err error) {
		entry, err = call(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestTradingPartnerDerivedOnManualEntries(t *testing.T) {
	d := derivedSetup(t)
	bank, ap := d.account(t, "2000", "100000"), d.account(t, "2000", "211000")
	supplier, vendor, us, maple := d.supplier.String(), d.vendor.String(), d.us.String(), d.maple.String()
	for name, c := range map[string]struct {
		lines []partnerLine
		want  string
	}{
		"the partner line takes its group company, the other line takes it too": {
			[]partnerLine{{account: bank, amount: "5.00"}, {account: ap, amount: "-5.00", partner: supplier}}, "us us"},
		"a partner line naming its group company keeps it": {
			[]partnerLine{{account: bank, amount: "5.00"}, {account: ap, amount: "-5.00", partner: supplier, tradingPartner: us}}, "us us"},
		"a line without a partner keeps its own": {
			[]partnerLine{{account: bank, amount: "5.00", tradingPartner: maple}, {account: ap, amount: "-5.00", partner: supplier}}, "maple us"},
		"a partner linked to none keeps the line's own": {
			[]partnerLine{{account: bank, amount: "5.00"}, {account: ap, amount: "-5.00", partner: vendor, tradingPartner: maple}}, "maple maple"},
		"partner lines naming different ones give the others none": {
			[]partnerLine{{account: bank, amount: "10.00"}, {account: ap, amount: "-5.00", partner: supplier},
				{account: ap, amount: "-5.00", partner: vendor}}, "- us -"},
	} {
		t.Run(name, func(t *testing.T) {
			je, _ := d.Must(201, "POST", "/v1/journal-entries", partnerEntry(d.de, "2026-03-15", c.lines...), "")
			if got := d.ofRep(t, je); got != c.want {
				t.Errorf("lines: got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("a partner line naming another than its group company is refused", func(t *testing.T) {
		p := d.Problem(422, "ledger.trading_partner_conflict", "POST", "/v1/journal-entries", partnerEntry(d.de, "2026-03-15",
			partnerLine{account: bank, amount: "5.00"}, partnerLine{account: ap, amount: "-5.00", partner: supplier, tradingPartner: maple}), "")
		if got := fieldErrors(t, p); got != "lines[1].trading_partner_id:conflict" {
			t.Errorf("field errors: got %q, want lines[1].trading_partner_id:conflict", got)
		}
	})
	t.Run("a partner that is the posting company gives none", func(t *testing.T) {
		d.link(t, d.vendor, &d.de)
		je, _ := d.Must(201, "POST", "/v1/journal-entries", partnerEntry(d.de, "2026-03-15",
			partnerLine{account: bank, amount: "5.00"}, partnerLine{account: ap, amount: "-5.00", partner: vendor}), "")
		if got := d.ofRep(t, je); got != "- -" {
			t.Errorf("lines: got %q, want - -", got)
		}
		d.link(t, d.vendor, nil)
	})
	t.Run("a parked entry takes it when it is posted", func(t *testing.T) {
		pe, tag := d.Must(201, "POST", "/v1/parked-journal-entries", partnerEntry(d.de, "2026-03-15",
			partnerLine{account: bank, amount: "5.00"}, partnerLine{account: ap, amount: "-5.00", partner: supplier}), "")
		if got := d.ofRep(t, pe); got != "- -" {
			t.Errorf("parked: got %q, want - - (derived when posted)", got)
		}
		posted, _ := d.Must(200, "POST", "/v1/parked-journal-entries/"+pe["id"].(string)+":post", "", tag)
		je, _ := d.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
		if got := d.ofRep(t, je); got != "us us" {
			t.Errorf("posted: got %q, want us us", got)
		}
	})
	t.Run("a reversal keeps the original's after the partner is unlinked", func(t *testing.T) {
		je, tag := d.Must(201, "POST", "/v1/journal-entries", partnerEntry(d.de, "2026-03-15",
			partnerLine{account: bank, amount: "5.00"}, partnerLine{account: ap, amount: "-5.00", partner: supplier}), "")
		d.link(t, d.supplier, nil)
		defer d.link(t, d.supplier, &d.us)
		rev, _ := d.Must(200, "POST", "/v1/journal-entries/"+je["id"].(string)+":reverse", "", tag)
		if got := d.ofRep(t, rev); got != "us us" {
			t.Errorf("reversal: got %q, want us us", got)
		}
		later, _ := d.Must(201, "POST", "/v1/journal-entries", partnerEntry(d.de, "2026-03-15",
			partnerLine{account: bank, amount: "5.00"}, partnerLine{account: ap, amount: "-5.00", partner: supplier}), "")
		if got := d.ofRep(t, later); got != "- -" {
			t.Errorf("a posting after unlinking: got %q, want - -", got)
		}
	})
}

// TestTradingPartnerDerivedOnPostingPaths: a supplier invoice, a payment,
// a bank statement line clearing the payment and a down payment with its
// transfer, each as its context posts it, carry the supplier's group
// company without naming it.
func TestTradingPartnerDerivedOnPostingPaths(t *testing.T) {
	d := derivedSetup(t)
	downPayments(t, d.env)
	dp, bank, ap, tax := settleAccount(t, d.env, "2000", "159000"), settleAccount(t, d.env, "2000", "100000"), settleAccount(t, d.env, "2000", "211000"),
		settleAccount(t, d.env, "2000", "140000")
	expense, clearingAccount := settleAccount(t, d.env, "2000", "520000"), settleAccount(t, d.env, "2000", "510000")
	pgtest.Exec(t, `UPDATE ledger.company_account SET is_open_item_managed = true, version = version + 1 WHERE legal_entity_id = $1 AND account_id = $2`,
		d.de, clearingAccount)
	posting := func(lines ...ledger.PostingLine) ledger.Posting {
		return ledger.Posting{LegalEntityID: d.de, DocumentType: "SA", DocumentDate: date("2026-03-01"), PostingDate: date("2026-03-01"), Currency: "EUR",
			SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: lines}
	}
	invoice := func(t *testing.T) ledger.Entry {
		t.Helper()
		return d.post(t, posting(ledger.PostingLine{AccountID: expense, Amount: 10000}, ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &d.supplier}))
	}
	item := func(e ledger.Entry, line int32) ledger.ItemRef {
		return ledger.ItemRef{FiscalYear: e.FiscalYear, EntryID: e.ID, LineNo: line}
	}
	var payment ledger.Entry
	t.Run("a supplier invoice", func(t *testing.T) {
		if got := d.of(invoice(t).Lines); got != "us us" {
			t.Errorf("lines: got %q, want us us", got)
		}
	})
	t.Run("a payment", func(t *testing.T) {
		s := settlementOf(t, d.env, d.de, "2000", "EUR", "2026-03-20", item(invoice(t), 2))
		s.BankAccountID = clearingAccount // the bank's clearing account, which the statement clears
		payment = d.committed(t, func(ctx context.Context) (ledger.Entry, error) { return postings().Settle(ctx, s) })
		if got := d.of(payment.Lines); got != "us us" {
			t.Errorf("lines (the item's offset, the bank clearing line): got %q, want us us", got)
		}
	})
	t.Run("a bank statement line clearing the payment", func(t *testing.T) {
		entry := d.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().PostBank(ctx, ledger.BankPosting{LegalEntityID: d.de, DocumentType: "SA", PostingDate: date("2026-03-22"), Currency: "EUR",
				BankAccountID: bank, Amount: -10000, ClearingAccountID: clearingAccount, Entries: []ledger.ItemRef{item(payment, 0)},
				SourceContext: "payments", SourceDocumentID: uuid.New()})
		})
		// The payment's line it offsets keeps its trading partner; the bank
		// line names no partner, and no partner line names one.
		if got := d.of(entry.Lines); got != "us -" {
			t.Errorf("lines: got %q, want us -", got)
		}
	})
	t.Run("a payment of an item posted before the partner was linked", func(t *testing.T) {
		d.link(t, d.supplier, nil)
		before := invoice(t)
		d.link(t, d.supplier, &d.us)
		entry := d.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().Settle(ctx, settlementOf(t, d.env, d.de, "2000", "EUR", "2026-03-20", item(before, 2)))
		})
		if got := d.of(entry.Lines); got != "- -" {
			t.Errorf("lines: got %q, want - - (the item's)", got)
		}
	})
	t.Run("a down payment and its transfer", func(t *testing.T) {
		v1, a := "V1", "A"
		paid := d.post(t, posting(ledger.PostingLine{AccountID: dp, Amount: 10000, PartnerID: &d.supplier, SpecialGL: &a},
			ledger.PostingLine{AccountID: tax, Amount: 1900, TaxCode: &v1}, ledger.PostingLine{AccountID: bank, Amount: -11900}))
		if got := d.of(paid.Lines); got != "us us us" {
			t.Errorf("down payment: got %q, want us us us", got)
		}
		transfer := d.committed(t, func(ctx context.Context) (ledger.Entry, error) {
			return postings().ClearDownPayments(ctx, ledger.DownPaymentClearing{LegalEntityID: d.de, SupplierID: d.supplier, APAccountID: ap, DocumentType: "SA",
				PostingDate: date("2026-03-20"), Currency: "EUR", Items: []ledger.ItemRef{item(paid, 1)}, SourceContext: "payables", SourceDocumentID: uuid.New()})
		})
		// The down payment's offset, its tax reversal, the payables line.
		if got := d.of(transfer.Lines); got != "us us us" {
			t.Errorf("transfer: got %q, want us us us", got)
		}
	})
	t.Run("a posting naming another for the partner line is refused", func(t *testing.T) {
		in := posting(ledger.PostingLine{AccountID: expense, Amount: 10000}, ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &d.supplier})
		in.TradingPartner = &d.maple
		err := apitest.InRequest(t, d.ten, d.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, in)
			return err
		})
		if got := problem.From(err).Code; got != "ledger.trading_partner_conflict" {
			t.Errorf("error: got %s (%v), want ledger.trading_partner_conflict", got, err)
		}
	})
}

// TestTradingPartnerDerivedDatabaseFailures posts an entry naming a linked
// partner with each database statement failing in turn, the group
// company's read among them.
func TestTradingPartnerDerivedDatabaseFailures(t *testing.T) {
	d := derivedSetup(t)
	bank, ap := uuid.MustParse(d.account(t, "2000", "100000")), uuid.MustParse(d.account(t, "2000", "211000"))
	create := &gen.JournalEntryCreate{LegalEntityID: d.de, DocumentDate: date("2026-03-15"), PostingDate: date("2026-03-15"), Currency: "EUR",
		Lines: []gen.JournalLineInput{{AccountID: bank, Amount: gen.Money{Amount: "1.00", Currency: "EUR"}},
			{AccountID: ap, Amount: gen.Money{Amount: "-1.00", Currency: "EUR"}, PartnerID: gen.NewOptUUID(d.supplier)}}}
	apitest.EveryFailure(t, d.ten, d.ten.Human, "post", func(ctx context.Context) error {
		_, err := endpoints().PostJournalEntry(ctx, create, gen.PostJournalEntryParams{})
		return err
	})
	t.Run("posted", func(t *testing.T) {
		var res gen.PostJournalEntryRes
		if err := apitest.Committed(t, d.ten, d.ten.Human, func(ctx context.Context) (err error) {
			res, err = endpoints().PostJournalEntry(ctx, create, gen.PostJournalEntryParams{})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%T", res); !strings.Contains(got, "JournalEntry") {
			t.Errorf("response: got %s, want the journal entry", got)
		}
	})
}
