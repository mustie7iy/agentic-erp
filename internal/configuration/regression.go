package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// RegressionCase is one case of a suite: an invoice and the decision it
// got when the suite was captured.
type RegressionCase struct {
	CaseNo           int32     `json:"case_no"`
	InvoiceID        uuid.UUID `json:"supplier_invoice_id"`
	DocumentNumber   string    `json:"document_number"`
	LegalEntityID    uuid.UUID `json:"legal_entity_id"`
	Company          string    `json:"company"`
	Why              string    `json:"why"`
	ExpectedDecision string    `json:"expected_decision"`
	ExpectedIssues   []string  `json:"expected_issues"`
}

// RegressionFailure is a case decided differently on replay.
type RegressionFailure struct {
	Case     RegressionCase `json:"case"`
	Decision string         `json:"decision"`
	Issues   []string       `json:"issues"`
}

// Regressor captures and replays regression cases in the request's
// transaction, reading committed data and writing nothing (the payables
// context implements it).
type Regressor interface {
	Capture(ctx context.Context, doc *config.Config, window time.Duration, maxCases int) ([]RegressionCase, error)
	Replay(ctx context.Context, doc *config.Config, cases []RegressionCase) ([]RegressionFailure, error)
}

// RegressionSuite is a row of config.regression_suite with its cases.
type RegressionSuite struct {
	ID            uuid.UUID
	Number        string
	Name          string
	ConfigVersion int32
	WindowDays    int32
	CaseCount     int32
	Strata        []string
	Cases         []RegressionCase
	CapturedAt    time.Time
	Version       int32
	CreatedAt     time.Time
	CreatedBy     uuid.UUID
	UpdatedAt     time.Time
	UpdatedBy     uuid.UUID
}

const suiteSelect = `SELECT s.id, s.number, s.name, s.config_version, s.window_days, s.case_count, s.strata, s.captured_at, s.version,
		s.created_at, s.created_by, s.updated_at, s.updated_by,
		coalesce((SELECT json_agg(json_build_object('case_no', c.case_no, 'supplier_invoice_id', c.supplier_invoice_id, 'document_number', c.document_number,
			'legal_entity_id', c.legal_entity_id, 'company', c.company, 'why', c.why, 'expected_decision', c.expected_decision,
			'expected_issues', c.expected_issues) ORDER BY c.case_no)
			FROM config.regression_case c WHERE c.tenant_id = s.tenant_id AND c.suite_id = s.id), '[]')
	FROM config.regression_suite s`

func scanSuite(row pgx.Row) (RegressionSuite, error) {
	var s RegressionSuite
	err := row.Scan(&s.ID, &s.Number, &s.Name, &s.ConfigVersion, &s.WindowDays, &s.CaseCount, &s.Strata, &s.CapturedAt, &s.Version,
		&s.CreatedAt, &s.CreatedBy, &s.UpdatedAt, &s.UpdatedBy, &s.Cases)
	return s, errs.Wrapf(err, "scan regression suite")
}

// NewRegressionSuite is what a capture asks for.
type NewRegressionSuite struct {
	Name       string
	MaxCases   int // default 50
	WindowDays int // default 90
}

// CaptureRegressionSuite captures a suite under the active configuration
// version. A window without invoices is 422 config.no_cases.
func CaptureRegressionSuite(ctx context.Context, deps Dependencies, in NewRegressionSuite) (RegressionSuite, error) {
	tx, actor, err := reqctx.Request(ctx)
	if err != nil {
		return RegressionSuite{}, err
	}
	if in.MaxCases <= 0 {
		in.MaxCases = 50
	}
	if in.WindowDays <= 0 {
		in.WindowDays = 90
	}
	number, err := activeVersion(ctx, tx)
	if err != nil {
		return RegressionSuite{}, err
	}
	doc, err := versionDocument(ctx, tx, number)
	if err != nil {
		return RegressionSuite{}, err
	}
	cases, err := deps.Regressor.Capture(ctx, doc, time.Duration(in.WindowDays)*24*time.Hour, in.MaxCases)
	if err != nil {
		return RegressionSuite{}, err
	}
	if len(cases) == 0 {
		return RegressionSuite{}, problem.Unprocessable("config.no_cases", fmt.Sprintf("No supplier invoices on purchase orders in the last %d days to capture.", in.WindowDays))
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM config.regression_suite`).Scan(&n); err != nil {
		return RegressionSuite{}, fmt.Errorf("configuration: count regression suites: %w", err)
	}
	id := uuid.Must(uuid.NewV7())
	strata, _ := json.Marshal(strataOf(cases)) // a string slice always encodes
	if _, err := tx.Exec(ctx, `INSERT INTO config.regression_suite (tenant_id, id, number, name, config_version, window_days, case_count, strata, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`, actor.TenantID, id, fmt.Sprintf("RS-%04d", n+1), in.Name, number, in.WindowDays, len(cases), strata, actor.ID); err != nil {
		return RegressionSuite{}, fmt.Errorf("configuration: insert regression suite %s: %w", id, err)
	}
	for _, c := range cases {
		issues, _ := json.Marshal(c.ExpectedIssues) // a string slice always encodes
		if _, err := tx.Exec(ctx, `INSERT INTO config.regression_case (tenant_id, suite_id, case_no, supplier_invoice_id, document_number, legal_entity_id, company,
			why, expected_decision, expected_issues) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			actor.TenantID, id, c.CaseNo, c.InvoiceID, c.DocumentNumber, c.LegalEntityID, c.Company, c.Why, c.ExpectedDecision, issues); err != nil {
			return RegressionSuite{}, fmt.Errorf("configuration: insert case %d of regression suite %s: %w", c.CaseNo, id, err)
		}
	}
	s, err := getSuite(ctx, tx, id)
	if err != nil {
		return RegressionSuite{}, err
	}
	// The audit entry counts no cases: they are of every company, and the
	// audit trail reaches readers of fewer (the suite shows each reader the
	// cases of their own, suiteShown).
	return s, audit.Add(ctx, audit.Entry{Action: "config.regression_suite.capture", ObjectType: "config.regression_suite", ObjectID: id, Outcome: "succeeded",
		Detail: map[string]any{"number": s.Number, "config_version": number, "window_days": in.WindowDays}})
}

