// Package leases is the lease accounting context (ADR-0073; 02-architecture
// §2.2, 03-data-model §3.32): the lease contracts a company enters as
// lessee, each with its payment schedule (contracts.go, schedule.go).
//
// The person makes every judgement and the system only does the
// arithmetic (ADR-0073 §2): that a contract is a lease, its term, its
// discount rate and its payments are a person's to decide, and nothing
// here judges them. Every contract entered is capitalized; no exemption
// for short-term or low-value leases is applied.
//
// The context is a leaf (ADR-0073 §1; ADR-0051 §2): it reads tenancy's
// legal entities, the partner context's suppliers, the ledger's cost
// centers and calendar and the configuration's number ranges and
// company dates through the ports below, which the composition root
// wires, and no context imports it.
//
// What the context takes IFRS 16 to say is ADR-0073 §2's six statements,
// each from memory and not yet confirmed by the accounting specialist.
package leases

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Draft is the status of a lease contract when it is created (ADR-0073
// §3).
const Draft = "draft"

// ContractRange is the document type lease contracts draw their numbers
// from (configuration number_ranges).
const ContractRange = "lease_contract"

// Payment frequencies, each with the months of its interval (ADR-0073 §3).
// The enumeration is open: a later version may add to it.
var frequencies = map[string]int{"monthly": 1, "quarterly": 3, "yearly": 12}

// Payment timings: due on the first day of each interval or on its last.
const (
	Advance = "advance"
	Arrears = "arrears"
)

// LegalEntities is what leases reads about legal entities (tenancy
// context): a company's code, name and local currency.
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Lessors reads a business partner as a supplier of a legal entity
// (partner context), the check procurement and payables make.
type Lessors interface {
	Supplier(ctx context.Context, partnerID, legalEntity uuid.UUID) (partner.Supplier, bool, error)
}

// Partners names business partners by number and name (partner context).
type Partners interface {
	Names(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]partner.Named, error)
}

// CostCenters reads the ledger's cost centers: their companies, codes and
// whether they are active, and whether one is in force on a day.
type CostCenters interface {
	CostCenterRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ledger.CostCenterRef, error)
	CostCenterValidOn(ctx context.Context, id uuid.UUID, day time.Time) (bool, error)
}

// Numbers issues document numbers (configuration context).
type Numbers interface {
	Next(ctx context.Context, legalEntity uuid.UUID, documentType string, fiscalYear int16) (string, error)
}

// Calendar maps dates to fiscal years (ledger context).
type Calendar interface {
	FiscalYear(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int16, error)
}

// Settings reads the active configuration: a company's time zone
// (configuration context).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
}

// Endpoints implements the leases part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	Lessors       Lessors
	Partners      Partners
	CostCenters   CostCenters
	Numbers       Numbers
	Calendar      Calendar
	Settings      Settings
	// Now is the clock a contract is numbered by, in its company's time
	// zone; nil: the wall clock.
	Now func() time.Time
}

// record adds the audit record of a contract's change and publishes
// LeaseContractChanged in the same transaction (ADR-0007, ADR-0011);
// before is its status before, nil when it had none. The event names the
// lessor by id and number, never by name (product owner, 2026-11-05), and
// carries each payment's amount, not the payments.
func record(ctx context.Context, action string, c Contract, le tenancy.Entity, lessorNumber string, before *string) error {
	amount := money.EventAmount(c.PaymentAmount, c.Currency, c.Exponent)
	day := c.CommencementDate.Format(time.DateOnly)
	payload := map[string]any{"lease_contract_id": c.ID, "document_number": c.DocumentNumber, "legal_entity_id": c.LegalEntityID,
		"legal_entity_code": le.Code, "lessor_id": c.LessorID, "lessor_number": lessorNumber, "commencement_date": day, "term_months": c.TermMonths,
		"payment_amount": amount, "payment_frequency": c.Frequency, "payment_timing": c.Timing, "payment_count": len(c.Payments),
		"status": c.Status, "status_before": before, "version": c.Version}
	detail := map[string]any{"document_number": c.DocumentNumber, "lessor_id": c.LessorID, "commencement_date": day, "term_months": c.TermMonths,
		"payment_amount": amount, "payment_frequency": c.Frequency, "payment_timing": c.Timing, "annual_discount_rate_percent": c.AnnualRate,
		"cost_center_id": c.CostCenterID, "status": c.Status, "status_before": before}
	if err := audit.Record(ctx, action, "leases.contract", c.ID, detail,
		outbox.Event{Context: "leases", Name: "lease_contract_changed", AggregateType: "leases.contract", AggregateID: c.ID, Payload: payload}); err != nil {
		return fmt.Errorf("leases: record %s of contract %s: %w", action, c.ID, err)
	}
	return nil
}
