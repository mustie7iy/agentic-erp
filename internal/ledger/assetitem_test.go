package ledger_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// documentSettings is the active configuration as a posting reads it: the
// document the ledger was set up from, or its failure.
type documentSettings struct {
	doc *config.Config
	err error
}

func (s documentSettings) Document(context.Context) (*config.Config, int32, error) {
	return s.doc, 1, s.err
}

// assetItemEnv is a grirEnv whose company 2000 has an asset clearing
// account, 169000, beside a second asset account that is none, 135000.
type assetItemEnv struct {
	grirEnv
	doc             *config.Config
	clearing, stock uuid.UUID
}

func assetItemSetup(t *testing.T) assetItemEnv {
	t.Helper()
	g, doc := grirSetupWith(t, func(de *config.Company) {
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "169000", Name: "Asset clearing", Type: config.Asset},
			config.Account{Number: "135000", Name: "Stock", Type: config.Asset})
		// Two classes sharing the clearing account: it is judged once.
		de.AssetAccounts = []config.AssetAccounts{{Class: "MACHINES", Clearing: "169000"}, {Class: "VEHICLES", Clearing: "169000"}}
	})
	id := func(number string) uuid.UUID { return uuid.MustParse(g.account(t, "2000", number)) }
	return assetItemEnv{grirEnv: g, doc: doc, clearing: id("169000"), stock: id("135000")}
}

// postings are the ledger's postings reading the set-up document as the
// active configuration.
func (a assetItemEnv) postings() ledger.Postings {
	p := postings()
	p.Settings = documentSettings{doc: a.doc}
	return p
}

// post posts in a committed transaction, the item accounts judged by the
// set-up document.
func (a assetItemEnv) post(t *testing.T, in ledger.Posting) ledger.Entry {
	t.Helper()
	var entry ledger.Entry
	if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
		entry, err = a.postings().Post(ctx, in)
		return err
	}); err != nil {
		t.Fatalf("post: %v", err)
	}
	return entry
}

// invoiceOf is invoicePosting with the order item's account and the tax
// the item bears.
func (a assetItemEnv) invoiceOf(day string, order uuid.UUID, qty string, net, tax money.Minor, account *uuid.UUID) ledger.Posting {
	in := a.invoicePosting(day, "EUR", order, qty, price{100, 1}, net)
	in.GRIR[0].ItemAccountID, in.GRIR[0].TaxBorne = account, tax
	return in
}

// TestAssetItemDifferences: what an invoice or credit memo differs from
// the order price for an order item on an asset clearing account, the
// tax it bears included, posts to that account, not to the price
// difference account, and is recorded with its order item and tax as any
// difference line; every other line is as it was; an item on any other
// account posts as before (ADR-0057 §6; product owner 2026-11-10).
func TestAssetItemDifferences(t *testing.T) {
	a := assetItemSetup(t)
	t.Run("an invoice over the order price with tax borne", func(t *testing.T) {
		order := uuid.New()
		before := a.post(t, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 25, nil))
		entry := a.post(t, a.invoiceOf("2026-03-02", order, "1", 140, 25, &a.clearing))
		if got, want := one(t, entry, a.clearing, "the difference"), [3]money.Minor{40, 40, 40}; got != want {
			t.Errorf("line on the asset clearing account: got %v, want %v", got, want)
		}
		if got := on(entry, a.diff); len(got) != 0 {
			t.Errorf("lines on the price difference account: got %v, want none", got)
		}
		for name, account := range map[string]uuid.UUID{"GR/IR": a.grir, "payable": a.ap} {
			if got, want := on(entry, account), on(before, account); !reflect.DeepEqual(got, want) {
				t.Errorf("%s lines: got %v, want %v as an item on another account posts", name, got, want)
			}
		}
		if got, want := len(entry.Lines), len(before.Lines); got != want {
			t.Errorf("lines: got %d, want %d", got, want)
		}
		want := []recorded{{LineNo: lineOn(t, entry, a.clearing, 0), Order: order, Item: 10, TC: 25, LC: 25}}
		if got := recordedOf(t, entry); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded: got %+v, want %+v", got, want)
		}
	})
	t.Run("a credit memo", func(t *testing.T) {
		order := uuid.New()
		a.post(t, a.invoiceOf("2026-03-02", order, "2", 200, 0, &a.clearing))
		entry := a.post(t, a.invoiceOf("2026-03-03", order, "-1", -112, -5, &a.clearing))
		if got, want := one(t, entry, a.clearing, "the difference"), [3]money.Minor{-12, -12, -12}; got != want {
			t.Errorf("line on the asset clearing account: got %v, want %v", got, want)
		}
		if got := on(entry, a.diff); len(got) != 0 {
			t.Errorf("lines on the price difference account: got %v, want none", got)
		}
	})
	t.Run("an item on another account", func(t *testing.T) {
		for name, account := range map[string]*uuid.UUID{"an expense account": &a.expense, "an asset account that is no clearing account": &a.stock} {
			entry := a.post(t, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 25, account))
			if got, want := one(t, entry, a.diff, "the difference"), [3]money.Minor{40, 40, 40}; got != want {
				t.Errorf("%s: line on the price difference account: got %v, want %v", name, got, want)
			}
			if got := on(entry, *account); len(got) != 0 {
				t.Errorf("%s: lines on the item's account: got %v, want none", name, got)
			}
		}
	})
	t.Run("a company without asset accounts in the configuration", func(t *testing.T) {
		// The tenant's bootstrapped configuration has no asset accounts.
		p := a.postings()
		p.Settings = configuration.Settings{}
		var entry ledger.Entry
		if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
			entry, err = p.Post(ctx, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 0, &a.clearing))
			return err
		}); err != nil {
			t.Fatalf("post: %v", err)
		}
		if got, want := one(t, entry, a.diff, "the difference"), [3]money.Minor{40, 40, 40}; got != want {
			t.Errorf("line on the price difference account: got %v, want %v", got, want)
		}
	})
	t.Run("a company the configuration does not have", func(t *testing.T) {
		p := a.postings()
		p.Settings = documentSettings{doc: &config.Config{}}
		var entry ledger.Entry
		if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
			entry, err = p.Post(ctx, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 0, &a.clearing))
			return err
		}); err != nil {
			t.Fatalf("post: %v", err)
		}
		if got, want := one(t, entry, a.diff, "the difference"), [3]money.Minor{40, 40, 40}; got != want {
			t.Errorf("line on the price difference account: got %v, want %v", got, want)
		}
	})
	t.Run("a receipt reads no configuration", func(t *testing.T) {
		p := a.postings()
		p.Settings = nil
		in := a.receiptPosting("2026-03-02", "EUR", uuid.New(), "-1", price{100, 1})
		in.GRIR[0].ItemAccountID = &a.clearing // not an invoiced or clearing item: not judged
		if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
			_, err := p.Post(ctx, in)
			return err
		}); err != nil {
			t.Errorf("got %v, want it posted", err)
		}
	})
	lost := errors.New("configuration lost")
	for name, c := range map[string]struct {
		settings ledger.Settings
		want     string
	}{
		"no configuration to read":           {nil, "no configuration to read the asset clearing accounts from"},
		"a configuration that does not load": {documentSettings{err: lost}, "asset clearing accounts of 2000: configuration lost"},
	} {
		t.Run(name, func(t *testing.T) {
			p := a.postings()
			p.Settings = c.settings
			err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) error {
				_, err := p.Post(ctx, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 0, &a.clearing))
				return err
			})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want %q", err, c.want)
			}
		})
	}
}

