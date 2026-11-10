// Package consolidation is the group reporting and consolidation context
// (ADR-0059; SAP's Group Reporting): consolidation groups, their runs per
// regular period, the members' translated balances, intercompany matching
// and eliminations, the investment's elimination with goodwill and
// non-controlling interests, and the consolidated statements, full
// consolidation only. It is a leaf (ADR-0059 §2): it reads the other
// contexts only through ports it declares, which the composition root
// (internal/app) adapts and which run in the request's transaction under
// row-level security (ADR-0003), and no context imports it
// (internal/layout).
//
// T68a builds its configuration and reads: the group chart, the
// eliminations, the groups and each company account's group account are
// configuration (ADR-0059 §3, internal/config), validated there and, for
// what needs the tenant's data (the group currency), by Setup, the
// context's section of the configuration. ListConsolidationGroups,
// GetConsolidationGroup and ListGroupAccounts answer them from the active
// document through the Settings and Companies ports. A group is visible
// only to a caller who may act in its parent and every member (ADR-0059
// §12): its figures are only meaningful whole, so a group outside that is
// unknown, never shown in part.
//
// T68d adds runs and currency translation (ADR-0059 §5-§7): a run of a
// group and regular period is queued by CreateConsolidationRun with its
// Operation and carried out by the workflow ConsolidationRun (ADR-0012)
// through the workflows.Consolidation port, which Endpoints implements:
// PrepareConsolidationRun, TranslateMember per company,
// EliminateConsolidationRun, CompleteConsolidationRun and
// FailConsolidationRun, each idempotent. Each company's leading-ledger
// balances are read through the Ledger port and translated into the group
// currency (translate).
//
// T68e adds the consolidated statements (ADR-0059 §11,
// GetConsolidatedStatement): a run's balance sheet or profit and loss,
// one row per group account with the companies' translated figures
// summed, the eliminations and the consolidated figure, and the totals,
// read from the run's stored rows alone (§14).
//
// T68f adds intercompany matching and eliminations (ADR-0059 §8,
// eliminate): EliminateConsolidationRun matches each elimination's rows
// per pair of the run's companies within its tolerance, lists the rows
// naming a company outside the run or none, and writes each pair's
// intercompany entry; ListIntercompanyDifferences and
// ListConsolidationEntries read them, and the statements' eliminations
// are the entries' lines.
//
// T68g adds accepting differences and finalization (ADR-0059 §8, §10,
// finalize): a person accepts an open difference of a computed run with a
// reason (AcceptIntercompanyDifference); FinalizeConsolidationRun makes a
// computed run without open differences, whose companies' periods were
// locked when it read them and still are at the versions it recorded, the
// group's final figures of its period, superseding the previous final run
// under a lock of the period's runs. A run is stale once a company's
// period version moved since it read its figures. The investment's
// elimination follows in T68h.
package consolidation

