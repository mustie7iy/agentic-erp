package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// AssignmentSimulation is the simulation evidence of a change to the
// controlling rules (ADR-0050 §4, T56c): it replays the journal lines of
// the last Window on the P&L accounts whose assignment the proposal
// changes (an account's cost element, a default cost center, center
// accounting, the default profit center) and lists, per company and
// account, how lines would be assigned before and after, and which would
// now be refused for a missing cost center. Until journal lines carry
// centers (T56d) no line has one entered, so every line on an account that
// needs a cost center without a default is counted as refused. It reads
// committed lines of the leading ledger in the request's transaction and
// writes nothing.
type AssignmentSimulation struct {
	LegalEntities LegalEntities
	Window        time.Duration // how far back lines are replayed; default 90 days
	Now           func() time.Time
}

// assignmentsKey is the simulation detail's key for what replaying each
// company's lines found (CompanyAssignments).
const assignmentsKey = "journal_lines_by_company"

// CompanyAssignments is what replaying one company's lines found, recorded
// per company so each reader is shown the companies they may access alone
// (AssignmentDisclosure).
type CompanyAssignments struct {
	Company       string          `json:"company"`
	LegalEntityID uuid.UUID       `json:"legal_entity_id"`
	Accounts      []AccountReplay `json:"accounts"`
}

// AccountReplay is an account whose lines the proposal assigns
// differently, with the lines of the window on it.
type AccountReplay struct {
	Account  string `json:"account"`
	Base     string `json:"base"`     // how a line is assigned under the base configuration
	Proposed string `json:"proposed"` // and under the proposal
	// Refused says a line without a cost center entered is refused under
	// the proposal and was not before.
	Refused  bool     `json:"refused"`
	Lines    int      `json:"lines"`
	Entries  int      `json:"entries"`
	Examples []string `json:"examples"` // the first entries' document types and numbers
}

// Assignment outcomes of a line, by what it needs.
const (
	ccNone     = "no cost center"
	ccRequired = "cost center required"
	pcNone     = "no profit center"
	pcFromCC   = "profit center of the cost center"
)

// assignment is how a line on the account, without a center entered, is
// assigned under the company's rules (ADR-0050 §5's table); "" for a
// balance sheet account or one not in the chart, which nothing derives.
func assignment(co *config.Company, number string) string {
	a := co.Account(number)
	if a == nil || a.Type.BalanceSheet() {
		return ""
	}
	ce := a.CostElementOrDefault()
	cc, pc := ccNone, pcNone
	if ce == config.CostElementPrimaryCost && co.CostCenterAccounting {
		cc = ccRequired
		if d := co.DefaultAssignment(number); d != nil {
			cc = "default cost center " + d.CostCenter
		}
	}
	switch {
	case !co.ProfitCenterAccounting:
	case cc != ccNone:
		pc = pcFromCC
	default:
		pc = "default profit center " + co.DefaultProfitCenter
	}
	return cc + "; " + pc
}

// changedAccounts are the company's P&L accounts whose lines the proposal
// assigns differently, with how before and after.
func changedAccounts(before, after *config.Company) map[string][2]string {
	out := map[string][2]string{}
	for _, a := range after.ChartOfAccounts {
		was, is := assignment(before, a.Number), assignment(after, a.Number)
		if was != "" && is != "" && was != is {
			out[a.Number] = [2]string{was, is}
		}
	}
	return out
}

// recordedAssignmentSummary is the simulation's summary as recorded: no
// count, since counts over every company would tell a reader of narrower
// access that other companies' lines change.
func recordedAssignmentSummary(days int) string {
	return fmt.Sprintf("Journal lines of the last %d days replayed under the proposed controlling rules.", days)
}

// Simulate implements configuration.Simulator; a proposal that changes no
// assignment records nothing.
func (s AssignmentSimulation) Simulate(ctx context.Context, base, proposed *config.Config) (configuration.Simulation, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return configuration.Simulation{}, err
	}
	window := s.Window
	if window <= 0 {
		window = 90 * 24 * time.Hour
	}
	since := day(clock.Now(s.Now).Add(-window))
	var companies []CompanyAssignments
	for _, co := range proposed.Companies {
		was := base.Company(co.Code)
		if was == nil {
			continue // a new company has no lines yet
		}
		changed := changedAccounts(was, &co)
		if len(changed) == 0 {
			continue
		}
		replay, err := s.replay(ctx, tx, co.Code, since, changed)
		if err != nil {
			return configuration.Simulation{}, err
		}
		companies = append(companies, replay)
	}
	if len(companies) == 0 {
		return configuration.Simulation{Passed: true}, nil
	}
	days := int(window.Hours() / 24)
	return configuration.Simulation{Passed: true, Summary: recordedAssignmentSummary(days),
		Detail: map[string]any{"assignment_window_days": days, assignmentsKey: companies}}, nil
}

