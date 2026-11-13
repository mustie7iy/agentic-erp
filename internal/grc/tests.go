package grc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// NewTest is the result of testing a control as a person records it.
type NewTest struct {
	TestedOn time.Time
	Result   string
	Note     string
}

// RecordTest records that an active control was tested (ADR-0078 §3,
// T98a): the date, the result and a note of the evidence, by the caller.
// The control's row is locked, so a test is never recorded on a control
// retired meanwhile; its version does not move. The test is never
// changed: a correction is another test. The date is the database's
// today or earlier, as identity dates a grant.
func RecordTest(ctx context.Context, controlID uuid.UUID, in NewTest) (Test, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Test{}, err
	}
	if err := oneOf("result", in.Result, results); err != nil {
		return Test{}, err
	}
	if err := problem.RequireText("note", in.Note); err != nil {
		return Test{}, err
	}
	c, err := control(ctx, tx, controlID, true)
	if err != nil {
		return Test{}, err
	}
	if c.Status != Active {
		return Test{}, errRetired(c.Code)
	}
	today, err := database.Today(ctx, tx)
	if err != nil {
		return Test{}, fmt.Errorf("grc: date of a test of control %s: %w", controlID, err)
	}
	on := time.Date(in.TestedOn.Year(), in.TestedOn.Month(), in.TestedOn.Day(), 0, 0, 0, 0, time.UTC)
	if on.After(today) {
		return Test{}, problem.Invalid("grc.test_in_future", "tested_on", "A test is dated "+today.Format("2006-01-02")+" or earlier.")
	}
	t := Test{ID: uuid.Must(uuid.NewV7()), ControlID: c.ID, ControlCode: c.Code, TestedOn: on, Result: in.Result, Note: in.Note, TestedBy: p.ID}
	if err := tx.QueryRow(ctx, `INSERT INTO grc.control_test (tenant_id, id, control_id, tested_on, result, note, tested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at`, p.TenantID, t.ID, c.ID, on, t.Result, t.Note, p.ID).Scan(&t.CreatedAt); err != nil {
		return Test{}, fmt.Errorf("grc: record a test of control %s: %w", controlID, err)
	}
	return t, changed(ctx, c, "test", "tested", map[string]any{"code": c.Code, "test_id": t.ID, "tested_on": on.Format("2006-01-02"),
		"result": t.Result}, &t)
}
