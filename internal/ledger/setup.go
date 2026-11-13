package ledger

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Setup is the configuration document's ledger section: the ledgers and
// ledger groups, each company's ledgers, chart of accounts and account
// determination, and its fiscal calendar with the periods of every fiscal
// year that has a number range.
//
// The mapping, decided in T3 (docs/design/07-transition.md):
//   - each company has its own chart, coded with the company code; accounts
//     removed from the document become inactive, and an account's type and
//     reconciliation kind never change once activated;
//   - the legal entity's fiscal_calendar_id names its calendar (12 normal and
//     4 special periods), created with the start month of the document;
//     fiscal years are named after the calendar year they end in, so with
//     start month 4 fiscal year 2026 runs from 1 April 2025 to 31 March 2026;
//   - ledgers, the ledgers each company keeps and ledger groups (T52a,
//     ADR-0045) are created from the document, the leading ledger 0L alone
//     when it names none;
//   - a fiscal year exists for a company once the document has a number
//     range for it; its periods are created closed in every ledger of the
//     company and opened with fiscal-periods/{id}:open;
//   - tax codes (T61a, ADR-0049 §3) are kept per legal entity as a
//     definition, components and dated rates (taxCodes): the document's
//     rates are written ahead, a rate a posting used never changes (the
//     database refuses it), and a removed code's rates end on the
//     activation day; a code in the single-rate form (T4) is valid from
//     the start of the company's first fiscal year and a changed rate
//     starts on the activation day;
//   - withholding tax codes (T46e) are kept per legal entity, changed in
//     place (what an invoice withheld is kept with its entry, T46f) and
//     deactivated when removed, which the partner context refuses while a
//     supplier has the code (partner.WithholdingCodes);
//   - the foreign-currency valuation (T48a, ADR-0042) is kept per legal
//     entity: its rate type, the flagged accounts and each revalued
//     account's valuation accounts, deactivated when removed;
//   - the controlling rules (T56c, ADR-0050 §4, controlling.go): each
//     account's cost element, the tenant's cost center categories, and per
//     legal entity the fiscal years cost and profit center accounting apply
//     from, the default profit center and the default cost centers.
type Setup struct {
	LegalEntities LegalEntities
	// Now is the clock the controlling rules read today by (the fiscal year
	// center accounting is turned on from, whether default centers are
	// valid); nil is the wall clock.
	Now func() time.Time
}

const (
	normalPeriods  = 12
	specialPeriods = 4
)

var accountNumber = regexp.MustCompile(`^[0-9A-Z]{1,10}$`)

// Check reports what the ledger cannot apply: companies without a legal
// entity, a currency that is not the legal entity's, account numbers the
// ledger does not accept, a start month that contradicts the legal
// entity's existing calendar, changes to activated accounts, a tax code's
// direction or account key changed, a rate change that would re-rate
// postings (predicting 409 ledger.tax_rate_in_use), a GR/IR account
// already posted and open-item management turned on for a posted account.
func (s Setup) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var out findings
	if err := checkCategories(ctx, tx, proposed, &out); err != nil {
		return nil, err
	}
	for _, co := range proposed.Companies {
		var was *config.Company
		if base != nil {
			was = base.Company(co.Code)
		}
		if err := s.checkCompany(ctx, tx, co, was, &out); err != nil {
			return nil, err
		}
	}
	if err := checkLedgers(ctx, tx, proposed, &out); err != nil {
		return nil, err
	}
	checkAssetRanges(proposed, clock.Now(s.Now), &out)
	if base == nil {
		return out, nil
	}
	checkActivated(base, proposed, &out)
	return out, nil
}

// findings collects the errors Check reports.
type findings []config.Finding

func (f *findings) add(path, format string, a ...any) {
	*f = append(*f, config.Finding{Severity: config.SevError, Path: path, Message: fmt.Sprintf(format, a...)})
}