// replay counts the company's lines since the date on the changed
// accounts, by account.
func (s AssignmentSimulation) replay(ctx context.Context, tx pgx.Tx, company string, since time.Time, changed map[string][2]string) (CompanyAssignments, error) {
	// A company without a legal entity (Check reports it) has no lines:
	// the query finds none for the nil id.
	le, _, err := s.LegalEntities.EntityByCode(ctx, company)
	if err != nil {
		return CompanyAssignments{}, err
	}
	out := CompanyAssignments{Company: company, LegalEntityID: le.ID, Accounts: []AccountReplay{}}
	numbers := []string{}
	for n := range maps.Keys(changed) {
		numbers = append(numbers, n)
	}
	rows, _ := tx.Query(ctx, `SELECT g.number, count(*), count(DISTINCT (l.fiscal_year, l.journal_entry_id)),
			(array_agg(DISTINCT e.document_type || ' ' || e.document_number ORDER BY e.document_type || ' ' || e.document_number))[1:5]
		FROM ledger.journal_line l
		JOIN ledger.ledger d ON d.tenant_id = l.tenant_id AND d.id = l.ledger_id AND d.is_leading
		JOIN ledger.gl_account g ON g.tenant_id = l.tenant_id AND g.id = l.account_id
		JOIN ledger.chart_of_accounts c ON c.tenant_id = g.tenant_id AND c.id = g.chart_id AND c.code = $2
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE l.legal_entity_id = $1 AND l.posting_date >= $3 AND g.number = ANY($4)
		GROUP BY 1 ORDER BY 1`, le.ID, company, since, numbers)
	var r AccountReplay
	_, err = pgx.ForEachRow(rows, []any{&r.Account, &r.Lines, &r.Entries, &r.Examples}, func() error {
		how := changed[r.Account]
		r.Base, r.Proposed = how[0], how[1]
		r.Refused = strings.HasPrefix(how[1], ccRequired) && !strings.HasPrefix(how[0], ccRequired)
		out.Accounts = append(out.Accounts, r)
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("ledger: simulate assignments of company %s: %w", company, err)
	}
	return out, nil
}

// AssignmentDisclosure shows the reader of a change request the replayed
// lines of the companies they may access alone, with the summary's counts
// over those (ADR-0010, as payables.Disclosure does for invoices). It fails
// closed: a reader of no company is shown no account and counts of zero.
type AssignmentDisclosure struct{}

// Disclose implements configuration.Disclosure.
func (AssignmentDisclosure) Disclose(ctx context.Context, e configuration.Evidence) (configuration.Evidence, error) {
	if e.Kind != "simulation" {
		return e, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.Detail, &fields); err != nil {
		return configuration.Evidence{}, fmt.Errorf("ledger: simulated assignments: %w", err)
	}
	if fields[assignmentsKey] == nil {
		return e, nil // another context's simulation
	}
	var rec struct {
		Days      int                  `json:"assignment_window_days"`
		Companies []CompanyAssignments `json:"journal_lines_by_company"`
	}
	if err := json.Unmarshal(e.Detail, &rec); err != nil {
		return configuration.Evidence{}, fmt.Errorf("ledger: simulated assignments: %w", err)
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return configuration.Evidence{}, fmt.Errorf("ledger: simulated assignments: %w", err)
	}
	shown := []CompanyAssignments{}
	lines, refused, accounts := 0, 0, 0
	for _, c := range rec.Companies {
		if !access.Allows(c.LegalEntityID) {
			continue
		}
		shown = append(shown, c)
		for _, a := range c.Accounts {
			accounts, lines = accounts+1, lines+a.Lines
			if a.Refused {
				refused += a.Lines
			}
		}
	}
	// A slice of structs of strings, numbers and booleans marshals without
	// fail, and so does a map of raw values just read.
	fields[assignmentsKey], _ = json.Marshal(shown)
	e.Detail, _ = json.Marshal(fields)
	e.Summary = strings.Replace(e.Summary, recordedAssignmentSummary(rec.Days), fmt.Sprintf(
		"%d journal lines of the last %d days on %d accounts would be assigned differently under the proposed controlling rules, %d of them refused without a cost center.",
		lines, rec.Days, accounts, refused), 1)
	return e, nil
}
