package ledger

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	problem.RegisterConstraint("tax_rate_in_use", func() *problem.Problem {
		return problem.Conflict("ledger.tax_rate_in_use", "A tax rate that postings used cannot change or be withdrawn, and a new rate cannot "+
			"cover dates already posted with its code: the past is never re-rated. Date the change after the last posting of the code "+
			"(a code in the single-rate form changes on the day of activation, so activate it on a day with no posting of the code from then on).")
	})
}

// taxCodes makes the legal entity's tax codes the company's (T61a,
// ADR-0049 §3): each code's definition and components are inserted or
// updated in place, and its dated rates are made the document's: a rate
// the document no longer has, or has with another value, is withdrawn and
// the document's inserted; a changed end is moved. The database refuses
// any of it that would re-rate a posting (409 ledger.tax_rate_in_use). A
// code or component the document no longer has keeps its rows, its rates
// ending today. The single-rate form's undated rate (config.TaxCode) is
// kept in force while unchanged; a new code starts with the company's
// first fiscal period, a changed rate today.
func taxCodes(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, calendar, chart uuid.UUID, co config.Company) error {
	// Postings hold their codes' definitions FOR KEY SHARE (lockTaxCodes):
	// this waits for those in flight, so the rate check sees their lines.
	if _, err := tx.Exec(ctx, `SELECT FROM ledger.tax_code_definition WHERE legal_entity_id = $1 ORDER BY code FOR UPDATE`, le); err != nil {
		return fmt.Errorf("ledger: lock tax codes of %s: %w", le, err)
	}
	if err := taxDefinitions(ctx, tx, p, le, chart, co); err != nil {
		return err
	}
	if err := requireTaxCodesFixed(ctx, tx, le, co); err != nil {
		return err
	}
	have, err := loadTaxRates(ctx, tx, le, calendar, true)
	if err != nil {
		return err
	}
	return have.apply(ctx, tx, p, le, have.plan(co))
}

// checkTaxRatesUnposted reports, on submission, the rate changes the
// activation would refuse with 409 ledger.tax_rate_in_use (T61a): it plans
// the change as the activation does, today, and finds the planned steps
// that would move a posting of the code into or out of a rate, so a change
// request that re-rates the past is refused before two approvers approve
// it. A posting or a later day of activation can still make the activation
// refuse what passed here; the database stays the rule.
func checkTaxRatesUnposted(ctx context.Context, tx pgx.Tx, p string, le, calendar uuid.UUID, co config.Company, out *findings) error {
	have, err := loadTaxRates(ctx, tx, le, calendar, false)
	if err != nil {
		return err
	}
	touched := have.plan(co).touched
	codes, keys, los, his := make([]string, len(touched)), make([]string, len(touched)), make([]time.Time, len(touched)), make([]*time.Time, len(touched))
	for i, s := range touched {
		codes[i], keys[i], los[i], his[i] = s.code, s.key, s.lo, s.hi
	}
	// The predicate is the trigger's (ledger.check_tax_rate); the last
	// posting tells the user which day a change may start after.
	rows, _ := tx.Query(ctx, `SELECT DISTINCT x.code, x.component, (SELECT max(l.posting_date) FROM ledger.journal_line l
			WHERE l.legal_entity_id = $1 AND l.tax_code = x.code)
		FROM unnest($2::text[], $3::text[], $4::date[], $5::date[]) AS x(code, component, lo, hi)
		JOIN ledger.tax_code_definition d ON d.legal_entity_id = $1 AND d.code = x.code
		WHERE ledger.tax_code_posted(d.tenant_id, $1, x.code, x.lo, x.hi)
		ORDER BY 1, 2`, le, codes, keys, los, his)
	type used struct {
		Code, Key string
		Last      time.Time
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[used])
	if err != nil {
		return fmt.Errorf("ledger: check company %s: tax rates in use: %w", co.Code, err)
	}
	for _, u := range found {
		out.add(ratePath(p, co, u.Code, u.Key), "the change would re-rate postings of tax code %s (its last posting is dated %s): "+
			"a rate postings used never changes, is withdrawn or ends before them, and no new rate covers posted dates; date the change after "+
			"the last posting (a code in the single-rate form, or one removed, changes on the day of activation: submit it on a day after the "+
			"last posting)", u.Code, u.Last.Format(time.DateOnly))
	}
	return nil
}

