package closing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

func init() {
	problem.RegisterConstraint("period_close_once", func() *problem.Problem {
		return problem.Conflict("closing.close_exists", "The company has a close of the period already; it was opened at the same time.")
	})
}

// Statuses of a close and of its tasks (ADR-0058 §5).
const (
	TaskOpen = "open"
	TaskDone = "done"
)

// periodClose is a stored close with its tasks' counts: all, done, and
// the due dates of those open, from which the overdue ones are counted on
// the company's date at the read; and its tasks' first and last due date.
type periodClose struct {
	ID, LegalEntityID        uuid.UUID
	FiscalYear, FiscalPeriod int16
	PeriodEnd                time.Time
	ConfigVersion            int32
	Status                   string
	Version                  int32
	CreatedAt, UpdatedAt     time.Time
	CreatedBy, UpdatedBy     uuid.UUID
	Total, Done              int32
	OpenDue                  []time.Time
	FirstDue, LastDue        *time.Time
}

// overdue counts the open tasks due before day.
func (c periodClose) overdue(day time.Time) int32 {
	var n int32
	for _, d := range c.OpenDue {
		if d.Before(day) {
			n++
		}
	}
	return n
}

const closeSelect = `SELECT c.id, c.legal_entity_id, c.fiscal_year, c.fiscal_period, c.period_end, c.config_version, c.status, c.version,
		c.created_at, c.created_by, c.updated_at, c.updated_by, k.total, k.done, k.open_due, k.first_due, k.last_due
	FROM closing.period_close c CROSS JOIN LATERAL (SELECT count(*)::int, (count(*) FILTER (WHERE t.status = 'done'))::int,
			coalesce(array_agg(t.due_on) FILTER (WHERE t.status = 'open'), '{}'), min(t.due_on), max(t.due_on)
		FROM closing.close_task t WHERE t.tenant_id = c.tenant_id AND t.close_id = c.id) k(total, done, open_due, first_due, last_due)`

func scanClose(r pgx.Row) (periodClose, error) {
	var c periodClose
	err := r.Scan(&c.ID, &c.LegalEntityID, &c.FiscalYear, &c.FiscalPeriod, &c.PeriodEnd, &c.ConfigVersion, &c.Status, &c.Version,
		&c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.Total, &c.Done, &c.OpenDue, &c.FirstDue, &c.LastDue)
	return c, errs.Wrapf(err, "scan period close")
}

// readClose reads a close of a legal entity the caller may act in; any
// other is not found.
func readClose(ctx context.Context, tx pgx.Tx, id uuid.UUID) (periodClose, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return periodClose{}, err
	}
	c, err := scanClose(tx.QueryRow(ctx, closeSelect+` WHERE c.id = $1`, id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("period close"), "closing: read period close %s", id); err != nil {
		return periodClose{}, err
	}
	return c, nil
}

// lockClose locks a close of a legal entity the caller may act in for a
// change of it or its tasks, and returns its version: every write on a
// close takes this lock first, so writes on one close serialise
// (ADR-0058 §5).
func lockClose(ctx context.Context, tx pgx.Tx, id uuid.UUID) (int32, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return 0, err
	}
	var le uuid.UUID
	var version int32
	err = tx.QueryRow(ctx, `SELECT legal_entity_id, version FROM closing.period_close WHERE id = $1 FOR UPDATE`, id).Scan(&le, &version)
	if err = authz.Visible(err, access, le, problem.NotFoundOf("period close"), "closing: lock period close %s", id); err != nil {
		return 0, err
	}
	return version, nil
}

