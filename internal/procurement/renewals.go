package procurement

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Contract renewal (T47g, migration 000066): a draft successor continues
// a contract from the day after it ends, with its legal entity, supplier,
// currency and items; one live renewal at a time.

func init() {
	problem.RegisterConstraint("contract_renewed_once", func() *problem.Problem {
		return problem.Conflict("procurement.contract_renewed", "The contract has a renewal already; change or cancel that one.")
	})
	problem.RegisterConstraint("contract_renewal_continues", func() *problem.Problem {
		return problem.Unprocessable("procurement.invalid_renewal", "A renewal starts the day after the contract it renews ends.",
			problem.FieldError{Field: "valid_from", Code: "invalid", Message: "the day after the renewed contract ends"})
	})
}

// renewContract drafts the renewal of an active or expired contract,
// locked so that two renewals queue and the second is refused: valid from
// the day after it ends to validTo, for the target, with its items checked
// again as a new contract's are.
func (e *Endpoints) renewContract(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, validTo time.Time, target, targetCurrency string) (Contract, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	c, err := loadContract(ctx, tx, id, ifMatch, "renew", Active, Expired)
	if err != nil {
		return Contract{}, err
	}
	// The renewal continues the contract as what it is: a scheduling
	// agreement's renewal is one (ADR-0070 §1); its schedule starts empty.
	return e.draftContract(ctx, NewContract{Kind: c.Kind, LegalEntityID: c.LegalEntityID, SupplierID: c.SupplierID, Currency: c.Currency,
		ValidFrom: c.ValidTo.AddDate(0, 0, 1), ValidTo: validTo, Target: target, TargetCurrency: targetCurrency, Items: contractItemInputs(c)}, &c.ID)
}

// contractItemInputs are a contract's items as input, for its renewal.
func contractItemInputs(c Contract) []ItemInput {
	in := make([]ItemInput, len(c.Items))
	for i, it := range c.Items {
		in[i] = ItemInput{MaterialID: it.MaterialID, MaterialKey: it.MaterialKey, Description: it.Description, Unit: it.Unit,
			Price: money.Minor(it.UnitPrice).Plain(c.Exponent), Currency: c.Currency, PriceUnit: it.PriceUnit, AccountID: it.AccountID}
	}
	return in
}