// ratePath is where a rate finding points: the component's rates, or the
// code or the list when the document no longer has the component or code.
func ratePath(p string, co config.Company, code, key string) string {
	i := slices.IndexFunc(co.TaxCodes, func(t config.TaxCode) bool { return t.Code == code })
	switch {
	case i < 0:
		return p + ".tax_codes"
	case !slices.ContainsFunc(co.TaxCodes[i].Parts(), func(c config.TaxComponent) bool { return c.Key == key }):
		return p + ".tax_codes[" + code + "].components"
	}
	return p + ".tax_codes[" + code + "].components[" + key + "].rates"
}

// taxDefinitions inserts or updates the codes' definitions and components.
// A direction or account key never changes (the database grants no update
// of either), which requireTaxCodesFixed reports.
func taxDefinitions(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le, chart uuid.UUID, co config.Company) error {
	var codes, descriptions, directions, kinds, categories []string
	var cCodes, keys, jurisdictions, accountKeys, recoverables, numbers []string
	for _, t := range co.TaxCodes {
		codes, descriptions, directions = append(codes, t.Code), append(descriptions, t.Description), append(directions, t.DirectionOrInput())
		kinds, categories = append(kinds, t.KindOrStandard()), append(categories, t.Category)
		for _, c := range t.Parts() {
			cCodes, keys, jurisdictions = append(cCodes, t.Code), append(keys, c.Key), append(jurisdictions, c.Jurisdiction)
			accountKeys, recoverables, numbers = append(accountKeys, c.AccountKey), append(recoverables, c.Recoverable()), append(numbers, c.Account)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.tax_code_definition AS d (tenant_id, legal_entity_id, code, description, direction, kind, category,
			created_by, updated_by)
		SELECT $1, $2, x.code, x.description, x.direction, x.kind, nullif(x.category, ''), $3, $3
		FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[]) AS x(code, description, direction, kind, category)
		ON CONFLICT (tenant_id, legal_entity_id, code) DO UPDATE SET description = EXCLUDED.description, kind = EXCLUDED.kind,
			category = EXCLUDED.category, version = d.version + 1, updated_by = $3
		WHERE (d.description, d.kind, d.category) IS DISTINCT FROM (EXCLUDED.description, EXCLUDED.kind, EXCLUDED.category)`,
		p.TenantID, le, p.ID, codes, descriptions, directions, kinds, categories); err != nil {
		return fmt.Errorf("ledger: tax code definitions of %s: %w", le, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.tax_component AS c (tenant_id, legal_entity_id, code, component, jurisdiction, account_key,
			recoverable_pct, account_id, created_by, updated_by)
		SELECT $1, $2, x.code, x.component, nullif(x.jurisdiction, ''), x.account_key, x.recoverable::numeric, g.id, $3, $3
		FROM unnest($4::text[], $5::text[], $6::text[], $7::text[], $8::text[], $9::text[])
			AS x(code, component, jurisdiction, account_key, recoverable, number)
		LEFT JOIN ledger.gl_account g ON g.chart_id = $10 AND g.number = x.number
		ON CONFLICT (tenant_id, legal_entity_id, code, component) DO UPDATE SET jurisdiction = EXCLUDED.jurisdiction,
			recoverable_pct = EXCLUDED.recoverable_pct, account_id = EXCLUDED.account_id, version = c.version + 1, updated_by = $3
		WHERE (c.jurisdiction, c.recoverable_pct, c.account_id) IS DISTINCT FROM (EXCLUDED.jurisdiction, EXCLUDED.recoverable_pct, EXCLUDED.account_id)`,
		p.TenantID, le, p.ID, cCodes, keys, jurisdictions, accountKeys, recoverables, numbers, chart); err != nil {
		return fmt.Errorf("ledger: tax code components of %s: %w", le, err)
	}
	return nil
}

