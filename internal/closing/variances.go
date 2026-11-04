package closing

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// varianceCursor is a variance report page's position: its last row's
// account number.
type varianceCursor struct {
	Number string `json:"n"`
}

// varianceRow is a row of a variance report: an account's actual (Net),
// its comparison, their variance, the variance in percent of the
// comparison's absolute value (nil when the comparison is zero), whether
// it requires a comment; its accepted comment, whether that is stale (the
// figures it was written against are no longer the row's) and whether it
// explains the row (accepted and not stale); and how many proposals wait.
type varianceRow struct {
	AccountNet
	comparison, variance money.Minor
	percent              *string
	flagged              bool
	accepted             *comment
	stale, explained     bool
	proposed             int32
}

// report is a close's variance report against one basis, every row of it
// by account number: the close, its company and leading ledger, the
// company's settings for the basis read with its currency's digits, the
// period compared against and, for the plan basis, the category.
type report struct {
	close                periodClose
	company              Company
	leading              LedgerPeriod
	exp                  int
	basis                string
	setting              *config.CloseVariance
	configured           []string
	threshold            money.Minor
	percent              *big.Rat
	compYear, compPeriod int16
	plan                 *PlanCategory
	rows                 []varianceRow
	sums                 [3]money.Minor // the rows' actuals, comparisons and variances
	flagged, explained   int32
}

// ListPeriodCloseVariances implements GET
// /v1/period-closes/{id}/variances (ADR-0058 §9): each profit and loss
// account's actual of the close's period in the leading ledger against
// the basis, flagged by the company's thresholds, in account number
// order, with the whole report's totals.
func (e *Endpoints) ListPeriodCloseVariances(ctx context.Context, params gen.ListPeriodCloseVariancesParams) (gen.ListPeriodCloseVariancesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pagination.Size(params.PageSize.Value, params.PageSize.Set)
	if err != nil {
		return nil, err
	}
	// Values, never pointers: a pointer prints its address, which would bind
	// the token to one request.
	scope := fmt.Sprintf("listPeriodCloseVariances|%s|%s|%t|%t|%t|%t", params.ID, params.Basis, params.RequiresComment.Set, params.RequiresComment.Value,
		params.Unexplained.Set, params.Unexplained.Value)
	var after varianceCursor
	if params.PageToken.Set {
		if err := e.Pages.Decode(scope, params.PageToken.Value, &after); err != nil {
			return nil, err
		}
	}
	r, err := e.report(ctx, tx, params.ID, params.Basis)
	if err == nil {
		err = r.refused()
	}
	if err != nil {
		return nil, err
	}
	people, err := idref.Resolve(ctx, e.People.Named, r.commenters(), personRef)
	if err != nil {
		return nil, fmt.Errorf("closing: who wrote and accepted the comments of close %s: %w", r.close.ID, err)
	}
	out := r.toAPI()
	for _, row := range r.rows {
		switch {
		case !keeps(params, row), row.Number <= after.Number && params.PageToken.Set:
			continue
		case len(out.Items) == size:
			tok, _ := e.Pages.Encode(scope, varianceCursor{Number: out.Items[size-1].AccountNumber}) // a string always encodes
			out.NextPageToken.SetTo(tok)
			return &out, nil
		}
		out.Items = append(out.Items, r.rowToAPI(row, people))
	}
	return &out, nil
}

// keeps reports whether a row passes the report's filters: whether it
// requires a comment, and whether it is unexplained.
func keeps(params gen.ListPeriodCloseVariancesParams, row varianceRow) bool {
	return (!params.RequiresComment.Set || row.flagged == params.RequiresComment.Value) && (!params.Unexplained.Set || row.explained != params.Unexplained.Value)
}

// commenters are the people who wrote and accepted the rows' accepted
// comments, for their names.
func (r *report) commenters() []uuid.UUID {
	var ids []uuid.UUID
	for _, row := range r.rows {
		if row.accepted != nil {
			ids = append(ids, row.accepted.AuthorID, *row.accepted.DecidedBy) // an accepted comment is decided (variance_comment_decided)
		}
	}
	return ids
}

