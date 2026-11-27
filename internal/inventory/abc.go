package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// The ABC analysis for cycle counting (S2c3; ADR-0082 As built S2c3; SAP
// MIBC, from memory): a plant's materials classed by their consumption
// value over a window, computed in SQL from the posted material documents
// and stored nowhere, each proposed the plant's indicator of its class's
// code. A person previews it (ABC) and applies it (ApplyABC, abc_apply.go):
// the apply computes it again under the plant's lock and makes its
// changes only when it is the analysis previewed, its digest (Token)
// unchanged, and keeps the application.

// The changes a line of the analysis makes.
const (
	ABCCreate = "create"
	ABCChange = "change"
	ABCNone   = "none"
)

const (
	// abcLock serializes the applies of a plant's analyses.
	abcLock = "inventory.cycle_count_abc"
	// maxABCDays is the longest window, a year with its leap day.
	maxABCDays = 366
)

// maxABCLines is the most materials an analysis lists, which its answer
// holds at once (the contract's maxItems); a variable so a test can
// lower it.
var maxABCLines = 1000

// abcClasses are the classes in their order, each proposed the plant's
// indicator of its own code.
var abcClasses = []string{"A", "B", "C"}

// abcConsumes are the movement types whose value is consumption at the
// plant (an issue to a cost center, a scrapping, a delivery's goods
// issue) and their reversals, which give it back, as the count planning
// agent counts it (ADR-0082 As built S2e).
var abcConsumes = []string{"201", "202", "551", "552", "601", "602"}

// ABCInput is the plant and window an analysis is of.
type ABCInput struct {
	PlantID  uuid.UUID
	From, To time.Time
}

// ABCLine is a material of the analysis: its consumption value and class,
// the plant's indicator of its class, the change applying it makes and
// the material's active indicator (Before), nil when it has none.
type ABCLine struct {
	MaterialID  uuid.UUID
	Consumption money.Minor
	Class       string
	Indicator   Indicator
	Change      string
	Before      *CycleMaterial
}

// ABCClass is a class's materials listed, their consumption value and the
// plant's indicator of its code, nil when it has none.
type ABCClass struct {
	Class       string
	Materials   int32
	Consumption money.Minor
	Indicator   *Indicator
}

// ABCAnalysis is a plant's analysis over its window.
type ABCAnalysis struct {
	Plant             tenancy.Unit
	From, To          time.Time
	Shares            config.ABCShares
	Currency          string
	Exponent          int
	Total, Excluded   money.Minor
	ExcludedMaterials int32
	Classes           []ABCClass
	Lines             []ABCLine
	Changes           int32
	Token             string
	Applied           bool
	materials         refs
	indicators        map[string]Indicator
	consumption       []abcRow
}

// abcRow is a material's consumption value at the plant and its class, as
// the database computes them.
type abcRow struct {
	material uuid.UUID
	value    money.Minor
	class    string
}

// abcSelect classes the materials with stock at the plant ($1) now or
// consumption there from $3 to $4 (the movement types $2 less their
// reversals; a net return is none) by their value, the greatest first,
// ties by id: A while the values before make up less than $5 percent of
// the plant's, B while less than $6 percent, else C, as is one with none.
// Sums of bigint are numeric, so the shares compare exactly.
const abcSelect = `WITH consumed AS (
		SELECT m.material_id, greatest(-sum(m.amount), 0)::bigint AS value FROM inventory.movement m
		JOIN inventory.material_document d ON d.tenant_id = m.tenant_id AND d.id = m.material_document_id
		WHERE m.plant_id = $1 AND d.movement_type = ANY($2) AND d.posting_date BETWEEN $3 AND $4
		GROUP BY m.material_id),
	held AS (SELECT DISTINCT material_id FROM inventory.stock WHERE plant_id = $1 AND quantity <> 0),
	listed AS (SELECT coalesce(c.material_id, h.material_id) AS material_id, coalesce(c.value, 0) AS value
		FROM consumed c FULL JOIN held h ON h.material_id = c.material_id)
	SELECT material_id, value, CASE WHEN value = 0 THEN 'C'
			WHEN 100 * coalesce(sum(value) OVER before, 0) < $5 * sum(value) OVER () THEN 'A'
			WHEN 100 * coalesce(sum(value) OVER before, 0) < $6 * sum(value) OVER () THEN 'B'
			ELSE 'C' END
	FROM listed
	WINDOW before AS (ORDER BY value DESC, material_id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
	ORDER BY value DESC, material_id`

// ABC computes the plant's analysis over the window: an active plant of a
// company the caller may act in, a window ending on or before its
// company's day today of at most maxABCDays.
func (e *Endpoints) ABC(ctx context.Context, in ABCInput) (ABCAnalysis, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return ABCAnalysis{}, err
	}
	return e.analyse(ctx, tx, in, false)
}

