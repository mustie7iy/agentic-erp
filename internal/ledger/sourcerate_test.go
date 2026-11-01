package ledger_test

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// sourcePost posts lines in a currency at a source system's rate, as a
// migration run posts a document in its document currency.
func sourcePost(ctx context.Context, e env, currency string, rate *big.Rat, lines ...ledger.PostingLine) (ledger.Entry, error) {
	return postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-02"), PostingDate: date("2026-03-02"),
		Currency: currency, SourceContext: "integration", SourceDocumentID: uuid.New(), Lines: lines, Rate: rate})
}

func kept(m money.Minor) *money.Minor { return &m }

// TestSourceRatePosting: a document posted at a source system's rate with
// every line's local amount kept (ADR-0005 As built) records that rate on
// the entry, needs no average rate of its own, keeps each local amount,
// a line of local amount only included (an exchange difference SAP posted
// in local currency alone), and converts the group amounts together so
// they balance; its supplier item is then revalued and counted among the
// open foreign-currency items as an item posted at the average rate is.
// A rate that is not above zero to 12 decimals, a line of no amount in
// either currency and a given rate with GR/IR items are refused.
func TestSourceRatePosting(t *testing.T) {
	e := setup(t)
	revaluationSetup(t, e, true)
	expense, ap, loss := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000"), settleAccount(t, e, "2000", "610000")
	var migrated ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		migrated, err = sourcePost(ctx, e, "USD", big.NewRat(92, 100),
			ledger.PostingLine{AccountID: expense, Amount: 10000, LC: kept(9195)}, ledger.PostingLine{AccountID: loss, Amount: 0, LC: kept(5)},
			ledger.PostingLine{AccountID: ap, Amount: -10000, LC: kept(-9200), PartnerID: &e.supplier})
		return err
	}); err != nil {
		t.Fatalf("post at the source system's rate without an average rate: %v", err)
	}
	if migrated.RateTCLC != "0.920000000000" {
		t.Errorf("the entry's rate: got %s, want 0.920000000000, the source system's", migrated.RateTCLC)
	}
	var lines []string
	for _, l := range migrated.Lines {
		lines = append(lines, fmt.Sprintf("%d %d %d", l.TC, l.LC, l.GC))
	}
	if got, want := strings.Join(lines, ", "), "10000 9195 9195, 0 5 5, -10000 -9200 -9200"; got != want {
		t.Errorf("lines (tc lc gc): got %s, want %s, the local amounts kept and no exchange difference added", got, want)
	}
	settleRate(t, e, "USD", "2026-01-01", "0.92")
	native := revaluationPost(t, e, e.de, "2026-03-02", "USD", ledger.PostingLine{AccountID: expense, Amount: 10000},
		ledger.PostingLine{AccountID: ap, Amount: -10000, PartnerID: &e.supplier})
	revaluationRate(t, e, "USD", "EUR", "2026-03-15", "0.9")
	got, _ := e.Must(200, "POST", "/v1/revaluation-runs/preview", preview(e.de, "2026-03-31"), "")
	valued := map[string]string{}
	for _, x := range got["items"].([]any) {
		it := x.(map[string]any)
		valued[fmt.Sprint(it["journal_entry_id"])] = fmt.Sprint(it["amount"], it["amount_lc"], it["rate"], it["valued_lc"], it["difference_lc"], it["direction"])
	}
	if valued[migrated.ID.String()] == "" || valued[migrated.ID.String()] != valued[native.ID.String()] {
		t.Errorf("revaluation of the migrated item: got %q, want %q, the natively posted one's", valued[migrated.ID.String()], valued[native.ID.String()])
	}
	sourceRateReports(t, e)
	sourceRateRefusals(t, e, expense, ap)
}