// checkCompany checks one proposed company against its legal entity: the
// account numbers, the legal entity's existence and currency, the start
// month of its calendar, a tax code's direction or account key changed, a
// rate change that would re-rate postings, a new GR/IR account that is
// already posted, and open-item management turned on for an account with
// postings. was is the activated company, nil for a new one.
func (s Setup) checkCompany(ctx context.Context, tx pgx.Tx, co config.Company, was *config.Company, out *findings) error {
	p := "companies[" + co.Code + "]"
	for _, a := range co.ChartOfAccounts {
		if !accountNumber.MatchString(a.Number) {
			out.add(p+".chart_of_accounts["+a.Number+"]", "account numbers are 1-10 upper-case letters or digits")
		}
	}
	le, ok, err := s.LegalEntities.EntityByCode(ctx, co.Code)
	if err != nil {
		return err
	}
	if !ok {
		out.add(p+".code", "no legal entity has company code %s", co.Code)
		return nil
	}
	if co.Currency != le.LocalCurrency {
		out.add(p+".currency", "must be the legal entity's local currency %s", le.LocalCurrency)
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT extract(month FROM start_date)::int FROM ledger.fiscal_period
		WHERE calendar_id = $1 AND period = 1 ORDER BY 1`, le.FiscalCalendarID)
	months, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return fmt.Errorf("ledger: check company %s: fiscal year start: %w", co.Code, err)
	}
	if len(months) > 0 && !slices.Contains(months, int32(co.FiscalYearStartMonth)) {
		out.add(p+".fiscal_year_start_month", "the legal entity's fiscal calendar starts its years in month %d", months[0])
	}
	if err := checkCompanyLedgers(ctx, tx, p, le.ID, co, out); err != nil {
		return err
	}
	if err := checkTaxCodesFixed(ctx, tx, p, le.ID, co, out); err != nil {
		return err
	}
	// One date per company: the rates and the controlling switches are
	// judged on the same company's date. A document whose zone does not
	// load is not judged on it: validation reports the zone
	// (companies[].time_zone).
	today, dated := checkDay(clock.Now(s.Now), co)
	if dated {
		if err := checkTaxRatesUnposted(ctx, tx, p, le.ID, le.FiscalCalendarID, co, today, out); err != nil {
			return err
		}
	}
	if err := checkOpenItemsUnposted(ctx, tx, p, le.ID, co, was, out); err != nil {
		return err
	}
	if err := checkAssetReconciliationNew(ctx, tx, p, co, was, out); err != nil {
		return err
	}
	if err := checkSecondaryNew(ctx, tx, p, co, out); err != nil {
		return err
	}
	if dated {
		if _, err := readControlling(ctx, tx, p, le.ID, co, today, false, out); err != nil {
			return err
		}
	}
	return checkGRIRUnposted(ctx, tx, p, le.ID, co, out)
}

// checkDay is the company's date at now in its time zone, and whether its
// zone loads; a check judges nothing on the date of one that does not.
func checkDay(now time.Time, co config.Company) (companyDay, bool) {
	d, err := dayOfCompany(now, co)
	return d, err == nil
}

// checkOpenItemsUnposted reports accounts the proposal makes open-item
// managed that already have postings (ADR-0047, product owner
// 2026-10-20): their lines were never open items, so a statement could
// not clear them and the account's balance would never reconcile. The
// company names a new clearing account instead and transfers the old
// balance to it with one audited manual entry.
func checkOpenItemsUnposted(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, was *config.Company, out *findings) error {
	turned := turnedOn(co, was, func(a *config.Account) bool { return a.OpenItems })
	posted, err := postedAccounts(ctx, tx, le, co.Code, turned)
	if err != nil {
		return fmt.Errorf("ledger: check company %s: open-item accounts: %w", co.Code, err)
	}
	for _, number := range posted {
		out.add(p+".chart_of_accounts["+number+"].open_items", "account %s has postings, so it cannot become open-item managed; "+
			"name a new clearing account and transfer the old balance to it", number)
	}
	return nil
}

// checkAssetReconciliationNew reports accounts the proposal makes asset
// reconciliation accounts that the chart already has (ADR-0057 §3, As
// built T57a): only an account the activation itself creates becomes one.
// An existing account could take a posting in flight while it is turned,
// which no check before the commit would see, and a line no assets
// document posted would stay on it for good; a new account has no lines
// until the activation commits.
func checkAssetReconciliationNew(ctx context.Context, tx pgx.Tx, p string, co config.Company, was *config.Company, out *findings) error {
	turned := turnedOn(co, was, func(a *config.Account) bool { return a.AssetReconciliation })
	if len(turned) == 0 {
		return nil
	}
	rows, _ := tx.Query(ctx, `SELECT g.number FROM ledger.gl_account g
		JOIN ledger.chart_of_accounts c ON c.tenant_id = g.tenant_id AND c.id = g.chart_id AND c.code = $1
		WHERE g.number = ANY($2) AND g.reconciliation_kind <> 'asset' ORDER BY 1`, co.Code, turned)
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("ledger: check company %s: asset reconciliation accounts: %w", co.Code, err)
	}
	for _, number := range existing {
		out.add(p+".chart_of_accounts["+number+"].asset_reconciliation", "account %s exists already, so it cannot become an asset reconciliation "+
			"account; add a new account flagged asset_reconciliation, before any balance is posted or migrated onto it", number)
	}
	return nil
}

// turnedOn is the numbers of the company's accounts flag holds for that it
// did not hold for in the activated company was (nil: a new company).
func turnedOn(co config.Company, was *config.Company, flag func(*config.Account) bool) []string {
	var numbers []string
	for i := range co.ChartOfAccounts {
		a := &co.ChartOfAccounts[i]
		if !flag(a) {
			continue
		}
		if was == nil || was.Account(a.Number) == nil || !flag(was.Account(a.Number)) {
			numbers = append(numbers, a.Number)
		}
	}
	return numbers
}

// postedAccounts is those of the numbers of company code's chart that have
// lines in the legal entity, in order.
func postedAccounts(ctx context.Context, tx pgx.Tx, le uuid.UUID, code string, numbers []string) ([]string, error) {
	if len(numbers) == 0 {
		return nil, nil
	}
	rows, _ := tx.Query(ctx, `SELECT g.number FROM ledger.gl_account g
		JOIN ledger.chart_of_accounts c ON c.tenant_id = g.tenant_id AND c.id = g.chart_id AND c.code = $2
		WHERE g.number = ANY($3) AND EXISTS (SELECT FROM ledger.journal_line l
			WHERE l.tenant_id = g.tenant_id AND l.legal_entity_id = $1 AND l.account_id = g.id)
		ORDER BY 1`, le, code, numbers)
	posted, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("accounts with postings: %w", err)
	}
	return posted, nil
}

// checkAssetRanges reports a company with asset accounts that lacks a
// number range of AssetRanges in its current fiscal year (ADR-0057 §3):
// its assets, acquisitions and depreciation could not be numbered. The
// fiscal year is the one the company's date at now falls in; a company
// whose zone does not load is not judged (validation reports the zone).
func checkAssetRanges(doc *config.Config, now time.Time, out *findings) {
	have := map[string]bool{}
	for _, n := range doc.NumberRanges {
		have[n.Key()] = true
	}
	for _, co := range doc.Companies {
		today, dated := checkDay(now, co)
		if len(co.AssetAccounts) == 0 || !dated {
			continue
		}
		year := fiscalYearOf(today.day, co.FiscalYearStartMonth)
		for _, kind := range config.AssetRanges {
			need := config.NumberRange{Company: co.Code, DocumentType: kind, FiscalYear: year}
			if !have[need.Key()] {
				out.add("number_ranges["+need.Key()+"]", "company %s has asset accounts, so it numbers its assets and their postings: "+
					"add a %s range for fiscal year %d, the current one", co.Code, kind, year)
			}
		}
	}
}

// checkGRIRUnposted reports a GR/IR account that is not yet the legal
// entity's but has postings other than a migration run's: the account's
// balance must be the order items' positions from the start (T33), and a
// reversal of such a posting would be refused once it is the GR/IR account.
func checkGRIRUnposted(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, out *findings) error {
	number := co.AccountDetermination.GRIR
	if number == "" {
		return nil
	}
	var posted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.gl_account g
		JOIN ledger.chart_of_accounts c ON c.tenant_id = g.tenant_id AND c.id = g.chart_id AND c.code = $2
		JOIN ledger.journal_line l ON l.tenant_id = g.tenant_id AND l.legal_entity_id = $1 AND l.account_id = g.id
		JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
		WHERE g.number = $3 AND e.source_context <> 'integration'
			AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = g.tenant_id AND d.legal_entity_id = $1
				AND d.key = 'gr_ir' AND d.account_id = g.id))`, le, co.Code, number).Scan(&posted); err != nil {
		return fmt.Errorf("ledger: check company %s: GR/IR account %s: %w", co.Code, number, err)
	}
	if posted {
		out.add(p+".account_determination.gr_ir", "account %s has postings other than a migration run's; the GR/IR account starts without any", number)
	}
	return nil
}