// report reads a close's variance report against the basis: the company's
// settings for it, the actuals of the close's period and the comparison;
// a report without rows and settings when the company does not configure
// the basis (refused says so).
func (e *Endpoints) report(ctx context.Context, tx pgx.Tx, id uuid.UUID, basis string) (report, error) {
	c, err := readClose(ctx, tx, id)
	if err != nil {
		return report{}, err
	}
	r := report{close: c, basis: basis}
	if err := e.settings(ctx, tx, &r); err != nil || r.setting == nil {
		return r, err
	}
	covered, err := e.coveredPeriods(ctx, tx, c.ID, c.LegalEntityID, c.FiscalYear, c.FiscalPeriod)
	if err != nil {
		return report{}, err
	}
	r.leading = covered[0] // the leading ledger, always covered, first
	actual, err := e.Ledger.ProfitAndLoss(ctx, c.LegalEntityID, c.FiscalYear, c.FiscalPeriod)
	if err != nil {
		return report{}, fmt.Errorf("closing: profit and loss of close %s: %w", c.ID, err)
	}
	compared, others, err := e.comparison(ctx, &r)
	if err != nil {
		return report{}, err
	}
	notes, err := rowComments(ctx, tx, c.ID, basis)
	if err != nil {
		return report{}, err
	}
	return r, r.compute(append(actual, others...), compared, notes)
}

// explain gives each row its accepted comment, stale when the figures it
// was written against are no longer the row's, and the count of its
// proposals, and counts the rows explained.
func (r *report) explain(notes map[uuid.UUID][]comment) {
	for i := range r.rows {
		row := &r.rows[i]
		for _, c := range notes[row.AccountID] {
			if c.Status == CommentProposed {
				row.proposed++
				continue
			}
			row.accepted = &c
			row.stale = c.Actual != row.Net || c.Comparison != row.comparison
			row.explained = !row.stale
		}
		if row.explained {
			r.explained++
		}
	}
}

// row is the report's row of an account, ok false when it has none.
func (r *report) row(account uuid.UUID) (varianceRow, bool) {
	for _, row := range r.rows {
		if row.AccountID == account {
			return row, true
		}
	}
	return varianceRow{}, false
}

// settings reads the close's company and its variance settings for the
// basis, none when it does not configure the basis, with its currency's
// digits.
func (e *Endpoints) settings(ctx context.Context, tx pgx.Tx, r *report) error {
	co, err := e.company(ctx, r.close.LegalEntityID)
	if err != nil {
		return err
	}
	r.company = co
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return fmt.Errorf("closing: variance settings: %w", err)
	}
	if dc := doc.Company(co.Code); dc != nil && dc.Close.Variance != nil {
		r.setting, r.configured = dc.Variance(r.basis), dc.Close.Variance.Bases
	}
	if r.setting == nil {
		return nil
	}
	if r.exp, err = currency.Exponent(ctx, tx, co.LocalCurrency, nil); err != nil {
		return fmt.Errorf("closing: currency of company %s: %w", co.Code, err)
	}
	// The configuration's validation admits only a positive amount in the
	// company's currency and a positive percent of two places.
	if r.threshold, err = money.ParseDecimal(r.setting.ThresholdAmount, r.exp); err != nil {
		return fmt.Errorf("closing: threshold of company %s: %w", co.Code, err)
	}
	if r.setting.ThresholdPercent != "" {
		if r.percent, err = money.ParseDecimalRat(r.setting.ThresholdPercent); err != nil {
			return fmt.Errorf("closing: percent threshold of company %s: %w", co.Code, err)
		}
	}
	return nil
}

// refused refuses a report whose basis the company does not configure,
// naming the bases it does.
func (r report) refused() error {
	if r.setting != nil {
		return nil
	}
	bases := "none (companies[].close.variance)"
	if len(r.configured) > 0 {
		bases = strings.Join(r.configured, ", ")
	}
	return problem.Unprocessable("closing.basis_not_configured", "Company "+r.company.Code+" does not compare its variances against "+r.basis+
		"; the bases it configures: "+bases+".", problem.FieldError{Field: "basis", Code: "not_configured", Message: "a basis the company configures"})
}