// taxCodesFixed are the document's tax code settings that differ from what
// the legal entity has and never change: a code's direction and a
// component's account key, as paths of the company's document.
func taxCodesFixed(ctx context.Context, tx pgx.Tx, le uuid.UUID, co config.Company) ([]string, error) {
	var codes, directions, cCodes, keys, accountKeys []string
	for _, t := range co.TaxCodes {
		codes, directions = append(codes, t.Code), append(directions, t.DirectionOrInput())
		for _, c := range t.Parts() {
			cCodes, keys, accountKeys = append(cCodes, t.Code), append(keys, c.Key), append(accountKeys, c.AccountKey)
		}
	}
	rows, _ := tx.Query(ctx, `SELECT x.code, '.direction' FROM ledger.tax_code_definition d
			JOIN unnest($2::text[], $3::text[]) AS x(code, direction) ON x.code = d.code
		WHERE d.legal_entity_id = $1 AND d.direction <> x.direction
		UNION ALL
		SELECT x.code, '.components[' || x.component || '].account_key' FROM ledger.tax_component c
			JOIN unnest($4::text[], $5::text[], $6::text[]) AS x(code, component, account_key) ON x.code = c.code AND x.component = c.component
		WHERE c.legal_entity_id = $1 AND c.account_key <> x.account_key
		ORDER BY 1, 2`, le, codes, directions, cCodes, keys, accountKeys)
	type setting struct{ Code, Field string }
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[setting])
	if err != nil {
		return nil, fmt.Errorf("ledger: tax codes of %s: fixed settings: %w", le, err)
	}
	fixed := make([]string, len(found))
	for i, f := range found {
		fixed[i] = "[" + f.Code + "]" + f.Field
	}
	return fixed, nil
}

// checkTaxCodesFixed reports, on submission, a tax code direction or
// component account key the proposal changes.
func checkTaxCodesFixed(ctx context.Context, tx pgx.Tx, p string, le uuid.UUID, co config.Company, out *findings) error {
	fixed, err := taxCodesFixed(ctx, tx, le, co)
	if err != nil {
		return err
	}
	for _, f := range fixed {
		out.add(p+".tax_codes"+f, "a tax code's direction and its components' account keys never change once activated: they are what its postings were; "+
			"use a new code or component")
	}
	return nil
}

// requireTaxCodesFixed refuses activating what checkTaxCodesFixed reports,
// should it have been activated meanwhile (409 ledger.tax_code_fixed).
func requireTaxCodesFixed(ctx context.Context, tx pgx.Tx, le uuid.UUID, co config.Company) error {
	fixed, err := taxCodesFixed(ctx, tx, le, co)
	if err != nil || len(fixed) == 0 {
		return err
	}
	return problem.Conflict("ledger.tax_code_fixed", "Company "+co.Code+" changes tax code settings that never change once activated: "+
		strings.Join(fixed, ", ")+".")
}

// taxRate is a live (not withdrawn) rate row; to is exclusive, nil when
// open.
type taxRate struct {
	id   uuid.UUID
	code string
	key  string
	from time.Time
	to   *time.Time
	rate string
}

// taxRates are the legal entity's live rates by code and component, with
// the day and the dates a single-rate code starts on.
type taxRates struct {
	byKey map[[2]string][]taxRate
	// started are the codes that ever had a rate, withdrawn ones included.
	started map[string]bool
	today   time.Time
	first   time.Time // the company's first fiscal period, else today
}

const (
	taxRatesQuery = `SELECT id, code, component, valid_from, valid_to, rate_pct::text, withdrawn_at IS NOT NULL FROM ledger.tax_rate
		WHERE legal_entity_id = $1 ORDER BY code, component, valid_from`
	taxRatesForUpdate = taxRatesQuery + ` FOR UPDATE`
)

