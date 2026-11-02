package partner

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Migration loads business partners for the integration context's
// migration runs. The run itself is the governed act (a reconciled load
// started by a person with integration.migration.run), so the changes it
// makes are applied at once instead of waiting for approval requests.
type Migration struct {
	Ports Ports
}

// backdating is how many days before partner's own UTC day
// (database.Today) a new bank account may start. It is unexported and no
// input type holds it, so no request can set it: CreatePartner,
// RequestBankAccount and ApplyChange always pass noBackdating, and only
// Migration.Create passes migrationBackdating. The one route to it is a
// migration run (integration.migration.run, a person's or an agent's),
// for the suppliers it creates, on the day the run computes (product
// owner 2026-11-02; ADR-0051 As built).
type backdating int

const (
	// noBackdating is the backdating control
	// (partner.backdated_bank_account): an account starts on UTC's day or
	// later.
	noBackdating backdating = 0
	// migrationBackdating lets the bank accounts of a partner a migration
	// run creates start on UTC's previous day, so that on a cutover evening
	// in Toronto, when UTC is already tomorrow, a payment run dated on the
	// company's day finds them. One day, never more.
	migrationBackdating backdating = 1
)

// earliest is the first day a new bank account may start when partner's
// day is day.
func (b backdating) earliest(day time.Time) time.Time { return day.AddDate(0, 0, -int(b)) }

// allowed says in a refusal which first days are accepted.
func (b backdating) allowed() string {
	if b == noBackdating {
		return "today or later"
	}
	return "yesterday (UTC) or later"
}

// Create creates the partner and applies its create change, the one this
// call made: the migration's backdating reaches only a partner it creates
// here, never one that exists.
func (m Migration) Create(ctx context.Context, in NewPartner) (uuid.UUID, error) {
	bp, c, err := createPartner(ctx, m.Ports, in, migrationBackdating)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = applyChange(ctx, m.Ports.Schemas, c.ID, migrationBackdating)
	return bp.ID, err
}

// AddCompanies sets the partner up as a supplier of more legal entities.
func (m Migration) AddCompanies(ctx context.Context, id uuid.UUID, companies []SupplierCompanyInput, reason string) error {
	c, err := RequestUpdate(ctx, m.Ports, id, Changes{SupplierCompanies: companies}, reason)
	if err != nil {
		return err
	}
	_, err = ApplyChange(ctx, m.Ports.Schemas, c.ID)
	return err
}

// Block blocks the partner.
func (Migration) Block(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := Block(ctx, id, func(int32) error { return nil }, reason)
	return err
}