// checkActivated reports changes the ledger cannot make to what is
// activated: removing a company, changing an account's type or
// reconciliation kind, or changing the GR/IR account.
func checkActivated(base, proposed *config.Config, out *findings) {
	for _, old := range base.Companies {
		p := "companies[" + old.Code + "]"
		co := proposed.Company(old.Code)
		if co == nil {
			out.add(p, "a company cannot be removed once activated")
			continue
		}
		// The open GR/IR positions are lines on the activated account, and
		// orders released since are valued through it (T33): another
		// account, or none, would strand them.
		if was := old.AccountDetermination.GRIR; was != "" && co.AccountDetermination.GRIR != was {
			out.add(p+".account_determination.gr_ir", "the GR/IR account cannot change once activated")
		}
		// Down payments are lines on the activated account, and their
		// reversals and clearings post to it (ADR-0037).
		if was := old.AccountDetermination.DownPayment; was != "" && co.AccountDetermination.DownPayment != was {
			out.add(p+".account_determination.down_payment", "the down payment account cannot change once activated")
		}
		// The determination is never deleted (no grant); the account may be
		// changed for the next carry-forward, not removed (ADR-0060 §4).
		if old.YearEnd.RetainedEarningsAccount != "" && co.YearEnd.RetainedEarningsAccount == "" {
			out.add(p+".year_end.retained_earnings_account", "the retained earnings account can be changed but not removed once activated")
		}
		for _, was := range old.ChartOfAccounts {
			a := co.Account(was.Number)
			switch {
			case a == nil: // removing an account makes it inactive
			case a.Type != was.Type:
				out.add(p+".chart_of_accounts["+was.Number+"].type", "an account's type cannot change once activated")
			case a.VendorReconciliation != was.VendorReconciliation:
				out.add(p+".chart_of_accounts["+was.Number+"].vendor_reconciliation", "an account's reconciliation kind cannot change once activated")
			case a.CustomerReconciliation != was.CustomerReconciliation: // its lines are, or are not, customers' open items (T74a)
				out.add(p+".chart_of_accounts["+was.Number+"].customer_reconciliation", "an account's reconciliation kind cannot change once activated")
			case was.OpenItems && !a.OpenItems: // its lines are open items a statement may still clear (ADR-0047)
				out.add(p+".chart_of_accounts["+was.Number+"].open_items", "open-item management cannot be turned off once activated")
			case was.AssetReconciliation && !a.AssetReconciliation: // its balance is assets' movements (ADR-0057 §3)
				out.add(p+".chart_of_accounts["+was.Number+"].asset_reconciliation", "an asset reconciliation account stays one once activated")
			}
		}
	}
}