// loadTaxRates reads the legal entity's rates, locking them for an
// activation (lock); a submission's check reads them without holding up
// the postings.
func loadTaxRates(ctx context.Context, tx pgx.Tx, le, calendar uuid.UUID, lock bool) (taxRates, error) {
	h := taxRates{byKey: map[[2]string][]taxRate{}, started: map[string]bool{}}
	if err := tx.QueryRow(ctx, `SELECT current_date, coalesce((SELECT min(start_date) FROM ledger.fiscal_period WHERE calendar_id = $1), current_date)`,
		calendar).Scan(&h.today, &h.first); err != nil {
		return h, fmt.Errorf("ledger: tax rates of %s: today: %w", le, err)
	}
	query := taxRatesQuery
	if lock {
		query = taxRatesForUpdate
	}
	rows, _ := tx.Query(ctx, query, le)
	var r taxRate
	var withdrawn bool
	_, err := pgx.ForEachRow(rows, []any{&r.id, &r.code, &r.key, &r.from, &r.to, &r.rate, &withdrawn}, func() error {
		h.started[r.code] = true
		if !withdrawn {
			k := [2]string{r.code, r.key}
			h.byKey[k] = append(h.byKey[k], r)
		}
		return nil
	})
	if err != nil {
		return h, fmt.Errorf("ledger: tax rates of %s: %w", le, err)
	}
	return h, nil
}

// ratePlan is what makes the live rates the desired ones: withdrawals
// first, then moved ends, then new rates, so no two live rates of a
// component overlap at any step (both sets are free of overlaps, and the
// rates kept start where they did).
type ratePlan struct {
	withdraw []uuid.UUID
	ends     []taxRate // id and the new end
	insert   []taxRate
	// touched are the dates each step moves into or out of a rate: a
	// posting of the code in one is what the database refuses (409
	// ledger.tax_rate_in_use), which checkTaxRatesUnposted predicts.
	touched []rateSpan
}

// rateSpan is a component's dates [lo, hi), hi nil when open.
type rateSpan struct {
	code, key string
	lo        time.Time
	hi        *time.Time
}

// plan compares the live rates with the document's.
func (h taxRates) plan(co config.Company) ratePlan {
	want := map[[2]string][]taxRate{}
	for _, t := range co.TaxCodes {
		for _, c := range t.Parts() {
			k := [2]string{t.Code, c.Key}
			want[k] = h.desired(k, c)
		}
	}
	for k, have := range h.byKey {
		if _, ok := want[k]; !ok {
			want[k] = h.clip(have)
		}
	}
	var out ratePlan
	for k, rates := range want {
		out.add(h.byKey[k], rates)
	}
	return out
}

// desired are a component's rates as the document wants them.
func (h taxRates) desired(k [2]string, c config.TaxComponent) []taxRate {
	if len(c.Rates) == 1 && c.Rates[0].ValidFrom == "" {
		return h.legacy(k, c.Rates[0].RatePercent)
	}
	out := make([]taxRate, 0, len(c.Rates))
	for _, r := range c.Rates {
		from, _ := time.Parse(time.DateOnly, r.ValidFrom) // validated (config.CheckTaxCodes)
		var to *time.Time
		if r.ValidTo != "" {
			last, _ := time.Parse(time.DateOnly, r.ValidTo)
			end := last.AddDate(0, 0, 1)
			to = &end
		}
		out = append(out, taxRate{code: k[0], key: k[1], from: from, to: to, rate: r.RatePercent})
	}
	return out
}

// legacy is the single-rate form's schedule: the rate in force today kept
// open when it is the document's, else the live rates ending today and the
// document's from today; a new code's from the first fiscal period.
func (h taxRates) legacy(k [2]string, rate string) []taxRate {
	have := h.byKey[k]
	if len(have) == 0 && !h.started[k[0]] {
		return []taxRate{{code: k[0], key: k[1], from: h.first, rate: rate}}
	}
	for _, r := range have {
		if h.inForce(r) && sameRate(r.rate, rate) {
			var kept []taxRate
			for _, o := range have {
				if o.from.Before(r.from) {
					kept = append(kept, o)
				}
			}
			r.to = nil
			return append(kept, r)
		}
	}
	return append(h.clip(have), taxRate{code: k[0], key: k[1], from: h.today, rate: rate})
}

func (h taxRates) inForce(r taxRate) bool {
	return !r.from.After(h.today) && (r.to == nil || r.to.After(h.today))
}

