// Package taxfiling is the tax filing context (ADR-0049 §10-§11 and
// Alternatives; 03-data-model §3.20) in its own schema, tax: the
// companies' registrations with tax authorities and their filing periods,
// and the returns that T61h-T61i add. The pure tax calculator is the
// shared kernel's internal/tax; this context owns what is filed.
//
// Setup keeps the configuration's companies[].tax_registrations in
// tax.registration (T61g): the number sealed with the tenant's key
// (fieldcrypt, as tax identifiers are) and shown by its last four
// characters. What a registration's periods and returns follow from (its
// companies, authority, return type, frequency, first period and number)
// never changes under its code: a changed one is a registration of a new
// code, the old one ended by its last_period_end. So the periods follow
// from the registration alone and are computed on read
// (config.TaxPeriods), never stored. A registration change is a governed
// configuration change (tier 3), audited and published by the
// configuration context's activation (ADR-0011, ADR-0007); this context
// adds no event of its own until returns move (T61i). Every read runs in
// the request's transaction under row-level security (ADR-0003), and a
// caller sees the registrations covering a company they may act in.
package taxfiling

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func init() {
	fieldcrypt.Register(fieldcrypt.Column{Table: "tax.registration", Keys: []string{"id"}, Cipher: "number_cipher",
		Fingerprint: "number_fingerprint", Versioned: true})
	// The trigger backs up Setup's own check for any other writer; the
	// number, which the database cannot open, is Setup's alone.
	problem.RegisterConstraint("registration_fixed", func() *problem.Problem {
		return registrationFixed("A tax registration keeps its company, group, authority, return type, frequency and first period.")
	})
}

// LegalEntities reads legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
	EntityByCode(ctx context.Context, code string) (tenancy.Entity, bool, error)
}

// Endpoints implements the tax filing part of the generated API.
type Endpoints struct {
	Pages         *pagination.Codec
	LegalEntities LegalEntities
	// Settings reads the active configuration, for a company's time zone.
	Settings config.Documents
	Now      func() time.Time
}

// registrationFixed is the refusal of a configuration that changes what a
// registration's periods and returns follow from.
func registrationFixed(detail string) *problem.Problem {
	return problem.Conflict("tax.registration_fixed", detail+
		" Give the changed registration a new code, and end the old one with its last_period_end.")
}