// analyse computes the analysis, holding the plant's materials' active
// indicators under their row locks with lock.
func (e *Endpoints) analyse(ctx context.Context, tx pgx.Tx, in ABCInput, lock bool) (ABCAnalysis, error) {
	r, err := e.read(ctx, nil, []uuid.UUID{in.PlantID}, nil, nil)
	if err != nil {
		return ABCAnalysis{}, err
	}
	plant, err := r.plant(in.PlantID)
	if err != nil {
		return ABCAnalysis{}, err
	}
	c, err := e.companyOf(ctx, plant.LegalEntityID)
	if err != nil {
		return ABCAnalysis{}, err
	}
	in.From, in.To = dayOf(in.From), dayOf(in.To)
	if err := abcWindow(in, dayOf(c.day.Date)); err != nil {
		return ABCAnalysis{}, err
	}
	a := ABCAnalysis{Plant: plant, From: in.From, To: in.To, Shares: c.co.ABC(), Currency: c.le.LocalCurrency}
	if a.Exponent, err = currency.Exponent(ctx, tx, a.Currency, nil); err != nil {
		return ABCAnalysis{}, fmt.Errorf("inventory: ABC analysis of plant %s: %w", plant.Code, err)
	}
	rows, _ := tx.Query(ctx, abcSelect, plant.ID, abcConsumes, in.From, in.To, a.Shares.A, a.Shares.A+a.Shares.B)
	a.consumption, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (abcRow, error) {
		var x abcRow
		return x, row.Scan(&x.material, &x.value, &x.class)
	})
	if err != nil {
		return ABCAnalysis{}, fmt.Errorf("inventory: consumption at plant %s: %w", plant.Code, err)
	}
	if err := e.list(ctx, &a); err != nil {
		return ABCAnalysis{}, err
	}
	if err := a.propose(ctx, tx, lock); err != nil {
		return ABCAnalysis{}, err
	}
	a.seal()
	return a, nil
}

// dayOf is the calendar day of t, as a date of the API and the database
// carries it.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// abcWindow checks the window, in days: from on or before to, to on or before the
// company's day today, at most maxABCDays (422 inventory.abc_window_invalid).
func abcWindow(in ABCInput, today time.Time) error {
	switch {
	case in.To.Before(in.From):
		return problem.Invalid("inventory.abc_window_invalid", "to", "The window ends before it starts.")
	case in.To.After(today):
		return problem.Invalid("inventory.abc_window_invalid", "to", "The window ends after the company's date today, "+today.Format(time.DateOnly)+".")
	case in.To.Sub(in.From) >= maxABCDays*24*time.Hour:
		return problem.Invalid("inventory.abc_window_invalid", "from", fmt.Sprintf("The window spans more than %d days.", maxABCDays))
	}
	return nil
}

// list keeps the materials a count document takes at the plant as lines
// and adds the others' consumption to the excluded; the total is every
// material's.
func (e *Endpoints) list(ctx context.Context, a *ABCAnalysis) error {
	ids := make([]uuid.UUID, len(a.consumption))
	for i, x := range a.consumption {
		ids[i] = x.material
	}
	r, err := e.read(ctx, ids, []uuid.UUID{a.Plant.ID}, nil, []uuid.UUID{a.Plant.LegalEntityID})
	if err != nil {
		return err
	}
	a.materials = r
	return a.tally(func(m uuid.UUID) bool { return r.canCount(m, a.Plant.ID) })
}

// tally lists the materials countable says a count takes, at most
// maxABCLines (422 inventory.abc_too_many_materials), and sums every
// material's consumption and the excluded's, which stay within an
// amount's range (ADR-0005).
func (a *ABCAnalysis) tally(countable func(uuid.UUID) bool) error {
	values := make([]money.Minor, len(a.consumption))
	var excluded []money.Minor
	for i, x := range a.consumption {
		values[i] = x.value
		if !countable(x.material) {
			if x.value > 0 {
				a.ExcludedMaterials++
				excluded = append(excluded, x.value)
			}
			continue
		}
		a.Lines = append(a.Lines, ABCLine{MaterialID: x.material, Consumption: x.value, Class: x.class})
	}
	if len(a.Lines) > maxABCLines {
		return problem.Invalid("inventory.abc_too_many_materials", "plant_id", fmt.Sprintf("Plant %s has %d materials to class; an analysis lists at most %d.",
			a.Plant.Code, len(a.Lines), maxABCLines))
	}
	var err error
	if a.Total, err = money.Sum(values...); err != nil {
		return fmt.Errorf("inventory: consumption at plant %s: %w", a.Plant.Code, err)
	}
	a.Excluded, err = money.Sum(excluded...)
	return errs.Wrapf(err, "inventory: excluded consumption at plant %s", a.Plant.Code)
}