// Apply makes the ledger's master data match the document.
func (s Setup) Apply(ctx context.Context, doc *config.Config) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	if err := applyLedgers(ctx, tx, p, doc); err != nil {
		return err
	}
	if err := applyCategories(ctx, tx, p, doc); err != nil {
		return err
	}
	for _, co := range doc.Companies {
		if err := s.applyCompany(ctx, tx, p, co, doc); err != nil {
			return err
		}
	}
	return applyLedgerGroups(ctx, tx, p, doc)
}

// applyCompany makes one company's chart of accounts, account
// determination, fiscal calendar, tax codes and foreign-currency valuation
// match the document.
func (s Setup) applyCompany(ctx context.Context, tx pgx.Tx, p reqctx.Principal, co config.Company, doc *config.Config) error {
	le, ok, err := s.LegalEntities.EntityByCode(ctx, co.Code)
	if err != nil {
		return err
	}
	if !ok { // checked on submission; legal entities are never deleted
		return fmt.Errorf("ledger: no legal entity %s", co.Code)
	}
	var chart uuid.UUID
	if err := tx.QueryRow(ctx, `WITH ins AS (
			INSERT INTO ledger.chart_of_accounts (tenant_id, id, code, name, created_by) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, code) DO NOTHING RETURNING id
		) SELECT id FROM ins UNION ALL SELECT id FROM ledger.chart_of_accounts WHERE code = $3 LIMIT 1`,
		p.TenantID, uuid.Must(uuid.NewV7()), co.Code, co.Name, p.ID).Scan(&chart); err != nil {
		return fmt.Errorf("ledger: apply company %s: chart of accounts: %w", co.Code, err)
	}
	if err := applyAccounts(ctx, tx, p, le.ID, chart, co); err != nil {
		return err
	}
	// One date per company, as checkCompany's; a zone that does not load
	// fails the activation rather than judging it in UTC.
	today, err := dayOfCompany(clock.Now(s.Now), co)
	if err != nil {
		return err
	}
	if err := applyControlling(ctx, tx, p, le.ID, chart, co, today); err != nil {
		return err
	}
	ad := co.AccountDetermination
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.account_determination AS d (tenant_id, legal_entity_id, key, account_id, updated_by)
		SELECT $1, $2, k, g.id, $4 FROM unnest($5::text[], $6::text[]) AS x(k, n) JOIN ledger.gl_account g ON g.chart_id = $3 AND g.number = x.n
		ON CONFLICT (tenant_id, legal_entity_id, key) DO UPDATE SET account_id = EXCLUDED.account_id, version = d.version + 1, updated_by = $4
		WHERE d.account_id <> EXCLUDED.account_id`,
		p.TenantID, le.ID, chart, p.ID, []string{"ap_reconciliation", "input_tax", "bank", "fx_gain", "fx_loss", "gr_ir", "price_difference",
			"cash_discount", "down_payment", "retained_earnings", "contract_asset", "contract_liability"}, []string{ad.APReconciliation, ad.InputTax,
			ad.Bank, ad.FXGain, ad.FXLoss, ad.GRIR, ad.PriceDifference, ad.CashDiscount, ad.DownPayment, co.YearEnd.RetainedEarningsAccount,
			ad.ContractAsset, ad.ContractLiability}); err != nil {
		return fmt.Errorf("ledger: apply company %s: account determination: %w", co.Code, err)
	}
	if err := applyCompanyLedgers(ctx, tx, p, le.ID, co); err != nil {
		return err
	}
	if err := s.calendar(ctx, tx, p, le.FiscalCalendarID, le.ID, co, doc); err != nil {
		return err
	}
	if err := taxCodes(ctx, tx, p, le.ID, le.FiscalCalendarID, chart, co, today); err != nil {
		return err
	}
	if err := withholdingTaxCodes(ctx, tx, p, le.ID, chart, co); err != nil {
		return err
	}
	return fxValuation(ctx, tx, p, le.ID, chart, co)
}

// withholdingTaxCodes makes the legal entity's withholding tax codes the
// company's (T46e): new ones inserted, changed ones updated, and those the
// document no longer has deactivated, never deleted (the partner context
// refuses removing a code a supplier has, partner.WithholdingCodes).
func withholdingTaxCodes(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company) error {
	n := len(co.WithholdingTaxCodes)
	codes, descriptions, rates, bases, numbers, postings := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	for i, w := range co.WithholdingTaxCodes {
		codes[i], descriptions[i], rates[i], bases[i], numbers[i], postings[i] = w.Code, w.Description, w.RatePercent, w.BaseOrNet(), w.Account, w.Posting
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.withholding_tax_code SET is_active = false, version = version + 1, updated_by = $3
		WHERE legal_entity_id = $1 AND is_active AND code <> ALL($2)`, le, codes, p.ID); err != nil {
		return fmt.Errorf("ledger: deactivate withholding tax codes of %s: %w", le, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.withholding_tax_code AS w (tenant_id, legal_entity_id, code, description, rate_pct, base, account_id, posting,
			created_by, updated_by)
		SELECT $1, $2, d.code, d.description, d.rate::numeric, d.base, g.id, d.posting, $3, $3
		FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[]) AS d(code, description, rate, base, number, posting)
		JOIN ledger.gl_account g ON g.chart_id = $10 AND g.number = d.number
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET description = EXCLUDED.description, rate_pct = EXCLUDED.rate_pct, base = EXCLUDED.base,
			account_id = EXCLUDED.account_id, posting = EXCLUDED.posting, is_active = true, version = w.version + 1, updated_by = $3
		WHERE (w.description, w.rate_pct, w.base, w.account_id, w.posting, w.is_active)
			IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.rate_pct, EXCLUDED.base, EXCLUDED.account_id, EXCLUDED.posting, true)`,
		p.TenantID, le, p.ID, codes, descriptions, rates, bases, numbers, postings, chart); err != nil {
		return fmt.Errorf("ledger: withholding tax codes of %s: %w", le, err)
	}
	return nil
}

// applyAccounts makes the chart's accounts those of the document,
// deactivating the ones it no longer has, and sets them up in the legal
// entity.
func applyAccounts(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company) error {
	var ids []uuid.UUID
	var numbers, names, types, kinds, elements []string
	for _, a := range co.ChartOfAccounts {
		kind := "none"
		switch {
		case a.VendorReconciliation:
			kind = "supplier"
		case a.CustomerReconciliation:
			kind = "customer"
		case a.AssetReconciliation:
			kind = "asset"
		}
		ids = append(ids, uuid.Must(uuid.NewV7()))
		numbers, names, types, kinds = append(numbers, a.Number), append(names, a.Name), append(types, string(a.Type)), append(kinds, kind)
		elements = append(elements, string(a.CostElementOrDefault()))
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.gl_account AS g (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind,
			cost_element, status, created_by, updated_by)
		SELECT $1, i, $2, n, nm, t, k, ce, 'active', $3, $3
		FROM unnest($4::uuid[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[]) AS a(i, n, nm, t, k, ce)
		ON CONFLICT (tenant_id, chart_id, number) DO UPDATE SET name = EXCLUDED.name, cost_element = EXCLUDED.cost_element, status = 'active',
			version = g.version + 1, updated_by = $3
		WHERE (g.name, g.cost_element, g.status) IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.cost_element, 'active')`,
		p.TenantID, chart, p.ID, ids, numbers, names, types, kinds, elements); err != nil {
		return fmt.Errorf("ledger: apply company %s: accounts: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1, updated_by = $3
		WHERE chart_id = $1 AND status <> 'inactive' AND NOT number = ANY($2)`, chart, numbers, p.ID); err != nil {
		return fmt.Errorf("ledger: apply company %s: deactivate accounts: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, is_open_item_managed, created_by, updated_by)
		SELECT $1, $2, id, reconciliation_kind IN ('supplier', 'customer'), $4, $4 FROM ledger.gl_account WHERE chart_id = $3 AND number = ANY($5)
		ON CONFLICT DO NOTHING`, p.TenantID, le, chart, p.ID, numbers); err != nil {
		return fmt.Errorf("ledger: apply company %s: company accounts: %w", co.Code, err)
	}
	if err := applyAssetReconciliation(ctx, tx, chart, co); err != nil {
		return err
	}
	return applyOpenItems(ctx, tx, p, le, chart, co)
}