// sourceRateReports: the open foreign-currency items per partner and
// currency are the two USD items, the EUR one left out.
func sourceRateReports(t *testing.T, e env) {
	t.Helper()
	revaluationPost(t, e, e.de, "2026-03-02", "EUR", ledger.PostingLine{AccountID: settleAccount(t, e, "2000", "520000"), Amount: 700},
		ledger.PostingLine{AccountID: settleAccount(t, e, "2000", "211000"), Amount: -700, PartnerID: &e.vendor})
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		open, err := ledger.Reports{}.OpenForeignItems(ctx, e.de, 2026)
		if got := fmt.Sprint(open); err == nil && got != fmt.Sprintf("map[%s:map[USD:-20000 minor]]", e.supplier) {
			t.Errorf("open foreign-currency items: got %s, want the supplier's two USD items, -20000", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(ctx context.Context) error{
		"open foreign-currency items": func(ctx context.Context) error {
			_, err := ledger.Reports{}.OpenForeignItems(ctx, e.de, 2026)
			return err
		},
		"entry currencies": func(ctx context.Context) error {
			_, err := postings().EntryCurrencies(ctx, []uuid.UUID{uuid.New()})
			return err
		},
	} {
		if read(context.Background()) == nil {
			t.Errorf("%s outside a unit of work: got no error, want one", name)
		}
		apitest.EveryFailure(t, e.ten, e.ten.Human, name, read)
	}
}

// TestEntryCurrencies: the transaction currency of each entry that exists,
// an id that names none left out.
func TestEntryCurrencies(t *testing.T) {
	e := setup(t)
	expense, ap := settleAccount(t, e, "2000", "520000"), settleAccount(t, e, "2000", "211000")
	var usd ledger.Entry
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		usd, err = sourcePost(ctx, e, "USD", big.NewRat(9, 10), ledger.PostingLine{AccountID: expense, Amount: 100, LC: kept(90)},
			ledger.PostingLine{AccountID: ap, Amount: -100, LC: kept(-90), PartnerID: &e.supplier})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	eur := revaluationPost(t, e, e.de, "2026-03-02", "EUR", ledger.PostingLine{AccountID: expense, Amount: 100},
		ledger.PostingLine{AccountID: ap, Amount: -100, PartnerID: &e.supplier})
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		got, err := postings().EntryCurrencies(ctx, []uuid.UUID{usd.ID, eur.ID, uuid.New()})
		if want := fmt.Sprint(map[uuid.UUID]string{usd.ID: "USD", eur.ID: "EUR"}); err == nil && fmt.Sprint(got) != want {
			t.Errorf("entry currencies: got %v, want %s", got, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// sourceRateRefusals: what a posting at a source system's rate refuses.
func sourceRateRefusals(t *testing.T, e env, expense, ap uuid.UUID) {
	t.Helper()
	tiny := new(big.Rat).SetFrac64(1, 10_000_000_000_000)
	for _, c := range []struct {
		name  string
		rate  *big.Rat
		lines []ledger.PostingLine
		grir  bool
		code  string
	}{
		{"a rate that rounds to zero", tiny, []ledger.PostingLine{{AccountID: expense, Amount: 100, LC: kept(1)}, {AccountID: ap, Amount: -100, LC: kept(-1), PartnerID: &e.supplier}},
			false, "ledger.rate_not_recordable"},
		{"a rate of 10^16", new(big.Rat).SetFrac64(10_000_000_000_000_000, 1), []ledger.PostingLine{{AccountID: expense, Amount: 100, LC: kept(1)},
			{AccountID: ap, Amount: -100, LC: kept(-1), PartnerID: &e.supplier}}, false, "ledger.rate_not_recordable"},
		{"a negative rate", big.NewRat(-1, 1), []ledger.PostingLine{{AccountID: expense, Amount: 100, LC: kept(1)}, {AccountID: ap, Amount: -100, LC: kept(-1), PartnerID: &e.supplier}},
			false, "ledger.rate_not_recordable"},
		{"a line of no amount in either currency", big.NewRat(1, 1), []ledger.PostingLine{{AccountID: expense, Amount: 0, LC: kept(0)},
			{AccountID: expense, Amount: 100, LC: kept(100)}, {AccountID: ap, Amount: -100, LC: kept(-100), PartnerID: &e.supplier}}, false, "ledger.zero_amount"},
		{"GR/IR items", big.NewRat(1, 1), nil, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				in := ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-02"), PostingDate: date("2026-03-02"),
					Currency: "USD", SourceContext: "integration", SourceDocumentID: uuid.New(), Lines: c.lines, Rate: c.rate}
				if c.grir {
					in.GRIR = []ledger.GRIRItem{{PurchaseOrderID: uuid.New(), POItemNo: 1, Quantity: "1", Unit: "EA"}}
				}
				_, err := postings().Post(ctx, in)
				return err
			})
			switch got := problem.From(err); {
			case c.code == "" && (err == nil || got.Status != 500):
				t.Errorf("got %v, want an internal error", err)
			case c.code != "" && got.Code != c.code:
				t.Errorf("got %v, want %s", err, c.code)
			}
		})
	}
}
