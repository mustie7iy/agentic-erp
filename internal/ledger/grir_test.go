package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// withGRIR is the document with company 2000 valuing receipts through a
// GR/IR account, 219000, with price differences on 590000.
func withGRIR(tb testing.TB) *config.Config {
	tb.Helper()
	doc := document(tb)
	de := &doc.Companies[1]
	de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "219000", Name: "GR/IR clearing", Type: config.Liability},
		config.Account{Number: "590000", Name: "Price differences", Type: config.Expense}, config.Account{Number: "218000", Name: "GR/IR 2", Type: config.Liability})
	de.AccountDetermination.GRIR, de.AccountDetermination.PriceDifference = "219000", "590000"
	return doc
}

// TestGRIRAccounts: the configuration's GR/IR and price difference accounts
// reach the ledger's account determination; the GR/IR account is posted
// only by goods receipts and invoices, never named on an order or invoice
// item or in a manual entry, while an entry a migration run loaded may
// carry SAP's GR/IR lines; and once activated it stays.
func TestGRIRAccounts(t *testing.T) {
	e := setup(t)
	s := ledger.Setup{LegalEntities: tenancy.Directory{}}
	doc := withGRIR(t)
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return s.Apply(ctx, doc) }); err != nil {
		t.Fatal(err)
	}
	grir, diff, bank := e.account(t, "2000", "219000"), e.account(t, "2000", "590000"), e.account(t, "2000", "100000")
	a := ledger.Accounts{}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		for key, want := range map[string]string{"gr_ir": grir, "price_difference": diff} {
			if id, ok, err := a.Determination(ctx, e.de, key); err != nil || !ok || id.String() != want {
				t.Errorf("determination %s: got %s %v %v, want %s", key, id, ok, err, want)
			}
		}
		for account, want := range map[string]bool{grir: false, diff: true} {
			if ok, err := a.IsPostable(ctx, e.de, uuid.MustParse(account)); err != nil || ok != want {
				t.Errorf("postable %s: got %v %v, want %v", account, ok, err, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Problem(422, "ledger.automatic_account", "POST", "/v1/journal-entries", entry(e.de, "2026-03-15", "EUR", line{grir, "10.00", ""}, line{bank, "-10.00", ""}), "")
	post := func(source string) error {
		return apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := postings().Post(ctx, ledger.Posting{LegalEntityID: e.de, DocumentType: "SA", DocumentDate: date("2026-03-01"),
				PostingDate: date("2026-03-02"), Currency: "EUR", SourceContext: source, SourceDocumentID: uuid.New(),
				Lines: []ledger.PostingLine{{AccountID: uuid.MustParse(grir), Amount: -100}, {AccountID: uuid.MustParse(bank), Amount: 100}}})
			return err
		})
	}
	if err := post("integration"); err != nil {
		t.Errorf("a migration run's GR/IR line: got %v, want it posted", err)
	}
	if err := post("payables"); err == nil || problem.From(err).Code != "ledger.automatic_account" {
		t.Errorf("a GR/IR line without its order item: got %v, want ledger.automatic_account", err)
	}
	checkGRIRStays(t, e, s, doc)
}

// checkGRIRStays: a document that changes or removes the activated GR/IR
// account is refused; one that keeps it, or sets it for the first time,
// is not.
func checkGRIRStays(t *testing.T, e env, s ledger.Setup, base *config.Config) {
	t.Helper()
	for _, c := range []struct {
		name, grir, want string
		base             *config.Config
	}{
		{"kept", "219000", "", base},
		{"changed", "218000", "companies[2000].account_determination.gr_ir: the GR/IR account cannot change once activated", base},
		{"removed", "", "companies[2000].account_determination.gr_ir: the GR/IR account cannot change once activated", base},
		{"set the first time", "219000", "", document(t)},
	} {
		t.Run(c.name, func(t *testing.T) {
			proposed := withGRIR(t)
			proposed.Companies[1].AccountDetermination.GRIR = c.grir
			var got []string
			err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
				fs, err := s.Check(ctx, c.base, proposed)
				for _, f := range fs {
					got = append(got, f.Path+": "+f.Message)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != c.want {
				t.Errorf("findings: got %q, want %q", got, c.want)
			}
		})
	}
}