// applyAssetReconciliation refuses the activation when the document
// flags asset_reconciliation an account the chart already had (409
// ledger.asset_reconciliation_not_new): applyAccounts inserts a new
// account as one and never changes an existing account's kind, so such an
// account is left reconciling nothing. Check refuses the same on
// submission; an account another activation created while the change
// awaited approval is caught here, and the activation rolls back.
func applyAssetReconciliation(ctx context.Context, tx pgx.Tx, chart uuid.UUID, co config.Company) error {
	var numbers []string
	for _, a := range co.ChartOfAccounts {
		if a.AssetReconciliation {
			numbers = append(numbers, a.Number)
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	var number *string
	if err := tx.QueryRow(ctx, `SELECT min(number) FROM ledger.gl_account WHERE chart_id = $1 AND number = ANY($2) AND reconciliation_kind <> 'asset'`,
		chart, numbers).Scan(&number); err != nil {
		return fmt.Errorf("ledger: apply company %s: asset reconciliation accounts: %w", co.Code, err)
	}
	if number != nil {
		return problem.Conflict("ledger.asset_reconciliation_not_new", "Account "+*number+" of company "+co.Code+
			" existed before, so it cannot become an asset reconciliation account; add a new account flagged asset_reconciliation.")
	}
	return nil
}

// applyOpenItems makes the accounts the document has open-item managed
// (ADR-0047) and refuses the activation when one of them, not yet open-item
// managed, has postings (409 ledger.open_items_account_posted). Check
// refuses the same on submission; an entry posted while the change awaited
// approval is caught here, and the activation rolls back. Turning the flag
// on locks the account's row, so a second activation waits for this one.
// A posting in flight needs no lock against it: no journal posting's
// checks read the flag, so a line committed while the activation runs is a
// line posted after it, an open item like any other (ADR-0047
// Consequences); only a statement's clearing reads it, and one that reads
// it off is refused as if it ran first. Assets differ: a posting reads the account's
// reconciliation kind, hence ADR-0057's new-accounts-only rule.
func applyOpenItems(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company) error {
	var numbers []string
	for _, a := range co.ChartOfAccounts {
		if a.OpenItems {
			numbers = append(numbers, a.Number)
		}
	}
	rows, _ := tx.Query(ctx, `UPDATE ledger.company_account ca SET is_open_item_managed = true, version = ca.version + 1, updated_by = $4
		FROM ledger.gl_account g WHERE g.tenant_id = ca.tenant_id AND g.id = ca.account_id AND ca.legal_entity_id = $1 AND g.chart_id = $2
			AND g.number = ANY($3) AND NOT ca.is_open_item_managed
		RETURNING g.number, EXISTS (SELECT FROM ledger.journal_line l WHERE l.tenant_id = ca.tenant_id AND l.legal_entity_id = $1
			AND l.account_id = ca.account_id)`, le, chart, numbers, p.ID)
	type turned struct {
		Number string
		Posted bool
	}
	on, err := pgx.CollectRows(rows, pgx.RowToStructByPos[turned])
	if err != nil {
		return fmt.Errorf("ledger: apply company %s: open-item accounts: %w", co.Code, err)
	}
	for _, a := range on {
		if a.Posted {
			return problem.Conflict("ledger.open_items_account_posted", "Account "+a.Number+" of company "+co.Code+
				" has postings, so it cannot become open-item managed; name a new clearing account and transfer the old balance to it.")
		}
	}
	return nil
}

// calendar creates the legal entity's fiscal calendar if needed and the
// periods of every fiscal year the document has a number range for, closed
// in every ledger the company keeps: a parallel ledger has periods of its
// own, opened, closed and locked apart from the leading ledger's (ADR-0045).
func (s Setup) calendar(ctx context.Context, tx pgx.Tx, p reqctx.Principal, calendar, legalEntity uuid.UUID, co config.Company, doc *config.Config) error {
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.fiscal_calendar (tenant_id, id, code, name, normal_periods, special_periods, created_by)
		SELECT $1, $2, 'C' || lpad((count(*) + 1)::text, 3, '0'), $3, $4, $5, $6 FROM ledger.fiscal_calendar
		ON CONFLICT (tenant_id, id) DO NOTHING`,
		p.TenantID, calendar, "Fiscal year from "+time.Month(co.FiscalYearStartMonth).String(), normalPeriods, specialPeriods, p.ID); err != nil {
		return fmt.Errorf("ledger: fiscal calendar %s: %w", calendar, err)
	}
	var years, periods []int32
	var starts, ends []time.Time
	var special []bool
	seen := map[int]bool{}
	for _, n := range doc.NumberRanges {
		if n.Company != co.Code || seen[n.FiscalYear] {
			continue
		}
		seen[n.FiscalYear] = true
		for _, fp := range fiscalYear(n.FiscalYear, co.FiscalYearStartMonth) {
			years, periods = append(years, int32(n.FiscalYear)), append(periods, fp.period)
			starts, ends, special = append(starts, fp.start), append(ends, fp.end), append(special, fp.special)
		}
	}
	if len(years) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.fiscal_period (tenant_id, calendar_id, fiscal_year, period, start_date, end_date, is_special)
		SELECT $1, $2, y, n, s, e, sp FROM unnest($3::smallint[], $4::smallint[], $5::date[], $6::date[], $7::bool[]) AS x(y, n, s, e, sp)
		ON CONFLICT DO NOTHING`, p.TenantID, calendar, years, periods, starts, ends, special); err != nil {
		return fmt.Errorf("ledger: fiscal calendar %s: periods: %w", calendar, err)
	}
	var ledgers []uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT array_agg(ledger_id ORDER BY ledger_id) FROM ledger.company_ledger WHERE legal_entity_id = $1`,
		legalEntity).Scan(&ledgers); err != nil {
		return fmt.Errorf("ledger: fiscal calendar %s: ledgers: %w", calendar, err)
	}
	var ids, ledgerIDs []uuid.UUID
	var statusYears, statusPeriods []int32
	for _, l := range ledgers {
		for i := range years {
			ids, ledgerIDs = append(ids, uuid.Must(uuid.NewV7())), append(ledgerIDs, l)
			statusYears, statusPeriods = append(statusYears, years[i]), append(statusPeriods, periods[i])
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO ledger.period_status (tenant_id, id, legal_entity_id, ledger_id, calendar_id, fiscal_year, period, status, updated_by)
		SELECT $1, i, $2, l, $3, y, n, 'closed', $4 FROM unnest($5::uuid[], $6::uuid[], $7::smallint[], $8::smallint[]) AS x(i, l, y, n)
		ON CONFLICT DO NOTHING`, p.TenantID, legalEntity, calendar, p.ID, ids, ledgerIDs, statusYears, statusPeriods)
	if err != nil {
		return fmt.Errorf("ledger: fiscal calendar %s: period status of %s: %w", calendar, legalEntity, err)
	}
	return nil
}

