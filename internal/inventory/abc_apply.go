package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// ABCApplication is an ABC analysis applied at a plant (S2c3), kept and
// never changed: its window, the shares then, the plant's consumption
// value, the preview's digest and the changes it made.
type ABCApplication struct {
	ID, LegalEntityID, PlantID uuid.UUID
	From, To                   time.Time
	ShareA, ShareB             int32
	Currency                   string
	Exponent                   int
	Total                      money.Minor
	Token                      string
	CreatedAt                  time.Time
	CreatedBy                  uuid.UUID
	Changes                    []ABCChangeMade
	ChangeCount                int32 // a list's count of Changes, which it leaves empty
}

// ABCChangeMade is a change an application made: the material given its
// class's indicator, or moved to it from another (Before), and the
// material's indicator row it made or changed.
type ABCChangeMade struct {
	LineNo                                   int32
	MaterialID, IndicatorID, CycleMaterialID uuid.UUID
	Consumption                              money.Minor
	Class, IndicatorCode                     string
	BeforeID                                 *uuid.UUID
	BeforeCode                               *string
}

// ApplyABC applies the analysis previewed with the token: under the
// plant's lock it is computed again with the materials' active indicators
// under their row locks, and only when its digest is the token's (409
// inventory.abc_preview_stale otherwise) is each line's change made, each
// audited as a material's indicator given or changed, naming the
// analysis; the application is kept with its changes and audited. An
// analysis that changes nothing is 422 inventory.abc_nothing_to_apply, so
// no application is kept without a change.
func (e *Endpoints) ApplyABC(ctx context.Context, in ABCInput, token string) (ABCApplication, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return ABCApplication{}, err
	}
	if err := database.LockTenant(ctx, tx, abcLock, in.PlantID.String()); err != nil {
		return ABCApplication{}, fmt.Errorf("inventory: ABC analysis of plant %s: %w", in.PlantID, err)
	}
	a, err := e.analyse(ctx, tx, in, true)
	if err != nil {
		return ABCApplication{}, err
	}
	if a.Token != token {
		return ABCApplication{}, problem.Conflict("inventory.abc_preview_stale", "The ABC analysis of plant "+a.Plant.Code+" has changed since it was "+
			"previewed; preview it again.")
	}
	if a.Changes == 0 {
		return ABCApplication{}, problem.Unprocessable("inventory.abc_nothing_to_apply", "The ABC analysis of plant "+a.Plant.Code+
			" changes no indicator: the plant's indicators are as it proposes.")
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO inventory.cycle_count_abc_application (tenant_id, id, legal_entity_id, plant_id, from_date, to_date, share_a,
			share_b, currency, total_consumption, preview_token, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, p.TenantID, id,
		a.Plant.LegalEntityID, a.Plant.ID, a.From, a.To, a.Shares.A, a.Shares.B, a.Currency, a.Total, a.Token, p.ID); err != nil {
		return ABCApplication{}, fmt.Errorf("inventory: ABC analysis applied at plant %s: %w", a.Plant.Code, err)
	}
	why := map[string]any{"application_id": id, "from": a.From.Format(time.DateOnly), "to": a.To.Format(time.DateOnly), "preview_token": a.Token}
	var no int32
	for _, l := range a.Lines {
		if l.Change == ABCNone {
			continue
		}
		no++
		if err := a.apply(ctx, tx, p, id, no, l, why); err != nil {
			return ABCApplication{}, err
		}
	}
	out, err := loadApplication(ctx, tx, id)
	if err != nil {
		return ABCApplication{}, err
	}
	return out, audit.Record(ctx, "inventory.cycle_count_abc_application.create", "inventory.cycle_count_abc_application", id, map[string]any{
		"legal_entity_id": out.LegalEntityID, "plant_id": out.PlantID, "plant_code": a.Plant.Code, "from": why["from"], "to": why["to"],
		"share_a": out.ShareA, "share_b": out.ShareB, "preview_token": out.Token, "changes": len(out.Changes)})
}

