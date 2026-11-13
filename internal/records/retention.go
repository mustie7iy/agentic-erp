package records

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Year is a fiscal year of a company with the day its retention ends
// under the rule, if there is one, and whether it is past.
type Year struct {
	FiscalYear    int16
	Start, End    time.Time
	RetentionEnds *time.Time
	Past          bool
}

// Status is the retention status of a company's documents of one kind
// (ADR-0079 §3): its rule's years, nil without a rule, the configuration
// version it was read from, the company's date, and its fiscal years.
type Status struct {
	Company tenancy.Entity
	Kind    string
	Years   *int
	Version int32
	AsOf    time.Time
	Fiscal  []Year
}

// Past is how many of the fiscal years are past retention.
func (s Status) Past() int {
	n := 0
	for _, y := range s.Fiscal {
		if y.Past {
			n++
		}
	}
	return n
}

// kind refuses a document kind the first version has no rules for.
func kind(k string) error {
	if slices.Contains(config.RetentionKinds, k) {
		return nil
	}
	list := strings.Join(config.RetentionKinds, ", ")
	return problem.Unprocessable("request.validation_failed", "The request is not valid: document_kind is one of "+list+".",
		problem.FieldError{Field: "document_kind", Code: "invalid", Message: "one of " + list})
}

// StatusOf is the retention status of a company's documents of the kind
// on the company's date today (ADR-0079 §3): each fiscal year set up for
// it, the day its retention ends under the company's rule in the active
// configuration and whether that day is past. Without a rule, nothing is
// past. A legal entity the caller cannot act in is not found.
func (e *Endpoints) StatusOf(ctx context.Context, legalEntity uuid.UUID, k string) (Status, error) {
	if _, _, err := reqctx.Request(ctx); err != nil {
		return Status{}, err
	}
	if err := kind(k); err != nil {
		return Status{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("records: company access for the retention status: %w", err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, legalEntity)
	if err != nil {
		return Status{}, fmt.Errorf("records: company %s of the retention status: %w", legalEntity, err)
	}
	if !ok || !access.Allows(legalEntity) {
		return Status{}, problem.NotFoundOf("legal entity") // another company's is not found, so ids cannot be probed
	}
	doc, version, err := e.Settings.Document(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("records: configuration of company %s: %w", le.Code, err)
	}
	today, err := doc.CompanyDay(le.Code, clock.Now(e.Now))
	if err != nil {
		return Status{}, fmt.Errorf("records: date of company %s: %w", le.Code, err)
	}
	years, err := e.Calendar.Years(ctx, le.ID)
	if err != nil {
		return Status{}, fmt.Errorf("records: fiscal years of company %s: %w", le.Code, err)
	}
	s := Status{Company: le, Kind: k, Version: version, AsOf: today.Date, Fiscal: make([]Year, len(years))}
	if co := doc.Company(le.Code); co != nil {
		if r := co.RetentionFor(k); r != nil {
			s.Years = r.Years
		}
	}
	for i, y := range years {
		s.Fiscal[i] = Year{FiscalYear: y.FiscalYear, Start: y.Start, End: y.End}
		if s.Years != nil {
			ends := config.RetentionEnds(y.End, *s.Years)
			s.Fiscal[i].RetentionEnds, s.Fiscal[i].Past = &ends, s.AsOf.After(ends)
		}
	}
	return s, nil
}

// GetRetentionStatus implements GET /v1/retention-status.
func (e *Endpoints) GetRetentionStatus(ctx context.Context, params gen.GetRetentionStatusParams) (gen.GetRetentionStatusRes, error) {
	s, err := e.StatusOf(ctx, params.LegalEntityID, params.DocumentKind)
	if err != nil {
		return nil, err
	}
	out := &gen.RetentionStatus{LegalEntityID: s.Company.ID, CompanyCode: s.Company.Code, CompanyName: s.Company.Name, DocumentKind: s.Kind,
		HasRule: s.Years != nil, ConfigurationVersion: s.Version, AsOf: s.AsOf, YearCount: int32(len(s.Fiscal)), PastRetentionCount: int32(s.Past()),
		Years: make([]gen.RetentionYear, len(s.Fiscal))}
	if s.Years != nil {
		out.RetentionYears.SetTo(int32(*s.Years))
	}
	for i, y := range s.Fiscal {
		out.Years[i] = gen.RetentionYear{FiscalYear: int32(y.FiscalYear), StartDate: y.Start, EndDate: y.End, RetentionEnds: apitypes.OptDate(y.RetentionEnds),
			PastRetention: y.Past}
	}
	return out, nil
}