type period struct {
	period     int32
	start, end time.Time
	special    bool
}

// fiscalYear returns the periods of fiscal year y of a calendar whose years
// start in month m: twelve monthly periods, then four special periods
// sharing the last period's dates. The year is named after the calendar
// year it ends in.
func fiscalYear(y, m int) []period {
	start := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	if m > 1 {
		start = start.AddDate(-1, 0, 0)
	}
	out := make([]period, 0, normalPeriods+specialPeriods)
	for i := range normalPeriods {
		s := start.AddDate(0, i, 0)
		out = append(out, period{period: int32(i + 1), start: s, end: s.AddDate(0, 1, -1)})
	}
	last := out[normalPeriods-1]
	for i := range specialPeriods {
		out = append(out, period{period: int32(normalPeriods + i + 1), start: last.start, end: last.end, special: true})
	}
	return out
}

// Accounts answers the partner context's questions about accounts.
type Accounts struct{}

// AccountNumbers returns the accounts' numbers by id, for the partner
// context to name the reconciliation accounts its company data holds by
// id (T74a); an id of no account is left out.
func (Accounts) AccountNumbers(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	names, err := AssetReads{}.AccountNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(names))
	for id, a := range names {
		out[id] = a.Number
	}
	return out, nil
}

// IsReconciliationAccount reports whether the account is an active
// reconciliation account of the kind with settings in the legal entity. The
// down payment account is not one: it is an alternative reconciliation
// account that takes only special G/L items (ADR-0037), never a partner's
// own, whose invoices it would refuse.
func (Accounts) IsReconciliationAccount(ctx context.Context, legalEntity, account uuid.UUID, kind string) (bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM ledger.gl_account a
		JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id AND ca.legal_entity_id = $1
		WHERE a.id = $2 AND a.reconciliation_kind = $3 AND a.status = 'active'
			AND NOT EXISTS (SELECT FROM ledger.account_determination d WHERE d.tenant_id = a.tenant_id AND d.legal_entity_id = $1
				AND d.key = 'down_payment' AND d.account_id = a.id))`, legalEntity, account, kind).Scan(&ok)
	if err != nil {
		return ok, fmt.Errorf("ledger: is account %s a %s reconciliation account in %s: %w", account, kind, legalEntity, err)
	}
	return ok, nil
}