// TestAssetItemDifferencesReversed: a reversal mirrors what its original
// posted. An asset's item's difference on the asset clearing account is
// reversed there; one a document posted to the price difference account
// before the change is reversed on that account, though the item would
// post to the clearing account now.
func TestAssetItemDifferencesReversed(t *testing.T) {
	a := assetItemSetup(t)
	reverse := func(t *testing.T, entry ledger.Entry) ledger.Entry {
		t.Helper()
		var rev ledger.Entry
		if err := apitest.Committed(t, a.ten, a.ten.Human, func(ctx context.Context) (err error) {
			rev, err = a.postings().Reverse(ctx, entry.ID, nil, nil)
			return err
		}); err != nil {
			t.Fatalf("reverse: %v", err)
		}
		return rev
	}
	t.Run("posted to the asset clearing account", func(t *testing.T) {
		order := uuid.New()
		entry := a.post(t, a.invoiceOf("2026-03-02", order, "1", 140, 25, &a.clearing))
		rev := reverse(t, entry)
		if got, want := one(t, rev, a.clearing, "the reversed difference"), [3]money.Minor{-40, -40, -40}; got != want {
			t.Errorf("reversal's line on the asset clearing account: got %v, want %v", got, want)
		}
		if got := on(rev, a.diff); len(got) != 0 {
			t.Errorf("reversal's lines on the price difference account: got %v, want none", got)
		}
		want := []recorded{{LineNo: lineOn(t, entry, a.clearing, 0), Order: order, Item: 10, TC: -25, LC: -25}}
		if got := recordedOf(t, rev); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded for the reversal: got %+v, want %+v", got, want)
		}
	})
	t.Run("posted to the price difference account before the change", func(t *testing.T) {
		order := uuid.New()
		// As the version before posted it: the item names no account.
		entry := a.post(t, a.invoiceOf("2026-03-02", order, "1", 140, 25, nil))
		if got, want := one(t, entry, a.diff, "the difference"), [3]money.Minor{40, 40, 40}; got != want {
			t.Fatalf("line on the price difference account: got %v, want %v", got, want)
		}
		rev := reverse(t, entry)
		if got, want := one(t, rev, a.diff, "the reversed difference"), [3]money.Minor{-40, -40, -40}; got != want {
			t.Errorf("reversal's line on the price difference account: got %v, want %v", got, want)
		}
		if got := on(rev, a.clearing); len(got) != 0 {
			t.Errorf("reversal's lines on the asset clearing account: got %v, want none", got)
		}
		want := []recorded{{LineNo: lineOn(t, entry, a.diff, 0), Order: order, Item: 10, TC: -25, LC: -25}}
		if got := recordedOf(t, rev); !reflect.DeepEqual(got, want) {
			t.Errorf("recorded for the reversal: got %+v, want %+v", got, want)
		}
	})
}

// TestAssetItemDatabaseFailures: every statement of an asset's item's
// invoice reports its failure, the read of the asset clearing accounts
// included.
func TestAssetItemDatabaseFailures(t *testing.T) {
	a := assetItemSetup(t)
	call := func(ctx context.Context) error {
		_, err := a.postings().Post(ctx, a.invoiceOf("2026-03-02", uuid.New(), "1", 140, 25, &a.clearing))
		return err
	}
	if call(context.Background()) == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, a.ten, a.ten.Human, "invoice of an asset's item", call)
}