// apply makes a line's change, audited naming the analysis, and keeps it
// as the application's change no.
func (a *ABCAnalysis) apply(ctx context.Context, tx pgx.Tx, p reqctx.Principal, app uuid.UUID, no int32, l ABCLine, from map[string]any) error {
	why := map[string]any{"class": l.Class, "consumption": l.Consumption.Plain(a.Exponent), "currency": a.Currency}
	for k, v := range from {
		why[k] = v
	}
	var after CycleMaterial
	var before *uuid.UUID
	var err error
	if l.Change == ABCCreate {
		after, err = giveIndicator(ctx, tx, p, l.Indicator, a.materials.materials[l.MaterialID], a.Plant, why)
	} else {
		before = &l.Before.IndicatorID
		after, err = changeIndicator(ctx, tx, p, *l.Before, l.Indicator, why)
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO inventory.cycle_count_abc_change (tenant_id, application_id, line_no, material_id, consumption, class,
			indicator_id, before_indicator_id, cycle_count_material_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, p.TenantID, app, no, l.MaterialID,
		l.Consumption, l.Class, l.Indicator.ID, before, after.ID)
	return errs.Wrapf(err, "inventory: change %d of ABC analysis applied %s", no, app)
}

const applicationSelect = `SELECT id, legal_entity_id, plant_id, from_date, to_date, share_a, share_b, currency, total_consumption, preview_token,
		created_at, created_by
	FROM inventory.cycle_count_abc_application`

func scanApplication(row pgx.Row) (ABCApplication, error) {
	var a ABCApplication
	err := row.Scan(&a.ID, &a.LegalEntityID, &a.PlantID, &a.From, &a.To, &a.ShareA, &a.ShareB, &a.Currency, &a.Total, &a.Token, &a.CreatedAt,
		&a.CreatedBy)
	return a, errs.Wrapf(err, "scan ABC analysis applied")
}

// loadApplication reads an application of a company the caller may act
// in with its changes, in their order; another company's is not found.
func loadApplication(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ABCApplication, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return ABCApplication{}, fmt.Errorf("inventory: company access for ABC analysis applied %s: %w", id, err)
	}
	a, err := scanApplication(tx.QueryRow(ctx, applicationSelect+` WHERE id = $1`, id))
	if err = authz.Visible(err, access, a.LegalEntityID, problem.NotFoundOf("ABC analysis applied"), "inventory: ABC analysis applied %s", id); err != nil {
		return ABCApplication{}, err
	}
	rows, _ := tx.Query(ctx, `SELECT c.line_no, c.material_id, c.cycle_count_material_id, c.consumption, c.class, c.indicator_id, i.code,
			c.before_indicator_id, b.code
		FROM inventory.cycle_count_abc_change c
		JOIN inventory.cycle_count_indicator i ON i.tenant_id = c.tenant_id AND i.id = c.indicator_id
		LEFT JOIN inventory.cycle_count_indicator b ON b.tenant_id = c.tenant_id AND b.id = c.before_indicator_id
		WHERE c.application_id = $1 ORDER BY c.line_no`, id)
	var c ABCChangeMade
	_, err = pgx.ForEachRow(rows, []any{&c.LineNo, &c.MaterialID, &c.CycleMaterialID, &c.Consumption, &c.Class, &c.IndicatorID, &c.IndicatorCode,
		&c.BeforeID, &c.BeforeCode}, func() error {
		a.Changes = append(a.Changes, c)
		return nil
	})
	if err != nil {
		return ABCApplication{}, fmt.Errorf("inventory: changes of ABC analysis applied %s: %w", id, err)
	}
	as := []ABCApplication{a}
	if err := withExponents(ctx, tx, as); err != nil {
		return ABCApplication{}, err
	}
	return as[0], nil
}

// withCounts counts the applications' changes, which a list gives
// instead of the changes themselves (up to 1,000 each).
func withCounts(ctx context.Context, tx pgx.Tx, as []ABCApplication) error {
	ids := make([]uuid.UUID, len(as))
	at := map[uuid.UUID]int{}
	for i, a := range as {
		ids[i], at[a.ID] = a.ID, i
	}
	rows, _ := tx.Query(ctx, `SELECT application_id, count(*)::int FROM inventory.cycle_count_abc_change WHERE application_id = ANY($1)
		GROUP BY application_id`, ids)
	var app uuid.UUID
	var n int32
	if _, err := pgx.ForEachRow(rows, []any{&app, &n}, func() error {
		as[at[app]].ChangeCount = n
		return nil
	}); err != nil {
		return fmt.Errorf("inventory: changes of %d ABC analyses applied: %w", len(as), err)
	}
	return withExponents(ctx, tx, as)
}

// withExponents reads the applications' currencies' digits.
func withExponents(ctx context.Context, tx pgx.Tx, as []ABCApplication) error {
	var err error
	for i := range as {
		if as[i].Exponent, err = currency.Exponent(ctx, tx, as[i].Currency, nil); err != nil {
			return fmt.Errorf("inventory: ABC analysis applied %s: %w", as[i].ID, err)
		}
	}
	return nil
}