// comparison reads what the basis compares the period against, per
// account: the previous regular period's or the prior year's period's net
// movement, with that period's accounts, or the plan category's amounts
// for the period.
func (e *Endpoints) comparison(ctx context.Context, r *report) (map[uuid.UUID]money.Minor, []AccountNet, error) {
	c := r.close
	r.compYear, r.compPeriod = c.FiscalYear, c.FiscalPeriod
	switch r.basis {
	case config.BasisPriorPeriod:
		if r.compPeriod--; r.compPeriod == 0 {
			r.compYear, r.compPeriod = c.FiscalYear-1, maxRegular
		}
	case config.BasisPriorYear:
		r.compYear--
	default: // config.BasisPlan: the only other basis the configuration admits
		amounts, err := e.planned(ctx, r)
		return amounts, nil, err
	}
	nets, err := e.Ledger.ProfitAndLoss(ctx, c.LegalEntityID, r.compYear, r.compPeriod)
	if err != nil {
		return nil, nil, fmt.Errorf("closing: profit and loss of %d/%d: %w", r.compYear, r.compPeriod, err)
	}
	out := map[uuid.UUID]money.Minor{}
	for i, n := range nets {
		out[n.AccountID] = n.Net
		nets[i].Net = 0 // the account's actual is the period's, not the comparison's
	}
	return out, nets, nil
}

// planned reads the configured plan category's amounts for the close's
// period, over its cost center or its profit center lines.
func (e *Endpoints) planned(ctx context.Context, r *report) (map[uuid.UUID]money.Minor, error) {
	cat, ok, err := e.Plans.Category(ctx, r.setting.PlanCategory)
	if err == nil && !ok {
		err = fmt.Errorf("plan category %s is not held by the controlling context", r.setting.PlanCategory)
	}
	if err != nil {
		return nil, fmt.Errorf("closing: plan category of company %s: %w", r.company.Code, err)
	}
	r.plan = &cat
	amounts, err := e.Plans.Amounts(ctx, r.close.LegalEntityID, cat.ID, r.close.FiscalYear, r.close.FiscalPeriod, r.setting.PlanCenters == config.PlanCentersProfit)
	if err != nil {
		return nil, fmt.Errorf("closing: plan %s of close %s: %w", cat.Code, r.close.ID, err)
	}
	return amounts, nil
}

// compute makes the report's rows from the accounts of the period's
// profit and loss with their actuals, then those of the comparison
// period's, at zero, that the first lack: one per account with a non-zero
// actual or comparison, by account number, each with its comments. A plan
// amount on an account that is no profit and loss account of the company
// has no row.
func (r *report) compute(accounts []AccountNet, compared map[uuid.UUID]money.Minor, notes map[uuid.UUID][]comment) error {
	byID := map[uuid.UUID]AccountNet{}
	for _, a := range accounts {
		if _, ok := byID[a.AccountID]; !ok {
			byID[a.AccountID] = a
		}
	}
	for _, a := range byID {
		row := varianceRow{AccountNet: a, comparison: compared[a.AccountID]}
		if row.Net == 0 && row.comparison == 0 {
			continue
		}
		var err error
		if row.variance, err = money.Diff(row.Net, row.comparison); err != nil {
			return fmt.Errorf("closing: variance of account %s: %w", a.Number, err)
		}
		if row.comparison != 0 {
			p := money.PercentOfMagnitude(row.variance, row.comparison)
			row.percent = &p
		}
		row.flagged = r.flags(row)
		r.rows = append(r.rows, row)
	}
	slices.SortFunc(r.rows, func(a, b varianceRow) int { return strings.Compare(a.Number, b.Number) })
	r.explain(notes)
	return r.total()
}

// total sums every row of the report and counts those flagged.
func (r *report) total() error {
	actuals, comparisons, variances := make([]money.Minor, len(r.rows)), make([]money.Minor, len(r.rows)), make([]money.Minor, len(r.rows))
	for i, row := range r.rows {
		actuals[i], comparisons[i], variances[i] = row.Net, row.comparison, row.variance
		if row.flagged {
			r.flagged++
		}
	}
	for i, amounts := range [][]money.Minor{actuals, comparisons, variances} {
		var err error
		if r.sums[i], err = money.Sum(amounts...); err != nil {
			return fmt.Errorf("closing: totals of the variances of close %s: %w", r.close.ID, err)
		}
	}
	return nil
}

