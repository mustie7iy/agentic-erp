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
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Setup is the configuration document's ledger section: each company's
// chart of accounts and account determination, and its fiscal calendar
// with the periods of every fiscal year that has a number range.
//
// The mapping, decided in T3 (docs/design/07-transition.md):
//   - the tenant gets a leading ledger 0L (IFRS) with its first configuration;
//   - each company has its own chart, coded with the company code; accounts
//     removed from the document become inactive, and an account's type and
//     reconciliation kind never change once activated;
//   - the legal entity's fiscal_calendar_id names its calendar (12 normal and
//     4 special periods), created with the start month of the document;
//     fiscal years are named after the calendar year they end in, so with
//     start month 4 fiscal year 2026 runs from 1 April 2025 to 31 March 2026;
//   - a fiscal year exists for a company once the document has a number
//     range for it; its periods are created closed in every ledger and
//     opened with fiscal-periods/{id}:open;
//   - tax codes (input tax, decided in T4) are valid from the start of the
//     company's first fiscal year; a changed rate or account ends the
//     current one and starts the new one on the activation day, and a
//     removed code ends on that day;
//   - withholding tax codes (T46e) are kept per legal entity, changed in
//     place (what an invoice withheld is kept with its entry, T46f) and
//     deactivated when removed, which the partner context refuses while a
//     supplier has the code (partner.WithholdingCodes).
type Setup struct {
	LegalEntities LegalEntities
}

const (
	normalPeriods  = 12
	specialPeriods = 4
)

var accountNumber = regexp.MustCompile(`^[0-9A-Z]{1,10}$`)

