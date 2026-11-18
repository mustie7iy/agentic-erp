package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Statistical key figures, their postings and cycles whose shares come
// from them (T60j, migrations 000385-000386; ADR-0064 As built T60j).

const (
	keyFigureSQL = `INSERT INTO controlling.statistical_key_figure (tenant_id, id, legal_entity_id, code, name, unit, created_by, updated_by)
		VALUES ($1, $2, $3, $4, 'Employees', $5, $6, $6)`
	keyFigurePostingSQL = `INSERT INTO controlling.statistical_key_figure_posting (tenant_id, id, key_figure_id, legal_entity_id, fiscal_year, fiscal_period,
		created_by) VALUES ($1, $2, $3, $4, 2026, $5, $6)`
	keyFigureValueSQL = `INSERT INTO controlling.statistical_key_figure_value (tenant_id, posting_id, position, cost_center_id, value)
		VALUES ($1, $2, $3, $4, $5)`
	keyFigureCycleSQL = `INSERT INTO controlling.allocation_cycle (tenant_id, id, legal_entity_id, code, name, kind, sender_cost_center_id,
		share_basis, statistical_key_figure_id, created_by, updated_by) VALUES ($1, $2, $3, $4, 'Cycle', 'distribution', $5, $6, $7, $8, $8)`
	keyFigureReceiverSQL = `INSERT INTO controlling.allocation_receiver (tenant_id, cycle_id, position, cost_center_id) VALUES ($1, $2, $3, $4)`
)

func TestStatisticalKeyFigureRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	figure, posting, center := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, keyFigureSQL, f.tenant, figure, f.entity, "EMPL", "EA", f.actor); err != nil {
			return err
		}
		if err := exec(tx, keyFigurePostingSQL, f.tenant, posting, figure, f.entity, 5, f.actor); err != nil {
			return err
		}
		return exec(tx, keyFigureValueSQL, f.tenant, posting, 1, center, "3.5")
	})
	try := func(sql string, args ...any) error {
		return asApp(t, f.tenant, func(tx pgx.Tx) error { return exec(tx, sql, args...) })
	}
	for name, c := range map[string]struct {
		sql  string
		args []any
		want string
	}{
		"a code twice in a company":     {keyFigureSQL, []any{f.tenant, id(), f.entity, "EMPL", "EA", f.actor}, "statistical_key_figure_code_once"},
		"a code the form cannot hold":   {keyFigureSQL, []any{f.tenant, id(), f.entity, "empl", "EA", f.actor}, "statistical_key_figure_code_check"},
		"a unit that is none":           {keyFigureSQL, []any{f.tenant, id(), f.entity, "AREA", "SQFT", f.actor}, "statistical_key_figure_unit"},
		"a key figure's unit changed":   {`UPDATE controlling.statistical_key_figure SET unit = 'M2', version = version + 1 WHERE id = $1`, []any{figure}, "keeps its company, code and unit"},
		"a key figure's code changed":   {`UPDATE controlling.statistical_key_figure SET code = 'X', version = version + 1 WHERE id = $1`, []any{figure}, "keeps its company, code and unit"},
		"a key figure deleted":          {`DELETE FROM controlling.statistical_key_figure WHERE id = $1`, []any{figure}, "permission denied"},
		"a posting of no key figure":    {keyFigurePostingSQL, []any{f.tenant, id(), id(), f.entity, 5, f.actor}, "statistical_key_figure_posting_key_figure"},
		"a posting in another company":  {keyFigurePostingSQL, []any{f.tenant, id(), figure, id(), 5, f.actor}, "statistical_key_figure_posting_key_figure"},
		"a posting of a period of none": {keyFigurePostingSQL, []any{f.tenant, id(), figure, f.entity, 17, f.actor}, "statistical_key_figure_posting_fiscal_period_check"},
		"a posting changed":             {`UPDATE controlling.statistical_key_figure_posting SET fiscal_period = 6 WHERE id = $1`, []any{posting}, "permission denied"},
		"a posting deleted":             {`DELETE FROM controlling.statistical_key_figure_posting WHERE id = $1`, []any{posting}, "permission denied"},
		"a value of nothing":            {keyFigureValueSQL, []any{f.tenant, posting, 2, id(), "0"}, "statistical_key_figure_value_not_zero"},
		"a center twice in a posting":   {keyFigureValueSQL, []any{f.tenant, posting, 2, center, "1"}, "statistical_key_figure_value_once"},
		"a value of no posting":         {keyFigureValueSQL, []any{f.tenant, id(), 1, id(), "1"}, "statistical_key_figure_value_posting"},
		"a place beyond the last":       {keyFigureValueSQL, []any{f.tenant, posting, 501, id(), "1"}, "statistical_key_figure_value_position_check"},
		"a value changed":               {`UPDATE controlling.statistical_key_figure_value SET value = 4 WHERE posting_id = $1`, []any{posting}, "permission denied"},
		"a value deleted":               {`DELETE FROM controlling.statistical_key_figure_value WHERE posting_id = $1`, []any{posting}, "permission denied"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, try(c.sql, c.args...), c.want)
		})
	}
	t.Run("a key figure renamed, and its value posted negative", func(t *testing.T) {
		mustApp(t, f.tenant, func(tx pgx.Tx) error {
			if err := exec(tx, `UPDATE controlling.statistical_key_figure SET name = 'Headcount', version = version + 1 WHERE id = $1`, figure); err != nil {
				return err
			}
			other := id()
			if err := exec(tx, keyFigurePostingSQL, f.tenant, other, figure, f.entity, 5, f.actor); err != nil {
				return err
			}
			return exec(tx, keyFigureValueSQL, f.tenant, other, 1, center, "-0.000001")
		})
	})
	t.Run("the trigger holds for the owner", func(t *testing.T) {
		failsWith(t, asApp(t, f.tenant, asOwner(func(tx pgx.Tx) error {
			return exec(tx, `UPDATE controlling.statistical_key_figure SET unit = 'M2' WHERE id = $1`, figure)
		})), "keeps its company, code and unit")
	})
	t.Run("another tenant sees none", func(t *testing.T) {
		var n int
		mustApp(t, id(), func(tx pgx.Tx) error {
			return tx.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM controlling.statistical_key_figure)
				+ (SELECT count(*) FROM controlling.statistical_key_figure_posting) + (SELECT count(*) FROM controlling.statistical_key_figure_value)`).Scan(&n)
		})
		if n != 0 {
			t.Errorf("key figures, postings and values another tenant sees: got %d, want 0", n)
		}
	})
}

// keyFigureCycle writes a cycle by key figure of the fixture's company
// with receivers holding the shares given (nil for none), in one
// transaction, as the application does.
func (f fixture) keyFigureCycle(t *testing.T, basis string, figure *uuid.UUID, sender uuid.UUID, shares ...*int) error {
	t.Helper()
	cycle := id()
	return asApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, keyFigureCycleSQL, f.tenant, cycle, f.entity, "K"+strings.ToUpper(cycle.String()[30:]), sender, basis, figure, f.actor); err != nil {
			return err
		}
		for i, bp := range shares {
			sql, args := keyFigureReceiverSQL, []any{f.tenant, cycle, i + 1, id()}
			if bp != nil {
				sql, args = allocationReceiverSQL, append(args, *bp)
			}
			if err := exec(tx, sql, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestKeyFigureCycleRules(t *testing.T) {
	need(t)
	f := newFixture(t)
	figure, other, sender := id(), id(), id()
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		if err := exec(tx, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by,
			updated_by) VALUES ($1, $2, '2000', 'Acme DE', 'DE', 'EUR', $3, 'active', $4, $4)`, f.tenant, other, f.calendar, f.actor); err != nil {
			return err
		}
		if err := exec(tx, keyFigureSQL, f.tenant, figure, f.entity, "EMPL", "EA", f.actor); err != nil {
			return err
		}
		return exec(tx, keyFigureSQL, f.tenant, id(), other, "EMPL", "EA", f.actor)
	})
	whole, half := 10000, 5000
	if err := f.keyFigureCycle(t, "statistical_key_figure", &figure, sender, nil, nil); err != nil {
		t.Fatalf("a cycle by key figure with two receivers: %v", err)
	}
	var otherFigure uuid.UUID
	mustApp(t, f.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT id FROM controlling.statistical_key_figure WHERE legal_entity_id = $1`, other).Scan(&otherFigure)
	})
	for name, c := range map[string]struct {
		basis  string
		figure *uuid.UUID
		shares []*int
		want   string
	}{
		"a basis that is none":                   {"actual_values", nil, []*int{&whole}, "allocation_cycle_share_basis"},
		"a key figure basis without a figure":    {"statistical_key_figure", nil, []*int{nil}, "allocation_cycle_key_figure"},
		"fixed percentages with a figure":        {"fixed_percentages", &figure, []*int{&whole}, "allocation_cycle_key_figure"},
		"a key figure that is none":              {"statistical_key_figure", new(uuid.UUID), []*int{nil}, "allocation_cycle_key_figure_company"},
		"a key figure of another company":        {"statistical_key_figure", &otherFigure, []*int{nil}, "allocation_cycle_key_figure_company"},
		"a receiver with a share by key figure":  {"statistical_key_figure", &figure, []*int{nil, &half}, "none has a share of its own"},
		"a cycle by key figure without receiver": {"statistical_key_figure", &figure, nil, "none has a share of its own"},
		"a receiver without a share by percent":  {"fixed_percentages", nil, []*int{&whole, nil}, "or a receiver has none"},
	} {
		t.Run(name, func(t *testing.T) {
			failsWith(t, f.keyFigureCycle(t, c.basis, c.figure, sender, c.shares...), c.want)
		})
	}
	t.Run("a cycle's basis and key figure are kept", func(t *testing.T) {
		for name, set := range map[string]string{
			"the basis":      `share_basis = 'fixed_percentages', statistical_key_figure_id = NULL`,
			"the key figure": `statistical_key_figure_id = '` + id().String() + `'`,
		} {
			t.Run(name, func(t *testing.T) {
				failsWith(t, asApp(t, f.tenant, asOwner(func(tx pgx.Tx) error {
					return exec(tx, `UPDATE controlling.allocation_cycle SET `+set+` WHERE statistical_key_figure_id = $1`, figure)
				})), "share basis and key figure")
			})
		}
	})
}
