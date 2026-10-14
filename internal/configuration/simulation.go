package configuration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/worker"
)

// SimulateKind is the operation kind of a simulation (:simulate).
const SimulateKind = "config.simulate"

// Simulation is what a replay of recent business transactions under the
// proposed configuration found: whether every transaction could be
// evaluated (Passed), one line for approvers and the differences in detail.
type Simulation struct {
	Passed  bool
	Summary string
	Detail  map[string]any
}

// Simulator replays recent transactions under the base and the proposed
// configuration in the request's transaction, reading committed data and
// writing nothing (the payables context implements it).
type Simulator interface {
	Simulate(ctx context.Context, base, proposed *config.Config) (Simulation, error)
}

// SimulationWorker runs queued config.simulate operations (ADR-0012): it
// finds them across tenants as WorkerRole, and runs each in one
// transaction of its tenant as AppRole, acting as the person who asked for
// it. The simulation's result is recorded as evidence; when the request
// then has all the evidence its tier needs, it moves on to approval.
type SimulationWorker struct {
	Dependencies Dependencies
	Pool         *database.Pool
	AppRole      string        // erp_app: runs the simulation in the tenant's scope
	WorkerRole   string        // erp_worker: finds jobs across tenants
	Timeout      time.Duration // statement timeout while simulating; default 5 minutes
	Interval     time.Duration // idle wait between rounds; default 1s
	Log          *slog.Logger
	Now          func() time.Time
	// Begin starts transactions; nil means Pool.Begin. Tests replace it to
	// inject database failures.
	Begin database.BeginFunc
}

func (w *SimulationWorker) begin(ctx context.Context, s database.Scope) (pgx.Tx, error) {
	return database.BeginOr(ctx, w.Pool, w.Begin, s)
}

// tenants returns the tenants with queued simulations.
func (w *SimulationWorker) tenants(ctx context.Context) ([]uuid.UUID, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.WorkerRole, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	rows, _ := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM platform.operation WHERE kind = $1 AND status = 'queued'`, SimulateKind)
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return tenants, fmt.Errorf("configuration: list the tenants with queued simulations: %w", err)
	}
	return tenants, nil
}

// job is a queued simulation: the operation and the change request it names.
type job struct {
	OperationID   uuid.UUID
	ChangeRequest string
	By            uuid.UUID
	ByKind        string
}

// one runs the tenant's oldest queued simulation and reports whether there
// was one.
func (w *SimulationWorker) one(ctx context.Context, tenant uuid.UUID) (bool, error) {
	return w.simulate(ctx, tenant, `o.kind = $1 AND o.status = 'queued' ORDER BY o.id LIMIT 1`, SimulateKind)
}

// Simulate runs one queued simulation, named by its operation, and reports
// whether there was one to run (ADR-0012: the activity of the simulation
// workflow; an operation no longer queued is left alone, so a retry runs
// nothing twice).
func (w *SimulationWorker) Simulate(ctx context.Context, tenant, operation uuid.UUID) (bool, error) {
	return w.simulate(ctx, tenant, `o.kind = $1 AND o.status = 'queued' AND o.id = $2`, SimulateKind, operation)
}

// simulate locks the queued operation the condition selects and runs it in
// one transaction as its requester, with the audit entries and events.
func (w *SimulationWorker) simulate(ctx context.Context, tenant uuid.UUID, where string, args ...any) (bool, error) {
	tx, err := w.begin(ctx, database.Scope{Role: w.AppRole, TenantID: tenant})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, fmt.Sprintf("%dms", timeout.Milliseconds())); err != nil {
		return false, fmt.Errorf("configuration: set the simulation timeout: %w", err)
	}
	var j job
	err = tx.QueryRow(ctx, `SELECT o.id, coalesce(o.request->>'change_request_id', ''), o.created_by, p.kind FROM platform.operation o
		JOIN identity.principal p ON p.tenant_id = o.tenant_id AND p.id = o.created_by
		WHERE `+where+` FOR UPDATE OF o SKIP LOCKED`, args...).Scan(&j.OperationID, &j.ChangeRequest, &j.By, &j.ByKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("configuration: claim a queued simulation: %w", err)
	}
	if err := database.SetActor(ctx, tx, j.By); err != nil {
		return false, err
	}
	p := reqctx.Principal{ID: j.By, Kind: j.ByKind, TenantID: tenant}
	ctx, buf := audit.WithBuffer(reqctx.WithPrincipal(reqctx.WithTx(ctx, tx), p))
	if err := w.process(ctx, tx, p, j); err != nil {
		return false, err
	}
	if err := buf.Flush(ctx, tx, p, "", clock.Now(w.Now)); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("configuration: commit simulation %s: %w", j.OperationID, err)
	}
	return true, nil
}

// process simulates the job's change request and completes its operation.
// A problem (the request is no longer awaiting evidence, the proposal
// cannot be replayed) fails the operation; other errors leave the job for
// another attempt.
func (w *SimulationWorker) process(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, j job) error {
	fail := func(p *problem.Problem) error {
		_, err := operations.Fail(ctx, j.OperationID, p)
		return err
	}
	id, err := uuid.Parse(j.ChangeRequest)
	if err != nil {
		return fail(problem.Unprocessable("config.invalid_request", "The operation does not name a change request."))
	}
	c, err := getCR(ctx, tx, id, true)
	if err != nil {
		return failOrRetry(err, fail)
	}
	if err := requireStatus(c, "simulate", AwaitingEvidence); err != nil {
		return failOrRetry(err, fail)
	}
	passed, detail, err := w.evidence(ctx, tx, actor, c, fail)
	if err != nil || detail == nil { // no detail: a problem failed the operation, the job is done
		return err
	}
	detail["operation_id"] = j.OperationID
	status, events, err := w.advance(ctx, tx, actor, c, passed)
	if err != nil {
		return err
	}
	detail["status"] = status
	if err := record(ctx, "config.change_request.evidence", id, detail, events...); err != nil {
		return err
	}
	delete(detail, "operation_id")
	_, err = operations.Succeed(ctx, j.OperationID, "/v1/change-requests/"+id.String(), detail)
	return err
}

// evidence simulates the change request and, when its tier asks for it,
// replays the regression suite, recording both, and reports whether all
// passed with the detail for the audit entry. The simulation and the
// regression suite are the evidence a tier needs (config.RequiredEvidence).
// A problem fails the operation and returns no detail: the job is done.
func (w *SimulationWorker) evidence(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, c ChangeRequest, fail func(*problem.Problem) error) (bool, map[string]any, error) {
	var baseDoc []byte
	if err := tx.QueryRow(ctx, `SELECT document FROM config.config_version WHERE number = $1`, c.BaseVersion).Scan(&baseDoc); err != nil {
		return false, nil, fmt.Errorf("configuration: read base version %d of change request %s: %w", c.BaseVersion, c.ID, err)
	}
	base, _ := config.Parse(baseDoc)        // activated documents always parse
	proposed, _ := config.Parse(c.Proposed) // validated on submission
	sim, err := w.Dependencies.Simulator.Simulate(ctx, base, proposed)
	if err != nil {
		return false, nil, failOrRetry(err, fail)
	}
	if err := recordEvidence(ctx, tx, actor, c.ID, "simulation", sim); err != nil {
		return false, nil, err
	}
	passed := sim.Passed
	detail := map[string]any{"kind": "simulation", "passed": sim.Passed, "summary": sim.Summary}
	if slices.Contains(config.RequiredEvidence(config.Tier(c.Tier)), "regression") {
		reg, err := regression(ctx, tx, w.Dependencies, c, base, proposed)
		if err != nil {
			return false, nil, failOrRetry(err, fail)
		}
		if err := recordEvidence(ctx, tx, actor, c.ID, "regression", reg); err != nil {
			return false, nil, err
		}
		passed = passed && reg.Passed
		detail["regression_passed"], detail["regression_summary"] = reg.Passed, reg.Summary
	}
	return passed, detail, nil
}

// advance moves a change request whose evidence passed on to approval and
// returns its status with the event to publish; one that failed keeps its
// status. Tiers that need evidence always have approval steps, so it never
// goes straight to approved here.
func (w *SimulationWorker) advance(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, c ChangeRequest, passed bool) (string, []outbox.Event, error) {
	if !passed {
		return c.Status, nil, nil
	}
	status, request, err := route(ctx, w.Dependencies, c, config.Tier(c.Tier))
	if err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE config.change_request SET status = $2, workflow_request_id = $3, version = version + 1, updated_by = $4 WHERE id = $1`,
		c.ID, status, request, actor.ID); err != nil {
		return "", nil, fmt.Errorf("configuration: advance change request %s: %w", c.ID, err)
	}
	return status, []outbox.Event{crEvent(c.ID, "change_request_validated", map[string]any{"change_request_id": c.ID, "number": c.Number, "tier": int(c.Tier),
		"workflow_request_id": request, "outcome": status, "status": status})}, nil
}