// strataOf lists why cases were selected, in order of first use.
func strataOf(cases []RegressionCase) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range cases {
		for _, why := range splitWhy(c.Why) {
			if !seen[why] {
				seen[why] = true
				out = append(out, why)
			}
		}
	}
	return out
}

func splitWhy(why string) []string {
	var out []string
	for _, part := range regexp.MustCompile(`;\s*`).Split(why, -1) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getSuite(ctx context.Context, tx pgx.Tx, id uuid.UUID) (RegressionSuite, error) {
	s, err := scanSuite(tx.QueryRow(ctx, suiteSelect+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RegressionSuite{}, problem.NotFoundOf("regression suite")
	}
	if err != nil {
		return s, fmt.Errorf("configuration: read regression suite %s: %w", id, err)
	}
	return s, nil
}

// GetRegressionSuite returns a suite with all its cases; the API shows each
// reader the cases of their own companies alone (suiteShown).
func GetRegressionSuite(ctx context.Context, id uuid.UUID) (RegressionSuite, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return RegressionSuite{}, err
	}
	return getSuite(ctx, tx, id)
}

// latestSuite is the newest suite captured on a configuration version, or
// ok = false when there is none.
func latestSuite(ctx context.Context, tx pgx.Tx, version int32) (RegressionSuite, bool, error) {
	s, err := scanSuite(tx.QueryRow(ctx, suiteSelect+` WHERE s.config_version = $1 ORDER BY s.captured_at DESC, s.id DESC LIMIT 1`, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return RegressionSuite{}, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("configuration: read the latest regression suite of version %d: %w", version, err)
	}
	return s, true, nil
}

var companyPath = regexp.MustCompile(`^companies\[([^\]]+)\]`)

// touched lists the company codes a change request's items change; items
// outside companies (payment terms, purchasing, payments) touch none.
func touched(c ChangeRequest) map[string]bool {
	out := map[string]bool{}
	for _, it := range c.Items {
		if m := companyPath.FindStringSubmatch(it.Path); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// regression is the regression evidence of a change request: the latest
// suite of its base version replayed under the proposal. It passes when the
// suite still matches the base version and no case outside the companies
// the change touches is decided differently; differences inside them are
// the change at work, listed for the approvers. The cases are recorded by
// company and the summary with no count (regressionRecord), so that each
// reader is shown those of their own companies alone (regressionShown).
func regression(ctx context.Context, tx pgx.Tx, deps Dependencies, c ChangeRequest, base, proposed *config.Config) (Simulation, error) {
	suite, ok, err := latestSuite(ctx, tx, c.BaseVersion)
	if err != nil {
		return Simulation{}, err
	}
	if !ok {
		return Simulation{Passed: false, Summary: fmt.Sprintf("No regression suite captured on version %d; capture one (createRegressionSuite) and simulate again.", c.BaseVersion),
			Detail: map[string]any{"config_version": c.BaseVersion}}, nil
	}
	rec := newRegressionRecord(suite, c.BaseVersion)
	inScope := touched(c)
	stale, err := deps.Regressor.Replay(ctx, base, suite.Cases)
	if err != nil {
		return Simulation{}, err
	}
	if len(stale) > 0 {
		rec.StaleSuite = true
		for _, f := range stale {
			r := rec.company(f.Case)
			r.Stale = append(r.Stale, f)
		}
		return rec.result(false, keys(inScope)), nil
	}
	failures, err := deps.Regressor.Replay(ctx, proposed, suite.Cases)
	if err != nil {
		return Simulation{}, err
	}
	passed := true
	for _, f := range failures {
		r := rec.company(f.Case)
		if inScope[f.Case.Company] {
			r.Changed = append(r.Changed, f)
		} else {
			r.Regressions, passed = append(r.Regressions, f), false
		}
	}
	return rec.result(passed, keys(inScope)), nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
