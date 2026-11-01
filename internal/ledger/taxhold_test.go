package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// holdingAll is a tax filing context whose filed return holds every tax
// date, or whose read fails with err.
type holdingAll struct{ err error }

func (h holdingAll) Holding(context.Context, uuid.UUID, time.Time) (ledger.HeldReturn, bool, error) {
	return ledger.HeldReturn{ID: uuid.New(), Registration: "TEST", PeriodStart: date("2026-01-01"), PeriodEnd: date("2026-03-31"),
		Kind: "original", Status: "filed"}, h.err == nil, h.err
}

// TestChosenTaxDateHeld: a context's posting with tax whose tax date a
// person chose (a supplier invoice's, P6) is held to the open returns, on
// an order or not; one whose date the document gives is not. A ledger
// wired without the tax filing context, or whose read fails, fails the
// posting rather than post it unheld.
func TestChosenTaxDateHeld(t *testing.T) {
	g := grirSetup(t)
	order := uuid.New()
	g.post(t, g.receiptPosting("2026-03-01", "EUR", order, "-1", price{10000, 1}))
	v1 := "V1"
	tax := ledger.PostingLine{AccountID: settleAccount(t, g.env, "2000", "140000"), Amount: 1900, TaxCode: &v1}
	onOrder := g.invoicePosting("2026-05-05", "EUR", order, "1", price{10000, 1}, 10000)
	onOrder.Lines = append(onOrder.Lines, tax)
	onOrder.Lines[0].Amount = -11900
	onAccount := ledger.Posting{LegalEntityID: g.de, DocumentType: "SA", DocumentDate: date("2026-03-10"), PostingDate: date("2026-05-05"),
		Currency: "EUR", SourceContext: "payables", SourceDocumentID: uuid.New(), Lines: []ledger.PostingLine{
			{AccountID: g.expense, Amount: 10000, TaxCode: &v1}, tax, {AccountID: g.ap, Amount: -11900, PartnerID: &g.supplier}}}
	taxDate := date("2026-03-10")
	for name, in := range map[string]ledger.Posting{"on an order": onOrder, "on an account": onAccount} {
		in.TaxDate = &taxDate
		post := func(t *testing.T, returns ledger.TaxReturns, chosen bool) error {
			t.Helper()
			in.TaxDateChosen = chosen
			p := ledger.Postings{LegalEntities: tenancy.Directory{}, Partners: partner.Postings{}, Numbers: configuration.Numbers{}, TaxReturns: returns}
			return apitest.InRequest(t, g.ten, g.ten.Human, func(ctx context.Context) error {
				_, err := p.Post(ctx, in)
				return err
			})
		}
		t.Run(name, func(t *testing.T) {
			if err := post(t, holdingAll{}, false); err != nil {
				t.Errorf("the document's tax date: got %v, want it posted", err)
			}
			if got := problem.From(post(t, holdingAll{}, true)).Code; got != "ledger.tax_date_in_return" {
				t.Errorf("a chosen tax date in a filed period: got %s, want ledger.tax_date_in_return", got)
			}
			if err := post(t, nil, true); err == nil || problem.From(err).Status != 500 {
				t.Errorf("no tax filing context: got %v, want an internal error", err)
			}
			if err := post(t, holdingAll{err: errors.New("down")}, true); err == nil || problem.From(err).Status != 500 {
				t.Errorf("the read failing: got %v, want an internal error", err)
			}
		})
	}
}
