package payables

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

type oneEntity struct{}

func (oneEntity) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{Code: "1000", LocalCurrency: "CAD"}, true, nil
}

type oneDocument struct{ doc *config.Config }

func (d oneDocument) Document(context.Context) (*config.Config, int32, error) { return d.doc, 1, nil }

// rates converts at Rate alone, or fails OfType with err.
type rates struct {
	rate string
	err  error
}

func (r rates) Conversion(context.Context, string, string, time.Time) (ledger.Conversion, error) {
	return ledger.Conversion{Rate: r.rate, FromExp: 2, ToExp: 2}, nil
}

func (r rates) OfType(context.Context, string, string, string, time.Time) (ledger.Conversion, bool, error) {
	return ledger.Conversion{}, false, r.err
}

// TestDocumentaryRefusals: a total that cannot be converted is too large,
// a threshold the company's currency cannot hold an error naming it, and
// a failing rate lookup of the tax rate type its error.
func TestDocumentaryRefusals(t *testing.T) {
	doc := &config.Config{Companies: []config.Company{{Code: "1000", TaxSettings: config.TaxSettings{DocumentaryRequirements: []config.DocumentaryRequirement{
		{AtLeast: "100", Requires: []string{config.RequiresSupplierTaxID}, Source: "s"}}}}}}
	e := &Endpoints{LegalEntities: oneEntity{}, Settings: oneDocument{doc}, Rates: rates{rate: "x"}}
	v := Invoice{Kind: KindInvoice, Currency: "USD", Gross: 11900, Tax: 1900}
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	ws, err := e.documentaryWarnings(context.Background(), nil, v, partner.Supplier{}, day)
	if err != nil || len(ws) != 1 || !ws[0].Unassessed {
		t.Errorf("a rate that cannot convert: got %+v, %v, want one unassessed warning", ws, err)
	}
	tiers := []config.DocumentaryRequirement{{AtLeast: "100.001", Requires: []string{config.RequiresSupplierTaxID}}}
	if _, err := unmet(tiers, big.NewRat(1, 1), 2, "CAD", v, partner.Supplier{}, "CA"); err == nil {
		t.Error("a threshold with more digits than the currency's: got nil error, want one")
	}
	failing := errors.New("no rates")
	e.Rates = rates{err: failing}
	if _, err := e.taxConversion(context.Background(), "USD", "CAD", day, "bank_selling"); !errors.Is(err, failing) {
		t.Errorf("a failing rate lookup: got %v, want %v", err, failing)
	}
	if got := settingsOf(nil); !got.IsZero() {
		t.Errorf("settings without the company: got %+v, want the defaults", got)
	}
}