import (
	"context"
	"math/big"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Settings reads the active configuration (configuration context), whose
// group chart and groups the context reads, and an activated version's,
// which a run reads (the version it recorded).
type Settings interface {
	Document(ctx context.Context) (*config.Config, int32, error)
	Version(ctx context.Context, number int32) (*config.Config, error)
}

// Company is a legal entity as the consolidation context reads it: its
// code, name, local currency and fiscal calendar.
type Company struct {
	ID               uuid.UUID
	Code             string
	Name             string
	LocalCurrency    string
	FiscalCalendarID uuid.UUID
}

// Companies reads the tenant's legal entities and its group currency
// (tenancy context, ADR-0059 §2). Company access is not applied: the
// context applies it, to a group as a whole.
type Companies interface {
	Entities(ctx context.Context) ([]Company, error)
	GroupCurrency(ctx context.Context) (string, error)
}

// Period is a company's posting period in the leading ledger: its dates,
// status and version, and its pending unlock request, if any (ADR-0059
// §4a, PeriodOf).
type Period struct {
	Start, End      time.Time
	Status          string
	Version         int32
	UnlockRequestID *uuid.UUID
}

// Account is a company's account: its number, name and type.
type Account struct {
	ID           uuid.UUID
	Number, Name string
	Type         config.AccountType
}

// Balance is a company's leading-ledger balance of an account through a
// period, of the lines naming the trading partner or, nil, none, in local
// currency and at its booked group amounts (ADR-0059 §4a,
// BalancesThrough): a balance sheet account's over every fiscal year
// (Period 0), a profit and loss account's per earlier fiscal year (Period
// 0) and per period of the period's year.
type Balance struct {
	AccountID          uuid.UUID
	TradingPartnerID   *uuid.UUID
	FiscalYear, Period int16
	LC, GC             money.Minor
}

// RevaluationLine is a line of one of a company's revaluation runs'
// entries (ADR-0042) on a profit and loss account, in its leading ledger
// (ADR-0059 §4a, BalancesThrough): its local and booked group amounts and
// the local-to-group rate its entry records, the posting date's, with the
// fiscal year and period of a profit and loss Balance (Period 0 in an
// earlier fiscal year).
type RevaluationLine struct {
	AccountID          uuid.UUID
	TradingPartnerID   *uuid.UUID
	FiscalYear, Period int16
	LC, GC             money.Minor
	Rate               *big.Rat
}

// Rate is a rate valid on a date (ADR-0042's rule): the rate, its
// 12-decimal form and the date it is valid from.
type Rate struct {
	Rate      *big.Rat
	Shown     string
	ValidFrom time.Time
}

// Ledger reads the ledger context's figures (ADR-0059 §4a), each in its
// leading ledger, which consolidation reads alone (§1).
type Ledger interface {
	// PeriodOf is the company's period; ok is false when it has none.
	PeriodOf(ctx context.Context, company uuid.UUID, year, period int16) (Period, bool, error)
	// PeriodsOf are the companies' periods by company, read at once; a
	// company without the period is absent.
	PeriodsOf(ctx context.Context, companies []uuid.UUID, year, period int16) (map[uuid.UUID]Period, error)
	Accounts(ctx context.Context, company uuid.UUID) ([]Account, error)
	// BalancesThrough are the company's balances through the period and,
	// read with them from one database state, the lines of its
	// revaluation runs' entries on profit and loss accounts through it.
	BalancesThrough(ctx context.Context, company uuid.UUID, year, period int16) ([]Balance, []RevaluationLine, error)
	// RateOn is the latest rate of the type valid on the date; ok is
	// false when there is none.
	RateOn(ctx context.Context, rateType, from, to string, date time.Time) (Rate, bool, error)
}

// People names principals (identity context, ADR-0059 §2): who finalized
// a run and who accepted a difference. An id the tenant has no principal
// of is absent.
type People interface {
	Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Runs keeps a run's Operation (operations, 04 §4.8).
type Runs interface {
	Start(ctx context.Context, resource string, request any) (gen.Operation, error)
	Report(ctx context.Context, id uuid.UUID, done, total int64) error
	Cancelled(ctx context.Context, id uuid.UUID, lock bool) (bool, error)
	Succeed(ctx context.Context, id uuid.UUID, resource string, result any) error
	Fail(ctx context.Context, id uuid.UUID, p *problem.Problem) error
}

// Endpoints are the consolidation context's operations.
type Endpoints struct {
	Settings  Settings
	Companies Companies
	Ledger    Ledger
	People    People
	Runs      Runs
	Pages     *pagination.Codec

	versions versionCache // the configuration versions runs read, parsed
}

// groupUnknown is the answer for a group that does not exist or that the
// caller may not see whole (ADR-0059 §12), so neither its existence nor
// its companies leak.
func groupUnknown(code string) *problem.Problem {
	return problem.New(http.StatusNotFound, "consolidation.group_unknown", "Not found",
		"There is no consolidation group "+code+" whose parent and members you may all act in.")
}

// companiesByCode are the tenant's legal entities by company code.
func (e *Endpoints) companiesByCode(ctx context.Context) (map[string]Company, error) {
	all, err := e.Companies.Entities(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Company, len(all))
	for _, c := range all {
		out[c.Code] = c
	}
	return out, nil
}

// visible reports whether the caller may see the group: its parent and
// every member a legal entity the caller may act in. A company of the
// document without a legal entity cannot be in an activated document
// (ledger.Setup), and would hide the group rather than show it in part.
func visible(g config.ConsolidationGroup, companies map[string]Company, access authz.Access) bool {
	codes := []string{g.Parent}
	for _, m := range g.Members {
		codes = append(codes, m.Company)
	}
	for _, code := range codes {
		le, ok := companies[code]
		if !ok || !access.Allows(le.ID) {
			return false
		}
	}
	return true
}