// closeOf refuses a second close of a company's period, naming the first.
func closeOf(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, year, period int16) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM closing.period_close WHERE legal_entity_id = $1 AND fiscal_year = $2 AND fiscal_period = $3`,
		legalEntity, year, period).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("closing: close of %s, %d/%d: %w", legalEntity, year, period, err)
	}
	p := problem.Conflict("closing.close_exists", fmt.Sprintf("The company has close %s of period %d/%d already.", id, year, period))
	p.Header = http.Header{"Location": {"/v1/period-closes/" + id.String()}}
	return p
}

// task is a stored task with its close's legal entity, period and
// version, and the codes of the tasks it depends on that are open.
type task struct {
	CloseID, LegalEntityID   uuid.UUID
	FiscalYear, FiscalPeriod int16
	CloseVersion             int32
	Code                     string
	Seq                      int16
	Name, Kind               string
	Check                    *string
	DueOn                    time.Time
	DependsOn, BlockedBy     []string
	Assignee                 *uuid.UUID
	Status                   string
	CompletedBy              *uuid.UUID
	CompletedAt              *time.Time
	Note                     *string
}

const taskSelect = `SELECT t.close_id, c.legal_entity_id, c.fiscal_year, c.fiscal_period, c.version, t.code, t.seq, t.name, t.kind, t.check_kind,
		t.due_on, t.depends_on, ARRAY(SELECT d.code FROM closing.close_task d WHERE d.tenant_id = t.tenant_id AND d.close_id = t.close_id
			AND d.code = ANY (t.depends_on) AND d.status = 'open' ORDER BY d.seq),
		t.assignee_id, t.status, t.completed_by, t.completed_at, t.note
	FROM closing.close_task t JOIN closing.period_close c ON c.tenant_id = t.tenant_id AND c.id = t.close_id`

func scanTask(r pgx.Row) (task, error) {
	var t task
	err := r.Scan(&t.CloseID, &t.LegalEntityID, &t.FiscalYear, &t.FiscalPeriod, &t.CloseVersion, &t.Code, &t.Seq, &t.Name, &t.Kind, &t.Check,
		&t.DueOn, &t.DependsOn, &t.BlockedBy, &t.Assignee, &t.Status, &t.CompletedBy, &t.CompletedAt, &t.Note)
	return t, errs.Wrapf(err, "scan close task")
}

// tasksOf reads a close's tasks in the template's order.
func tasksOf(ctx context.Context, tx pgx.Tx, closeID uuid.UUID) ([]task, error) {
	rows, _ := tx.Query(ctx, taskSelect+` WHERE t.close_id = $1 ORDER BY t.seq`, closeID)
	ts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) { return scanTask(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: tasks of close %s: %w", closeID, err)
	}
	return ts, nil
}

// readTask reads a task of a close the caller has locked; a code the
// close has no task of is not found.
func readTask(ctx context.Context, tx pgx.Tx, closeID uuid.UUID, code string) (task, error) {
	t, err := scanTask(tx.QueryRow(ctx, taskSelect+` WHERE t.close_id = $1 AND t.code = $2`, closeID, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return task{}, problem.NotFound("Period close " + closeID.String() + " has no task " + code + ".")
	}
	if err != nil {
		return task{}, fmt.Errorf("closing: task %s of close %s: %w", code, closeID, err)
	}
	return t, nil
}

// touchClose moves a close's version with a change of one of its tasks,
// so an If-Match on the checklist is refused once it is stale.
func touchClose(ctx context.Context, tx pgx.Tx, id, by uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE closing.period_close SET version = version + 1, updated_by = $2 WHERE id = $1`, id, by)
	return errs.Wrapf(err, "closing: touch period close %s", id)
}

// change is what a write did to a close, for its audit record and event:
// the action audited, the change published, the task changed (nil when
// the close was created), why a task was reopened, and what else the
// audit record keeps.
type change struct {
	action, name string
	task         *string
	reason       *string
	detail       map[string]any
}

// record audits a change of a close and publishes it, as the close is
// after it.
func record(ctx context.Context, c periodClose, ch change) error {
	detail := map[string]any{"legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear, "fiscal_period": c.FiscalPeriod, "status": c.Status,
		"version": c.Version}
	for k, v := range ch.detail {
		detail[k] = v
	}
	if ch.task != nil {
		detail["task_code"] = *ch.task
	}
	if ch.reason != nil {
		detail["reason"] = *ch.reason
	}
	return audit.Record(ctx, "closing.period_close."+ch.action, "closing.period_close", c.ID, detail,
		outbox.Event{Context: "closing", Name: "period_close_changed", AggregateType: "closing.period_close", AggregateID: c.ID,
			Payload: map[string]any{"period_close_id": c.ID, "legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear,
				"fiscal_period": c.FiscalPeriod, "change": ch.name, "task_code": ch.task, "reason": ch.reason, "status": c.Status, "version": c.Version}})
}