// recordEvidence adds a result as evidence of the given kind.
func recordEvidence(ctx context.Context, tx pgx.Tx, actor reqctx.Principal, id uuid.UUID, kind string, s Simulation) error {
	if _, err := tx.Exec(ctx, `INSERT INTO config.evidence (tenant_id, change_request_id, kind, passed, summary, detail, recorded_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, actor.TenantID, id, kind, s.Passed, s.Summary, s.Detail, actor.ID); err != nil {
		return fmt.Errorf("configuration: record %s evidence of change request %s: %w", kind, id, err)
	}
	return nil
}

// failOrRetry fails the operation with a problem the request caused, and
// returns any other error for another attempt.
func failOrRetry(err error, fail func(*problem.Problem) error) error {
	var p *problem.Problem
	if errors.As(err, &p) && p.Status < 500 {
		return fail(p)
	}
	return err
}

// route decides where a change request goes once its evidence is complete:
// approved when its tier needs no approval, else pending_approval with an
// approval request (approved at once when no step applies).
func route(ctx context.Context, deps Dependencies, c ChangeRequest, tier config.Tier) (string, *uuid.UUID, error) {
	if len(config.ApprovalSteps(tier)) == 0 {
		return Approved, nil, nil
	}
	r, needed, err := deps.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.ConfigChange, ObjectID: c.ID,
		Title: "Configuration change " + c.Number + ": " + c.Title, Tier: int(tier)})
	if err != nil {
		return "", nil, err
	}
	if !needed {
		return Approved, nil, nil
	}
	return PendingApproval, &r, nil
}

// RunOnce runs every queued simulation and returns how many it ran. A
// tenant whose job fails is left for the next round; the others go on.
func (w *SimulationWorker) RunOnce(ctx context.Context) (int, error) {
	tenants, err := w.tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("simulation worker: %w", err)
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		for {
			did, err := w.one(ctx, t)
			if err != nil {
				errs = append(errs, fmt.Errorf("simulation worker: tenant %s: %w", t, err))
				break
			}
			if !did {
				break
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// Run simulates until ctx ends; errors are logged and retried after the
// interval.
func (w *SimulationWorker) Run(ctx context.Context) {
	worker.Loop(ctx, w.Interval, time.Second, func(ctx context.Context) (bool, error) {
		_, err := w.RunOnce(ctx)
		return false, err
	}, w.Log, "simulation failed")
}
