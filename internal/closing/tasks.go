package closing

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// act changes a task of a locked close; it returns what it did, or ok
// false when it changed nothing.
type act func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task) (ch change, ok bool, err error)

// taskAction runs a change of a close's task (ADR-0058 §5, §6): the close
// is locked first and its version checked against If-Match, so a change
// made on a stale view of the checklist is refused; the change moves the
// close's version, is audited and published, and the answer is the task
// with the close's new ETag.
func (e *Endpoints) taskAction(ctx context.Context, closeID uuid.UUID, code, ifMatch string, do act) (*gen.CloseTaskHeaders, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	version, err := lockClose(ctx, tx, closeID)
	if err != nil {
		return nil, err
	}
	if err := etag.IfMatch(ifMatch)(version); err != nil {
		return nil, err
	}
	t, err := readTask(ctx, tx, closeID, code)
	if err != nil {
		return nil, err
	}
	ch, ok, err := do(ctx, tx, p, t)
	if err != nil {
		return nil, err
	}
	if ok {
		if err := e.changed(ctx, tx, p, closeID, ch); err != nil {
			return nil, err
		}
		if t, err = readTask(ctx, tx, closeID, code); err != nil {
			return nil, err
		}
	}
	v, err := e.view(ctx, []uuid.UUID{t.LegalEntityID}, []task{t})
	if err != nil {
		return nil, err
	}
	return &gen.CloseTaskHeaders{ETag: gen.NewOptString(etag.Format(t.CloseVersion)), Response: v.taskToAPI(t)}, nil
}

// changed moves the close's version with a change of its task, and audits
// and publishes the change.
func (e *Endpoints) changed(ctx context.Context, tx pgx.Tx, p reqctx.Principal, closeID uuid.UUID, ch change) error {
	if err := touchClose(ctx, tx, closeID, p.ID); err != nil {
		return err
	}
	c, err := readClose(ctx, tx, closeID)
	if err != nil {
		return err
	}
	return record(ctx, c, ch)
}

// UpdateCloseTask implements PATCH /v1/period-closes/{id}/tasks/{code}:
// the task's assignee, an active person of the tenant, or none.
func (e *Endpoints) UpdateCloseTask(ctx context.Context, req *gen.CloseTaskUpdate, params gen.UpdateCloseTaskParams) (gen.UpdateCloseTaskRes, error) {
	return e.taskAction(ctx, params.ID, params.Code, params.IfMatch, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task) (change, bool, error) {
		if !req.AssigneeID.Set {
			return change{}, false, nil
		}
		var assignee *uuid.UUID
		if !req.AssigneeID.Null {
			assignee = &req.AssigneeID.Value
			if err := e.person(ctx, *assignee); err != nil {
				return change{}, false, err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE closing.close_task SET assignee_id = $3, version = version + 1, updated_by = $4 WHERE close_id = $1 AND code = $2`,
			t.CloseID, t.Code, assignee, p.ID)
		if err != nil {
			return change{}, false, fmt.Errorf("closing: assign task %s of close %s: %w", t.Code, t.CloseID, err)
		}
		return change{action: "assign_task", name: "task_assigned", task: &t.Code, detail: map[string]any{"assignee_id": assignee}}, true, nil
	})
}

// person refuses an assignee that is not an active person of the tenant:
// tasks are people's work (ADR-0058 §6).
func (e *Endpoints) person(ctx context.Context, id uuid.UUID) error {
	named, err := e.People.Named(ctx, []uuid.UUID{id})
	if err != nil {
		return fmt.Errorf("closing: assignee %s: %w", id, err)
	}
	if n, ok := named[id]; !ok || n.Kind != "human" || n.Status != "active" {
		return problem.Unprocessable("closing.assignee_invalid", "A task is assigned to an active person of the tenant, not to an agent, a service, "+
			"a blocked or an unknown principal.", problem.FieldError{Field: "assignee_id", Code: "invalid", Message: "an active person of the tenant"})
	}
	return nil
}

// CompleteCloseTask implements POST
// /v1/period-closes/{id}/tasks/{code}:complete: an open task done, once
// every task it depends on is.
func (e *Endpoints) CompleteCloseTask(ctx context.Context, req gen.OptCloseTaskComplete, params gen.CompleteCloseTaskParams) (gen.CompleteCloseTaskRes, error) {
	return e.taskAction(ctx, params.ID, params.Code, params.IfMatch, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task) (change, bool, error) {
		switch {
		case t.Status == TaskDone:
			return change{}, false, problem.Conflict("closing.task_already_done", "Task "+t.Code+" is done already; reopen it to complete it again.")
		case len(t.BlockedBy) > 0:
			return change{}, false, problem.Conflict("closing.task_blocked", "Task "+t.Code+" waits for "+strings.Join(t.BlockedBy, ", ")+
				", still open; complete them first.")
		}
		note := apitypes.Ptr[string](req.Value.Note)
		// The close's lock serialises its writes, so the guard on the
		// status never meets another; it keeps the move the trigger allows.
		_, err := tx.Exec(ctx, `UPDATE closing.close_task SET status = 'done', completed_by = $3, completed_at = now(), note = $4, version = version + 1,
				updated_by = $3 WHERE close_id = $1 AND code = $2 AND status = 'open'`, t.CloseID, t.Code, p.ID, note)
		if err != nil {
			return change{}, false, fmt.Errorf("closing: complete task %s of close %s: %w", t.Code, t.CloseID, err)
		}
		return change{action: "complete_task", name: "task_completed", task: &t.Code, detail: map[string]any{"note": note}}, true, nil
	})
}

// ReopenCloseTask implements POST
// /v1/period-closes/{id}/tasks/{code}:reopen: a done task open again,
// for a reason; nothing that depends on it is reopened.
func (e *Endpoints) ReopenCloseTask(ctx context.Context, req *gen.ReasonInput, params gen.ReopenCloseTaskParams) (gen.ReopenCloseTaskRes, error) {
	return e.taskAction(ctx, params.ID, params.Code, params.IfMatch, func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, t task) (change, bool, error) {
		if t.Status != TaskDone {
			return change{}, false, problem.Conflict("closing.task_not_done", "Task "+t.Code+" is open; only a done task is reopened.")
		}
		_, err := tx.Exec(ctx, `UPDATE closing.close_task SET status = 'open', completed_by = NULL, completed_at = NULL, note = NULL, version = version + 1,
				updated_by = $3 WHERE close_id = $1 AND code = $2 AND status = 'done'`, t.CloseID, t.Code, p.ID)
		if err != nil {
			return change{}, false, errs.Wrapf(err, "closing: reopen task %s of close %s", t.Code, t.CloseID)
		}
		return change{action: "reopen_task", name: "task_reopened", task: &t.Code, reason: &req.Reason}, true, nil
	})
}
