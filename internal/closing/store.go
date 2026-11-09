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
	problem.RegisterConstraint("year_close_once", func() *problem.Problem {
		return problem.Conflict("closing.year_close_exists", "The company has a close of the fiscal year already; it was opened at the same time.")
	})
}

// Statuses of a close and of its tasks (ADR-0058 §5).
const (
	CloseOpen      = "open"
	CloseSignedOff = "signed_off"
	TaskOpen       = "open"
	TaskDone       = "done"
)

// closeRow is a stored close, a period close or a year close (closeKind),
// with its tasks' counts: all, done, and the due dates of those open, from
// which the overdue ones are counted on the company's date at the read;
// its tasks' first and last due date; and, while it is signed off, when
// and by whom (its latest sign-off). A year close's FiscalPeriod is 0 and
// its PeriodEnd the year's last day.
type closeRow struct {
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
	SignedAt                 *time.Time
	SignedBy                 *uuid.UUID
}

// overdue counts the open tasks due before day.
func (c closeRow) overdue(day time.Time) int32 {
	var n int32
	for _, d := range c.OpenDue {
		if d.Before(day) {
			n++
		}
	}
	return n
}

// closeSelect reads closes of a kind (closeKind.q).
const closeSelect = `SELECT c.id, c.legal_entity_id, c.fiscal_year, {period}, {end}, c.config_version, c.status, c.version,
		c.created_at, c.created_by, c.updated_at, c.updated_by, k.total, k.done, k.open_due, k.first_due, k.last_due, so.signed_at, so.signed_by
	FROM {close} c CROSS JOIN LATERAL (SELECT count(*)::int, (count(*) FILTER (WHERE t.status = 'done'))::int,
			coalesce(array_agg(t.due_on) FILTER (WHERE t.status = 'open'), '{}'), min(t.due_on), max(t.due_on)
		FROM {task} t WHERE t.tenant_id = c.tenant_id AND t.close_id = c.id) k(total, done, open_due, first_due, last_due)
	LEFT JOIN LATERAL {signed} so ON true`

func scanClose(r pgx.Row) (closeRow, error) {
	var c closeRow
	err := r.Scan(&c.ID, &c.LegalEntityID, &c.FiscalYear, &c.FiscalPeriod, &c.PeriodEnd, &c.ConfigVersion, &c.Status, &c.Version,
		&c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.Total, &c.Done, &c.OpenDue, &c.FirstDue, &c.LastDue, &c.SignedAt, &c.SignedBy)
	return c, errs.Wrapf(err, "scan close")
}

// readClose reads a close of a kind of a legal entity the caller may act
// in; any other is not found.
func readClose(ctx context.Context, tx pgx.Tx, k *closeKind, id uuid.UUID) (closeRow, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return closeRow{}, err
	}
	c, err := scanClose(tx.QueryRow(ctx, k.q(closeSelect+` WHERE c.id = $1`), id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf(k.noun), "closing: read "+k.noun+" %s", id); err != nil {
		return closeRow{}, err
	}
	return c, nil
}

// lockClose locks a close of a legal entity the caller may act in for a
// change of it or its tasks, and returns its version: every write on a
// close takes this lock first, so writes on one close serialise
// (ADR-0058 §5).
func lockClose(ctx context.Context, tx pgx.Tx, k *closeKind, id uuid.UUID) (int32, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return 0, err
	}
	var le uuid.UUID
	var version int32
	err = tx.QueryRow(ctx, k.q(`SELECT legal_entity_id, version FROM {close} WHERE id = $1 FOR UPDATE`), id).Scan(&le, &version)
	if err = authz.Visible(err, access, le, problem.NotFoundOf(k.noun), "closing: lock "+k.noun+" %s", id); err != nil {
		return 0, err
	}
	return version, nil
}

// existing is the close of a kind a company has of a period or year
// already, ok false when it has none.
func existing(ctx context.Context, tx pgx.Tx, k *closeKind, legalEntity uuid.UUID, year, period int16) (uuid.UUID, bool, error) {
	var id uuid.UUID
	// A year close's {period} is 0, the period it is asked with.
	err := tx.QueryRow(ctx, k.q(`SELECT c.id FROM {close} c WHERE c.legal_entity_id = $1 AND c.fiscal_year = $2 AND {period} = $3`),
		legalEntity, year, period).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.UUID{}, false, nil
	}
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("closing: %s of %s, %d/%d: %w", k.noun, legalEntity, year, period, err)
	}
	return id, true, nil
}

// exists refuses a second close of a kind with p, naming the first by its
// Location.
func exists(k *closeKind, first uuid.UUID, p *problem.Problem) error {
	p.Header = http.Header{"Location": {k.path + first.String()}}
	return p
}