// flags reports whether a row requires a comment: its variance at least
// the amount threshold either way and, when a percent threshold is set and
// the row has a percent, the percent as shown at least it either way.
func (r *report) flags(row varianceRow) bool {
	if row.variance < r.threshold && row.variance > -r.threshold {
		return false
	}
	if r.percent == nil || row.percent == nil {
		return true
	}
	shown, _ := money.ParseDecimalRat(*row.percent) // PercentOfMagnitude writes a plain decimal
	return shown.Abs(shown).Cmp(r.percent) >= 0
}

// toAPI is the report's header and totals, with no rows yet.
func (r *report) toAPI() gen.PeriodCloseVariancePage {
	cur, sums := r.company.LocalCurrency, r.sums
	out := gen.PeriodCloseVariancePage{CloseID: r.close.ID, LegalEntityID: r.close.LegalEntityID, CompanyCode: r.company.Code, CompanyName: r.company.Name,
		FiscalYear: int32(r.close.FiscalYear), FiscalPeriod: int32(r.close.FiscalPeriod), LedgerID: r.leading.LedgerID, LedgerCode: r.leading.Code,
		LedgerName: r.leading.Name, Currency: cur, Basis: r.basis, ComparisonFiscalYear: int32(r.compYear), ComparisonFiscalPeriod: int32(r.compPeriod),
		ThresholdAmount: apitypes.Money(r.threshold, cur, r.exp), Items: []gen.PeriodCloseVariance{},
		Totals: gen.PeriodCloseVarianceTotals{Actual: apitypes.Money(sums[0], cur, r.exp), Comparison: apitypes.Money(sums[1], cur, r.exp),
			Variance: apitypes.Money(sums[2], cur, r.exp), Rows: int32(len(r.rows)), RowsRequiringComment: r.flagged, RowsExplained: r.explained}} // at most the accounts of one chart
	if r.setting.ThresholdPercent != "" {
		out.ThresholdPercent.SetTo(gen.Decimal(r.setting.ThresholdPercent))
	}
	if r.plan != nil {
		out.PlanCategoryID.SetTo(r.plan.ID)
		out.PlanCategoryCode.SetTo(r.plan.Code)
		out.PlanCategoryName.SetTo(r.plan.Name)
		out.PlanCenters.SetTo(r.setting.PlanCenters)
	}
	return out
}

// rowToAPI is a row as the report shows it, its accepted comment's
// author and decider named.
func (r *report) rowToAPI(row varianceRow, people map[uuid.UUID]Person) gen.PeriodCloseVariance {
	cur := r.company.LocalCurrency
	out := gen.PeriodCloseVariance{AccountID: row.AccountID, AccountNumber: row.Number, AccountName: row.Name, Actual: apitypes.Money(row.Net, cur, r.exp),
		Comparison: apitypes.Money(row.comparison, cur, r.exp), Variance: apitypes.Money(row.variance, cur, r.exp), RequiresComment: row.flagged,
		Explained: row.explained, ProposedComments: row.proposed}
	if row.percent != nil {
		out.VariancePercent.SetTo(gen.Decimal(*row.percent))
	}
	if c := row.accepted; c != nil {
		// The comment is in the company's currency, the report's.
		out.Comment.SetTo(gen.VarianceRowComment{ID: c.ID, Text: c.Text, Edited: c.ProposedText != nil, AuthorID: c.AuthorID, AuthorKind: c.AuthorKind,
			AuthorName: people[c.AuthorID].Name, DecidedBy: *c.DecidedBy, DecidedByName: people[*c.DecidedBy].Name, DecidedAt: *c.DecidedAt,
			Actual: apitypes.Money(c.Actual, c.Currency, r.exp), Comparison: apitypes.Money(c.Comparison, c.Currency, r.exp), Stale: row.stale, Version: c.Version})
	}
	return out
}