// Check reports what the ledger cannot apply: companies without a legal
// entity, a currency that is not the legal entity's, account numbers the
// ledger does not accept, a start month that contradicts the legal
// entity's existing calendar, and changes to activated accounts.
func (s Setup) Check(ctx context.Context, base, proposed *config.Config) ([]config.Finding, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	var out findings
	for _, co := range proposed.Companies {
		if err := s.checkCompany(ctx, tx, co, &out); err != nil {
			return nil, err
		}
	}
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
// month of its calendar, tax codes already changed today, and a new GR/IR
// account that is already posted.
func (s Setup) checkCompany(ctx context.Context, tx pgx.Tx, co config.Company, out *findings) error {
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
	if err := checkTaxCodesChangedToday(ctx, tx, p, le.ID, co, out); err != nil {
		return err
	}
	return checkGRIRUnposted(ctx, tx, p, le.ID, co, out)
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

// checkTaxCodesChangedToday reports tax codes the proposal changes or
// removes that already changed today: a tax code changes at most once a
// day.
func checkTaxCodesChangedToday(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, out *findings) error {
	var codes, rates, numbers []string
	for _, t := range co.TaxCodes {
		codes, rates, numbers = append(codes, t.Code), append(rates, t.RatePercent), append(numbers, t.Account)
	}
	rows, _ := tx.Query(ctx, `SELECT t.code FROM ledger.tax_code t JOIN ledger.gl_account g ON g.id = t.account_id
		LEFT JOIN unnest($2::text[], $3::text[], $4::text[]) AS d(code, rate, number) ON d.code = t.code
		WHERE t.legal_entity_id = $1 AND t.valid_to IS NULL AND t.valid_from = current_date
			AND (d.code IS NULL OR d.rate::numeric <> t.rate_pct OR d.number <> g.number) ORDER BY 1`, le, codes, rates, numbers)
	today, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("ledger: check company %s: tax codes changed today: %w", co.Code, err)
	}
	for _, code := range today {
		out.add(p+".tax_codes["+code+"]", "the tax code already changed today; change it again from tomorrow")
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
		for _, was := range old.ChartOfAccounts {
			a := co.Account(was.Number)
			switch {
			case a == nil: // removing an account makes it inactive
			case a.Type != was.Type:
				out.add(p+".chart_of_accounts["+was.Number+"].type", "an account's type cannot change once activated")
			case a.VendorReconciliation != was.VendorReconciliation:
				out.add(p+".chart_of_accounts["+was.Number+"].vendor_reconciliation", "an account's reconciliation kind cannot change once activated")
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
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.ledger (tenant_id, id, code, name, is_leading, accounting_principle, created_by)
		SELECT $1, $2, '0L', 'Leading ledger', true, 'IFRS', $3 WHERE NOT EXISTS (SELECT FROM ledger.ledger)`,
		p.TenantID, uuid.Must(uuid.NewV7()), p.ID); err != nil {
		return fmt.Errorf("ledger: apply leading ledger: %w", err)
	}
	for _, co := range doc.Companies {
		if err := s.applyCompany(ctx, tx, p, co, doc); err != nil {
			return err
		}
	}
	return nil
}

// applyCompany makes one company's chart of accounts, account
// determination, fiscal calendar and tax codes match the document.
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
	ad := co.AccountDetermination
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.account_determination AS d (tenant_id, legal_entity_id, key, account_id, updated_by)
		SELECT $1, $2, k, g.id, $4 FROM unnest($5::text[], $6::text[]) AS x(k, n) JOIN ledger.gl_account g ON g.chart_id = $3 AND g.number = x.n
		ON CONFLICT (tenant_id, legal_entity_id, key) DO UPDATE SET account_id = EXCLUDED.account_id, version = d.version + 1, updated_by = $4
		WHERE d.account_id <> EXCLUDED.account_id`,
		p.TenantID, le.ID, chart, p.ID, []string{"ap_reconciliation", "input_tax", "bank", "fx_gain", "fx_loss", "gr_ir", "price_difference",
			"cash_discount", "down_payment"}, []string{ad.APReconciliation, ad.InputTax, ad.Bank, ad.FXGain, ad.FXLoss, ad.GRIR, ad.PriceDifference,
			ad.CashDiscount, ad.DownPayment}); err != nil {
		return fmt.Errorf("ledger: apply company %s: account determination: %w", co.Code, err)
	}
	if err := s.calendar(ctx, tx, p, le.FiscalCalendarID, le.ID, co, doc); err != nil {
		return err
	}
	if err := taxCodes(ctx, tx, p, le.ID, le.FiscalCalendarID, chart, co); err != nil {
		return err
	}
	return withholdingTaxCodes(ctx, tx, p, le.ID, chart, co)
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
	var numbers, names, types, kinds []string
	for _, a := range co.ChartOfAccounts {
		kind := "none"
		if a.VendorReconciliation {
			kind = "supplier"
		}
		ids = append(ids, uuid.Must(uuid.NewV7()))
		numbers, names, types, kinds = append(numbers, a.Number), append(names, a.Name), append(types, string(a.Type)), append(kinds, kind)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.gl_account AS g (tenant_id, id, chart_id, number, name, account_type, reconciliation_kind,
			status, created_by, updated_by)
		SELECT $1, i, $2, n, nm, t, k, 'active', $3, $3 FROM unnest($4::uuid[], $5::text[], $6::text[], $7::text[], $8::text[]) AS a(i, n, nm, t, k)
		ON CONFLICT (tenant_id, chart_id, number) DO UPDATE SET name = EXCLUDED.name, status = 'active', version = g.version + 1, updated_by = $3
		WHERE (g.name, g.status) IS DISTINCT FROM (EXCLUDED.name, 'active')`,
		p.TenantID, chart, p.ID, ids, numbers, names, types, kinds); err != nil {
		return fmt.Errorf("ledger: apply company %s: accounts: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.gl_account SET status = 'inactive', version = version + 1, updated_by = $3
		WHERE chart_id = $1 AND status <> 'inactive' AND NOT number = ANY($2)`, chart, numbers, p.ID); err != nil {
		return fmt.Errorf("ledger: apply company %s: deactivate accounts: %w", co.Code, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.company_account (tenant_id, legal_entity_id, account_id, is_open_item_managed, created_by, updated_by)
		SELECT $1, $2, id, reconciliation_kind <> 'none', $4, $4 FROM ledger.gl_account WHERE chart_id = $3 AND number = ANY($5)
		ON CONFLICT DO NOTHING`, p.TenantID, le, chart, p.ID, numbers); err != nil {
		return fmt.Errorf("ledger: apply company %s: company accounts: %w", co.Code, err)
	}
	return nil
}

// taxCodes makes the legal entity's tax codes valid today match the
// company's, ending changed and removed ones today.
func taxCodes(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, calendar, chart uuid.UUID, co config.Company) error {
	var codes, descriptions, rates, numbers []string
	for _, t := range co.TaxCodes {
		codes, descriptions, rates, numbers = append(codes, t.Code), append(descriptions, t.Description), append(rates, t.RatePercent), append(numbers, t.Account)
	}
	const doc = `doc AS (SELECT d.code, d.description, d.rate::numeric AS rate, g.id AS account_id
		FROM unnest($3::text[], $4::text[], $5::text[], $6::text[]) AS d(code, description, rate, number)
		JOIN ledger.gl_account g ON g.chart_id = $7 AND g.number = d.number)`
	if _, err := tx.Exec(ctx, `WITH `+doc+`
		UPDATE ledger.tax_code t SET valid_to = current_date, ended_by = $2, ended_at = now()
		WHERE t.legal_entity_id = $1 AND t.valid_to IS NULL AND t.valid_from < current_date
			AND NOT EXISTS (SELECT FROM doc WHERE doc.code = t.code AND doc.rate = t.rate_pct AND doc.account_id = t.account_id)`,
		le, p.ID, codes, descriptions, rates, numbers, chart); err != nil {
		return fmt.Errorf("ledger: end tax codes of %s: %w", le, err)
	}
	_, err := tx.Exec(ctx, `WITH `+doc+`
		INSERT INTO ledger.tax_code (tenant_id, legal_entity_id, code, description, direction, rate_pct, account_id, valid_from, created_by)
		SELECT $8, $1, doc.code, doc.description, 'input', doc.rate, doc.account_id,
			CASE WHEN EXISTS (SELECT FROM ledger.tax_code t WHERE t.legal_entity_id = $1 AND t.code = doc.code) THEN current_date
			ELSE coalesce((SELECT min(start_date) FROM ledger.fiscal_period WHERE calendar_id = $9), current_date) END, $2
		FROM doc WHERE NOT EXISTS (SELECT FROM ledger.tax_code t WHERE t.legal_entity_id = $1 AND t.code = doc.code
			AND (t.valid_to IS NULL OR t.valid_to > current_date))`,
		le, p.ID, codes, descriptions, rates, numbers, chart, p.TenantID, calendar)
	if err != nil {
		return fmt.Errorf("ledger: start tax codes of %s: %w", le, err)
	}
	return nil
}

// calendar creates the legal entity's fiscal calendar if needed and the
// periods of every fiscal year the document has a number range for, closed
// in every ledger.
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
	if err := tx.QueryRow(ctx, `SELECT array_agg(id ORDER BY id) FROM ledger.ledger`).Scan(&ledgers); err != nil {
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