// task is a stored task of a close of either kind with its close's legal
// entity, period (0 for a year close) and version, and the codes of the
// tasks it depends on that are open.
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
	CheckedAt                *time.Time
	CheckedBy                *uuid.UUID
	CheckPassed              *bool
	Found                    *found
}

// taskSelect reads tasks of closes of a kind (closeKind.q).
const taskSelect = `SELECT t.close_id, c.legal_entity_id, c.fiscal_year, {period}, c.version, t.code, t.seq, t.name, t.kind, t.check_kind,
		t.due_on, t.depends_on, ARRAY(SELECT d.code FROM {task} d WHERE d.tenant_id = t.tenant_id AND d.close_id = t.close_id
			AND d.code = ANY (t.depends_on) AND d.status = 'open' ORDER BY d.seq),
		t.assignee_id, t.status, t.completed_by, t.completed_at, t.note, t.checked_at, t.checked_by, t.check_passed, t.check_result
	FROM {task} t JOIN {close} c ON c.tenant_id = t.tenant_id AND c.id = t.close_id`

func scanTask(r pgx.Row) (task, error) {
	var t task
	err := r.Scan(&t.CloseID, &t.LegalEntityID, &t.FiscalYear, &t.FiscalPeriod, &t.CloseVersion, &t.Code, &t.Seq, &t.Name, &t.Kind, &t.Check,
		&t.DueOn, &t.DependsOn, &t.BlockedBy, &t.Assignee, &t.Status, &t.CompletedBy, &t.CompletedAt, &t.Note,
		&t.CheckedAt, &t.CheckedBy, &t.CheckPassed, &t.Found)
	return t, errs.Wrapf(err, "scan close task")
}

// tasksOf reads a close's tasks in the template's order.
func tasksOf(ctx context.Context, tx pgx.Tx, k *closeKind, closeID uuid.UUID) ([]task, error) {
	rows, _ := tx.Query(ctx, k.q(taskSelect+` WHERE t.close_id = $1 ORDER BY t.seq`), closeID)
	ts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (task, error) { return scanTask(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: tasks of %s %s: %w", k.noun, closeID, err)
	}
	return ts, nil
}

// readTask reads a task of a close the caller has locked; a code the
// close has no task of is not found.
func readTask(ctx context.Context, tx pgx.Tx, k *closeKind, closeID uuid.UUID, code string) (task, error) {
	t, err := scanTask(tx.QueryRow(ctx, k.q(taskSelect+` WHERE t.close_id = $1 AND t.code = $2`), closeID, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return task{}, problem.NotFound(k.title + " " + closeID.String() + " has no task " + code + ".")
	}
	if err != nil {
		return task{}, fmt.Errorf("closing: task %s of %s %s: %w", code, k.noun, closeID, err)
	}
	return t, nil
}

// touchClose moves a close's version with a change of one of its tasks,
// so an If-Match on the checklist is refused once it is stale.
func touchClose(ctx context.Context, tx pgx.Tx, k *closeKind, id, by uuid.UUID) error {
	_, err := tx.Exec(ctx, k.q(`UPDATE {close} SET version = version + 1, updated_by = $2 WHERE id = $1`), id, by)
	return errs.Wrapf(err, "closing: touch "+k.noun+" %s", id)
}

// change is what a write did to a close, for its audit record and event:
// the action audited, the change published, the task changed (nil when
// the close was created), why a task was reopened, whether its check
// passed, and what else the audit record keeps.
type change struct {
	action, name string
	task         *string
	reason       *string
	passed       *bool
	detail       map[string]any
}

// record audits a change of a close of a kind and publishes it, as the
// close is after it: a year close's record and event carry its fiscal
// year alone (ADR-0060 §12).
func record(ctx context.Context, k *closeKind, c closeRow, ch change) error {
	detail := map[string]any{"legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear, "status": c.Status, "version": c.Version}
	payload := map[string]any{k.idKey: c.ID, "legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear, "change": ch.name, "task_code": ch.task,
		"reason": ch.reason, "check_passed": ch.passed, "status": c.Status, "version": c.Version}
	if !k.year {
		detail["fiscal_period"], payload["fiscal_period"] = c.FiscalPeriod, c.FiscalPeriod
	}
	for key, v := range ch.detail {
		detail[key] = v
	}
	if ch.task != nil {
		detail["task_code"] = *ch.task
	}
	if ch.reason != nil {
		detail["reason"] = *ch.reason
	}
	return audit.Record(ctx, k.entity+"."+ch.action, k.entity, c.ID, detail,
		outbox.Event{Context: "closing", Name: k.event, AggregateType: k.entity, AggregateID: c.ID, Payload: payload})
}