// propose gives each line the plant's indicator of its class (422
// inventory.abc_indicator_missing for a class with lines and none) and the
// change applying it makes against the material's active indicator.
func (a *ABCAnalysis) propose(ctx context.Context, tx pgx.Tx, lock bool) error {
	rows, _ := tx.Query(ctx, indicatorSelect+` WHERE plant_id = $1 AND code = ANY($2) ORDER BY code`, a.Plant.ID, abcClasses)
	is, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Indicator, error) { return scanIndicator(r) })
	if err != nil {
		return fmt.Errorf("inventory: ABC indicators of plant %s: %w", a.Plant.Code, err)
	}
	a.indicators = map[string]Indicator{}
	for _, i := range is {
		a.indicators[i.Code] = i
	}
	current, err := a.current(ctx, tx, lock)
	if err != nil {
		return err
	}
	var missing []string
	for i := range a.Lines {
		l := &a.Lines[i]
		ind, ok := a.indicators[l.Class]
		if !ok {
			if !slices.Contains(missing, l.Class) {
				missing = append(missing, l.Class)
			}
			continue
		}
		l.Indicator, l.Change = ind, ABCCreate
		if c, ok := current[l.MaterialID]; ok {
			l.Before, l.Change = &c, ABCChange
			if c.IndicatorID == ind.ID {
				l.Change = ABCNone
			}
		}
		if l.Change != ABCNone {
			a.Changes++
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return problem.Invalid("inventory.abc_indicator_missing", "plant_id", "Plant "+a.Plant.Code+" has no cycle counting indicator "+
			strings.Join(missing, ", ")+" for its materials of that class; create it first.")
	}
	a.classes()
	return nil
}

// current reads the listed materials' active indicators at the plant, by
// material, with lock under their row locks, taken in the order of their
// ids by themselves first (as loadCycleMaterial takes one, so a row a
// writer moved to another indicator is still found).
func (a *ABCAnalysis) current(ctx context.Context, tx pgx.Tx, lock bool) (map[uuid.UUID]CycleMaterial, error) {
	ids := make([]uuid.UUID, len(a.Lines))
	for i, l := range a.Lines {
		ids[i] = l.MaterialID
	}
	if lock {
		if _, err := tx.Exec(ctx, `SELECT FROM inventory.cycle_count_material WHERE plant_id = $1 AND status = 'active' AND material_id = ANY($2)
				ORDER BY id FOR NO KEY UPDATE`, a.Plant.ID, ids); err != nil {
			return nil, fmt.Errorf("inventory: lock materials' cycle count indicators at plant %s: %w", a.Plant.Code, err)
		}
	}
	rows, _ := tx.Query(ctx, cycleMaterialSelect+` WHERE m.plant_id = $1 AND m.status = 'active' AND m.material_id = ANY($2) ORDER BY m.id`,
		a.Plant.ID, ids)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CycleMaterial, error) { return scanCycleMaterial(r) })
	if err != nil {
		return nil, fmt.Errorf("inventory: materials' cycle count indicators at plant %s: %w", a.Plant.Code, err)
	}
	out := map[uuid.UUID]CycleMaterial{}
	for _, c := range cs {
		out[c.MaterialID] = c
	}
	return out, nil
}

// classes sums the lines by class, A, B and C in that order.
func (a *ABCAnalysis) classes() {
	a.Classes = make([]ABCClass, len(abcClasses))
	for i, code := range abcClasses {
		a.Classes[i] = ABCClass{Class: code}
		if ind, ok := a.indicators[code]; ok {
			a.Classes[i].Indicator = &ind
		}
	}
	for _, l := range a.Lines {
		c := &a.Classes[slices.Index(abcClasses, l.Class)]
		// The lines' values add up to at most the total, which money.Sum
		// checked, so no sum of a class overflows.
		c.Materials, c.Consumption = c.Materials+1, c.Consumption+l.Consumption
	}
}

// seal sets the analysis's digest (Token): a SHA-256 of everything it
// shows that applying it depends on, in its order.
func (a *ABCAnalysis) seal() {
	h := sha256.New()
	// A hash's Write never fails, so neither does a print to it.
	_, _ = fmt.Fprintf(h, "%s %s %s %s %d %d %d %d %d\n", a.Plant.ID, a.From.Format(time.DateOnly), a.To.Format(time.DateOnly), a.Currency,
		a.Shares.A, a.Shares.B, a.Total, a.Excluded, a.ExcludedMaterials)
	for _, l := range a.Lines {
		_, _ = fmt.Fprintf(h, "%s %d %s %s %s", l.MaterialID, l.Consumption, l.Class, l.Indicator.ID, l.Change)
		if l.Before != nil {
			_, _ = fmt.Fprintf(h, " %s %s %d", l.Before.ID, l.Before.IndicatorID, l.Before.Version)
		}
		_, _ = fmt.Fprintln(h)
	}
	a.Token = hex.EncodeToString(h.Sum(nil))
}
