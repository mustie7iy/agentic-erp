package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/closing"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Closing is the financial close context as the API wires it (ADR-0058
// §2): a leaf that reads the configuration, tenancy and ledger contexts
// through the adapters below, so none of them imports it and it imports
// none of them.
func Closing() *closing.Endpoints {
	return &closing.Endpoints{Settings: configuration.Settings{}, Companies: closingCompanies{}, Ledger: closingLedger{}}
}

// closingCompanies gives the closing context a legal entity's code and
// name.
type closingCompanies struct{}

func (closingCompanies) Company(ctx context.Context, id uuid.UUID) (closing.Company, bool, error) {
	le, ok, err := tenancy.Directory{}.Entity(ctx, id)
	return closing.Company{ID: le.ID, Code: le.Code, Name: le.Name}, ok, err
}

// closingLedger gives the closing context the ledger's posting periods.
type closingLedger struct{}

func (closingLedger) LeadingPeriods(ctx context.Context, legalEntity uuid.UUID, fiscalYear int16) ([]closing.Period, error) {
	ps, err := ledger.ClosePeriods{}.Leading(ctx, legalEntity, fiscalYear)
	out := make([]closing.Period, len(ps))
	for i, p := range ps {
		out[i] = closing.Period{ID: p.ID, Number: p.Period, StartDate: p.StartDate, EndDate: p.EndDate, Status: p.Status}
	}
	return out, err
}