// clip ends rates today: those ended by today are kept, the one in force
// ends today and those starting today or later go.
func (h taxRates) clip(have []taxRate) []taxRate {
	var out []taxRate
	for _, r := range have {
		if !r.from.Before(h.today) {
			continue
		}
		if r.to == nil || r.to.After(h.today) {
			r.to = new(h.today)
		}
		out = append(out, r)
	}
	return out
}

// add plans one component's change from the live rates to the wanted ones,
// matched by start: one with another rate is withdrawn and inserted anew.
func (p *ratePlan) add(have, want []taxRate) {
	kept := map[string]bool{} // by start date: dates compare by value, not by location
	for _, w := range want {
		i := slices.IndexFunc(have, func(r taxRate) bool { return r.from.Equal(w.from) })
		switch {
		case i < 0, !sameRate(have[i].rate, w.rate):
			p.insert = append(p.insert, w)
			p.touched = append(p.touched, rateSpan{w.code, w.key, w.from, w.to})
		default:
			kept[w.from.Format(time.DateOnly)] = true
			if !sameEnd(have[i].to, w.to) {
				end := have[i]
				end.to = w.to
				p.ends = append(p.ends, end)
				p.touched = append(p.touched, between(end, have[i].to, w.to))
			}
		}
	}
	for _, r := range have {
		if !kept[r.from.Format(time.DateOnly)] {
			p.withdraw = append(p.withdraw, r.id)
			p.touched = append(p.touched, rateSpan{r.code, r.key, r.from, r.to})
		}
	}
}

// between are the dates a rate's end moving from was to now passes over:
// from the earlier end to the later, open when either is (they differ).
func between(r taxRate, was, now *time.Time) rateSpan {
	switch {
	case was == nil:
		return rateSpan{r.code, r.key, *now, nil}
	case now == nil:
		return rateSpan{r.code, r.key, *was, nil}
	case now.Before(*was):
		return rateSpan{r.code, r.key, *now, was}
	default:
		return rateSpan{r.code, r.key, *was, now}
	}
}

func sameRate(a, b string) bool { return money.ParseRat(a).Cmp(money.ParseRat(b)) == 0 }

func sameEnd(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }

// apply carries out the plan in its order.
func (h taxRates) apply(ctx context.Context, tx pgx.Tx, p reqctx.Principal, le uuid.UUID, plan ratePlan) error {
	if _, err := tx.Exec(ctx, `UPDATE ledger.tax_rate SET withdrawn_at = now(), withdrawn_by = $2 WHERE id = ANY($1)`, plan.withdraw, p.ID); err != nil {
		return fmt.Errorf("ledger: withdraw tax rates of %s: %w", le, err)
	}
	ids, ends := make([]uuid.UUID, len(plan.ends)), make([]*time.Time, len(plan.ends))
	for i, r := range plan.ends {
		ids[i], ends[i] = r.id, r.to
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger.tax_rate t SET valid_to = x.valid_to, ended_at = now(), ended_by = $3
		FROM unnest($1::uuid[], $2::date[]) AS x(id, valid_to) WHERE t.id = x.id`, ids, ends, p.ID); err != nil {
		return fmt.Errorf("ledger: end tax rates of %s: %w", le, err)
	}
	n := len(plan.insert)
	newIDs, codes, keys, froms, tos, rates := make([]uuid.UUID, n), make([]string, n), make([]string, n), make([]time.Time, n), make([]*time.Time, n), make([]string, n)
	for i, r := range plan.insert {
		newIDs[i], codes[i], keys[i], froms[i], tos[i], rates[i] = uuid.Must(uuid.NewV7()), r.code, r.key, r.from, r.to, r.rate
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, valid_to, rate_pct, created_by)
		SELECT $1, x.id, $2, x.code, x.component, x.valid_from, x.valid_to, x.rate::numeric, $3
		FROM unnest($4::uuid[], $5::text[], $6::text[], $7::date[], $8::date[], $9::text[]) AS x(id, code, component, valid_from, valid_to, rate)`,
		p.TenantID, le, p.ID, newIDs, codes, keys, froms, tos, rates); err != nil {
		return fmt.Errorf("ledger: start tax rates of %s: %w", le, err)
	}
	return nil
}
