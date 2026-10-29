package integration

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/operations"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/sap"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Migration run statuses.
const (
	Queued     = "queued"
	Running    = "running"
	Loaded     = "loaded"
	Reconciled = "reconciled"
	Failed     = "failed"
)

// Scope is what a migration run covers.
type Scope struct {
	LegalEntityIDs []uuid.UUID `json:"legal_entity_ids"`
	FiscalYear     int32       `json:"fiscal_year"`
}

// Run is a row of integration.migration_run.
type Run struct {
	ID          uuid.UUID
	ConnectorID uuid.UUID
	Scope       Scope
	Mode        string
	IsRehearsal bool
	Status      string
	Report      *Report // nil until the run finished a step
	OperationID *uuid.UUID
	StartedAt   *time.Time
	FinishedAt  *time.Time
	Version     int32
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
}

const runColumns = `id, connector_id, scope, mode, is_rehearsal, status, report, operation_id, started_at, finished_at, version, created_at,
	created_by, updated_at`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.ConnectorID, &r.Scope, &r.Mode, &r.IsRehearsal, &r.Status, &r.Report, &r.OperationID, &r.StartedAt, &r.FinishedAt,
		&r.Version, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt)
	return r, errs.Wrapf(err, "scan migration run")
}

// getRun reads a run whose legal entities the caller may all act in.
func getRun(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Run, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	q := `SELECT ` + runColumns + ` FROM integration.migration_run WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanRun(tx.QueryRow(ctx, q, id))
	if noRows(err) {
		return Run{}, problem.NotFoundOf("migration run")
	}
	if err != nil {
		return Run{}, fmt.Errorf("integration: read migration run %s: %w", id, err)
	}
	for _, le := range r.Scope.LegalEntityIDs {
		if !access.Allows(le) {
			return Run{}, problem.NotFoundOf("migration run")
		}
	}
	return r, nil
}

// NewRun is a migration run as a client defines it.
type NewRun struct {
	ConnectorID uuid.UUID
	Scope       Scope
	Mode        string
	IsRehearsal bool
}

// CreateRun defines a queued migration run from an active source connector
// over active legal entities the caller may act in.
func (e *Endpoints) CreateRun(ctx context.Context, in NewRun) (Run, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	if in.Mode != "documents" && in.Mode != "balances" {
		return Run{}, invalid("integration.unknown_mode", "mode", "documents or balances")
	}
	c, err := getConnector(ctx, tx, in.ConnectorID, false)
	if err != nil {
		return Run{}, problem.Reference(err, "connector_id", "connector", "no connector with this id")
	}
	if c.Kind != SAPFile && c.Kind != SAPOData {
		return Run{}, invalid("integration.not_a_source", "connector_id", "Migration runs read from an sap_file or sap_odata connector.")
	}
	if _, err := e.entities(ctx, in.Scope); err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, `INSERT INTO integration.migration_run (tenant_id, id, connector_id, scope, mode, is_rehearsal, status, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7) RETURNING `+runColumns, p.TenantID, uuid.Must(uuid.NewV7()), c.ID, in.Scope, in.Mode,
		in.IsRehearsal, p.ID))
	if err != nil {
		return Run{}, fmt.Errorf("integration: create migration run for connector %s: %w", c.ID, err)
	}
	return r, record(ctx, "integration.migration_run.create", r.ID, map[string]any{"connector_id": c.ID, "mode": r.Mode, "is_rehearsal": r.IsRehearsal,
		"scope": r.Scope})
}

// entities resolves the scope's legal entities by company code; each must
// be active and accessible to the caller.
func (e *Endpoints) entities(ctx context.Context, s Scope) (map[string]tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]tenancy.Entity{}
	for i, id := range s.LegalEntityIDs {
		field := fmt.Sprintf("scope.legal_entity_ids[%d]", i)
		le, err := tenancy.Accessible(ctx, e.LegalEntities, access, id, field)
		if err != nil {
			return nil, err
		}
		if le.Status != "active" {
			return nil, invalid("integration.legal_entity_not_active", field, "The legal entity is "+le.Status+".")
		}
		out[le.Code] = le
	}
	return out, nil
}

var systemCode = regexp.MustCompile(`^[A-Z0-9_]{2,20}$`)

// system names the source system in external references: the connector's
// settings.system, else SAP.
func system(c Connector) string {
	if s, ok := c.Settings["system"].(string); ok && systemCode.MatchString(s) {
		return s
	}
	return "SAP"
}

// source is the SAP reader a connector describes. Its settings were
// checked when they were saved and are checked again here: stored settings
// may predate a rule, and a run must answer 422, not panic, on them.
func (e *Endpoints) source(ctx context.Context, c Connector) (sap.Source, error) {
	if err := checkSettings(c.Kind, c.Settings); err != nil {
		return nil, err
	}
	if c.Kind == SAPFile {
		if e.ExtractDir == "" {
			return nil, problem.Unavailable("Extract files are not configured on this server (ERP_EXTRACT_DIR).", 0)
		}
		path, _ := c.Settings["path"].(string) // checkSettings held it to a local path
		return sap.FileSource{Path: filepath.Join(e.ExtractDir, path)}, nil
	}
	baseURL, _ := c.Settings["base_url"].(string) // checkSettings held it to an http(s) URL
	o := &sap.ODataSource{BaseURL: baseURL, HTTP: e.HTTP}
	if v, ok := c.Settings["client"].(string); ok {
		o.Client = v
	}
	//nolint:forbidigo // a JSON number in map[string]any is a float64; page_size is a count checkODataSettings held to a whole 1-5000, not an amount
	if v, ok := c.Settings["page_size"].(float64); ok {
		o.PageSize = int(v)
	}
	if c.SecretRef != nil {
		credentials, err := e.Secrets.Resolve(ctx, *c.SecretRef)
		user, password, found := strings.Cut(credentials, ":")
		if err != nil || !found {
			return nil, problem.Unprocessable("integration.secret_unavailable", "The connector's credentials ("+*c.SecretRef+") cannot be resolved as user:password.").WithCause(err)
		}
		o.User, o.Password = user, password
	}
	return o, nil
}

// prepare reads what the platform holds for the run's scope and extracts
// the scope from the connector. An extraction that fails is 503: the run
// stays queued (or loaded) for another attempt.
func (e *Endpoints) prepare(ctx context.Context, tx pgx.Tx, r Run) (target, *sap.Extract, Connector, error) {
	c, err := getConnector(ctx, tx, r.ConnectorID, false)
	if err != nil {
		return target{}, nil, c, err
	}
	if c.Status != "active" {
		return target{}, nil, c, problem.Conflict("integration.connector_disabled", "The run's connector is disabled.")
	}
	t := target{charts: map[string]map[string]ledger.ChartAccount{}, terms: map[string]bool{}, ranges: map[string]bool{},
		taxes: map[string]map[string]config.TaxCode{}, year: r.Scope.FiscalYear, balances: r.Mode == "balances"}
	if t.entities, err = e.entities(ctx, r.Scope); err != nil {
		return t, nil, c, err
	}
	scope := sap.Scope{FiscalYear: strconv.Itoa(int(r.Scope.FiscalYear))}
	for code, le := range t.entities {
		scope.CompanyCodes = append(scope.CompanyCodes, code)
		if t.charts[code], err = e.Accounts.Chart(ctx, le.ID); err != nil {
			return t, nil, c, err
		}
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return t, nil, c, err
	}
	for _, term := range doc.PaymentTerms {
		t.terms[term.Key] = true
	}
	for _, n := range doc.NumberRanges {
		t.ranges[n.Key()] = true
	}
	for _, co := range doc.Companies {
		t.taxes[co.Code] = map[string]config.TaxCode{}
		for _, code := range co.TaxCodes {
			t.taxes[co.Code][code.Code] = code
		}
	}
	if t.exponents, err = currency.Load(ctx, tx); err != nil {
		return t, nil, c, fmt.Errorf("integration: read the currency exponents for run %s: %w", r.ID, err)
	}
	src, err := e.source(ctx, c)
	if err != nil {
		return t, nil, c, err
	}
	ext, err := src.Extract(ctx, scope)
	if err != nil {
		return t, nil, c, problem.Unavailable("The scope cannot be extracted from "+src.Name()+"; try again.", 30).WithCause(err)
	}
	return t, ext, c, nil
}

// finish records a run's new status and report, with MigrationRunCompleted
// when the status is final, and completes its operation: failed when the
// run failed.
func finish(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, op operations.Operation, status, action string, report Report) (operations.Operation, error) {
	final := status == Reconciled || status == Failed
	updated, err := scanRun(tx.QueryRow(ctx, `UPDATE integration.migration_run SET status = $2, report = $3, operation_id = $4,
		started_at = coalesce(started_at, now()), finished_at = CASE WHEN $5 THEN now() END, version = version + 1 WHERE id = $1 RETURNING `+runColumns,
		r.ID, status, report, op.ID, final))
	if err != nil {
		return op, fmt.Errorf("integration: record migration run %s as %s: %w", r.ID, status, err)
	}
	var events []outbox.Event
	if final {
		summary := map[string]any{"passed": nil, "loaded": nil}
		if report.Reconciliation != nil {
			summary["passed"] = report.Passed
		}
		if report.LoadResult != nil {
			summary["loaded"] = report.Loaded
		}
		events = append(events, outbox.Event{Context: "integration", Name: "migration_run_completed", AggregateType: "integration.migration_run",
			AggregateID: r.ID, Payload: map[string]any{"migration_run_id": r.ID, "connector_id": r.ConnectorID, "mode": r.Mode,
				"is_rehearsal": r.IsRehearsal, "scope": r.Scope, "status": status, "operation_id": op.ID,
				"started_at": updated.StartedAt.UTC().Format(time.RFC3339Nano), "finished_at": updated.FinishedAt.UTC().Format(time.RFC3339Nano),
				"summary": summary}})
	}
	if err := record(ctx, action, r.ID, map[string]any{"status": status, "operation_id": op.ID, "by": p.ID}, events...); err != nil {
		return op, err
	}
	resource := "/v1/migration-runs/" + r.ID.String()
	if status == Failed {
		return operations.Fail(ctx, op.ID, problem.Unprocessable("integration.migration_failed",
			"The migration run failed; its report at "+resource+" says why."))
	}
	return operations.Succeed(ctx, op.ID, resource, map[string]any{"migration_run_id": r.ID, "status": status})
}

// StartRun extracts the scope, checks it and loads it: the run becomes
// loaded, or failed when the quality gate or the load refused something
// (nothing stays loaded then). A rehearsal also reconciles and is rolled
// back, keeping only its report: reconciled or failed. With a workflow
// engine (Workflows, ADR-0012) the run moves to running and the work is the
// migration workflow's; without one it completes within the request.
func (e *Endpoints) StartRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (operations.Operation, error) {
	return e.runStep(ctx, id, ifMatch, step{from: Queued, refusal: "Only a queued migration run can be started",
		kind: "integration.migrate", action: "integration.migration_run.start", work: e.load})
}

// QueuedRuns lists, across tenants and in the order they were queued, the
// migrations and reconciliations of running runs waiting for a workflow
// (ADR-0012), each numbered among the run's operations of its kind: what
// the workflows dispatcher asks integration, so that it reads none of its
// tables.
func QueuedRuns(ctx context.Context, tx pgx.Tx) ([]operations.Waiting, error) {
	rows, _ := tx.Query(ctx, `SELECT o.kind, o.tenant_id, o.id, r.id, '00000000-0000-0000-0000-000000000000'::uuid, '',
			(SELECT count(*) FROM platform.operation x WHERE x.tenant_id = o.tenant_id AND x.kind = o.kind AND x.resource = o.resource AND x.id <= o.id)
			FROM platform.operation o
			JOIN integration.migration_run r ON r.tenant_id = o.tenant_id AND r.operation_id = o.id
			WHERE o.kind IN ('integration.migrate', 'integration.reconcile') AND o.status = 'queued' AND r.status = 'running' AND o.workflow_id IS NULL
		ORDER BY o.id`)
	waiting, err := pgx.CollectRows(rows, operations.ScanWaiting)
	if err != nil {
		return nil, fmt.Errorf("integration: list the migration runs waiting for a workflow: %w", err)
	}
	return waiting, nil
}

// step is one move of a migration run: the status it starts from, the
// refusal when the run is elsewhere (runStep appends "; this one is
// <status>."), the operation kind and audit action, and the work done
// within the request when there is no workflow engine.
type step struct {
	from, refusal, kind, action string
	work                        func(context.Context, pgx.Tx, reqctx.Principal, Run, operations.Operation, target, *sap.Extract, Connector) (operations.Operation, error)
}

// runStep moves the run on: with a workflow engine its work is handed to
// the workflow, without one it is done within the request.
func (e *Endpoints) runStep(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, st step) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	r, err := getRun(ctx, tx, id, true)
	if err != nil {
		return operations.Operation{}, err
	}
	if err := ifMatch(r.Version); err != nil {
		return operations.Operation{}, err
	}
	if r.Status != st.from {
		return operations.Operation{}, problem.Conflict("integration.invalid_status", st.refusal+"; this one is "+r.Status+".")
	}
	if e.Workflows {
		if _, err := activeConnector(ctx, tx, r); err != nil {
			return operations.Operation{}, err
		}
		return e.hand(ctx, tx, p, r, st.kind, st.action)
	}
	t, ext, c, err := e.prepare(ctx, tx, r)
	if err != nil {
		return operations.Operation{}, err
	}
	op, err := operations.StartFor(ctx, st.kind, map[string]any{"migration_run_id": r.ID}, "/v1/migration-runs/"+r.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	return st.work(ctx, tx, p, r, op, t, ext, c)
}

// activeConnector is the run's connector, active.
func activeConnector(ctx context.Context, tx pgx.Tx, r Run) (Connector, error) {
	c, err := getConnector(ctx, tx, r.ConnectorID, false)
	if err != nil {
		return c, err
	}
	if c.Status != "active" {
		return c, problem.Conflict("integration.connector_disabled", "The run's connector is disabled.")
	}
	return c, nil
}

// hand queues the run's work for the workflow engine: the operation is
// created, the run moves to running under it, and the dispatcher takes it
// from there.
func (e *Endpoints) hand(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, kind, action string) (operations.Operation, error) {
	op, err := operations.StartFor(ctx, kind, map[string]any{"migration_run_id": r.ID}, "/v1/migration-runs/"+r.ID.String())
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE integration.migration_run SET status = 'running', operation_id = $2, started_at = coalesce(started_at, now()), version = version + 1 WHERE id = $1`, r.ID, op.ID); err != nil {
		return operations.Operation{}, fmt.Errorf("integration: hand migration run %s to operation %s: %w", r.ID, op.ID, err)
	}
	return op, record(ctx, action, r.ID, map[string]any{"status": Running, "operation_id": op.ID, "by": p.ID, "workflow": true})
}

// Execute does a running run's work in the request's transaction, as the
// activity of the migration workflow: the start (extract, check, load) or
// the reconciliation, by the operation's kind. A problem preparing the run
// (the connector's secret, the source not answering) fails the operation
// and puts the run back where it was, to be started again; the load's and
// reconciliation's own outcomes are the run's report, as in the request.
func (e *Endpoints) Execute(ctx context.Context, id uuid.UUID) (operations.Operation, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return operations.Operation{}, err
	}
	r, err := getRun(ctx, tx, id, true)
	if err != nil {
		return operations.Operation{}, err
	}
	if r.Status != Running || r.OperationID == nil {
		return operations.Operation{}, nil // finished or handed back: nothing to do twice
	}
	op, err := operations.Get(ctx, *r.OperationID)
	if err != nil {
		return operations.Operation{}, err
	}
	if op.Status != operations.Queued && op.Status != operations.Running {
		return op, nil // done, or cancelled: nothing to do twice
	}
	reconcile := op.Kind == "integration.reconcile"
	t, ext, c, err := e.prepare(ctx, tx, r)
	if err != nil {
		var why *problem.Problem
		if !errors.As(err, &why) || why.Status >= 500 && why.Code != "platform.unavailable" {
			return operations.Operation{}, err
		}
		back := Queued
		if reconcile {
			back = Loaded
		}
		if _, err := tx.Exec(ctx, `UPDATE integration.migration_run SET status = $2, version = version + 1 WHERE id = $1`, r.ID, back); err != nil {
			return operations.Operation{}, fmt.Errorf("integration: put migration run %s back to %s: %w", r.ID, back, err)
		}
		if err := record(ctx, "integration.migration_run.fail", r.ID, map[string]any{"operation_id": op.ID, "code": why.Code, "detail": why.Detail, "status": back}); err != nil {
			return operations.Operation{}, err
		}
		return operations.Fail(ctx, op.ID, why)
	}
	if reconcile {
		return e.reconcileRun(ctx, tx, p, r, op, t, ext, c)
	}
	return e.load(ctx, tx, p, r, op, t, ext, c)
}

// load checks the extract and loads it under the operation.
func (e *Endpoints) load(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, op operations.Operation, t target, ext *sap.Extract, c Connector) (operations.Operation, error) {
	quality := checkQuality(ext, t)
	report := Report{Extract: ext.Counts(), Quality: asList(quality)}
	if hasErrors(quality) {
		return finish(ctx, tx, p, r, op, Failed, "integration.migration_run.start", report)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT load`); err != nil {
		return operations.Operation{}, fmt.Errorf("integration: open the load savepoint of migration run %s: %w", r.ID, err)
	}
	today, err := database.Today(ctx, tx)
	if err != nil {
		return operations.Operation{}, fmt.Errorf("integration: read the load date of migration run %s: %w", r.ID, err)
	}
	existing, err := loadRefs(ctx, tx, system(c))
	if err != nil {
		return operations.Operation{}, err
	}
	l := &loader{e: e, tx: tx, p: p, run: r, system: system(c), ext: ext, t: t, refs: existing, today: today,
		vendors: map[string]uuid.UUID{}, entries: map[string]loadedEntry{}}
	if err := l.load(ctx); err != nil {
		return operations.Operation{}, err
	}
	report.LoadResult = &LoadResult{Refusals: asList(l.issues), Loaded: l.counts}
	status := Loaded
	if !hasErrors(l.issues) && r.IsRehearsal {
		rec, err := e.reconcile(ctx, ext, t, l.refs)
		if err != nil {
			return operations.Operation{}, err
		}
		report.Reconciliation = &rec
		status = Failed
		if rec.Passed {
			status = Reconciled
		}
	}
	if hasErrors(l.issues) || r.IsRehearsal {
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT load`); err != nil {
			return operations.Operation{}, fmt.Errorf("integration: roll back the load of migration run %s: %w", r.ID, err)
		}
		audit.Discard(ctx) // what the load recorded did not happen
		if hasErrors(l.issues) {
			status = Failed
		}
	}
	return finish(ctx, tx, p, r, op, status, "integration.migration_run.start", report)
}

func asList(issues []Issue) []Issue {
	if issues == nil {
		return []Issue{}
	}
	return issues
}

// ReconcileRun compares a loaded run's scope, extracted again, with the
// platform and writes the report: reconciled or failed. With a workflow
// engine the run moves to running and the work is the workflow's.
func (e *Endpoints) ReconcileRun(ctx context.Context, id uuid.UUID, ifMatch func(int32) error) (operations.Operation, error) {
	return e.runStep(ctx, id, ifMatch, step{from: Loaded, refusal: "Only a loaded migration run can be reconciled",
		kind: "integration.reconcile", action: "integration.migration_run.reconcile", work: e.reconcileRun})
}

// reconcileRun compares the extract with the platform under the operation.
func (e *Endpoints) reconcileRun(ctx context.Context, tx pgx.Tx, p reqctx.Principal, r Run, op operations.Operation, t target, ext *sap.Extract, c Connector) (operations.Operation, error) {
	existing, err := loadRefs(ctx, tx, system(c))
	if err != nil {
		return operations.Operation{}, err
	}
	rec, err := e.reconcile(ctx, ext, t, existing)
	if err != nil {
		return operations.Operation{}, err
	}
	report := *cmp.Or(r.Report, &Report{}) // a loaded run holds the report its start wrote
	report.Reconciliation = &rec
	status := Failed
	if rec.Passed {
		status = Reconciled
	}
	return finish(ctx, tx, p, r, op, status, "integration.migration_run.reconcile", report)
}

func runToAPI(r Run) gen.MigrationRun {
	out := gen.MigrationRun{ID: r.ID, Version: r.Version, CreatedAt: r.CreatedAt, CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
		ConnectorID: r.ConnectorID, Scope: gen.MigrationScope{LegalEntityIds: r.Scope.LegalEntityIDs, FiscalYear: r.Scope.FiscalYear},
		Mode: r.Mode, IsRehearsal: r.IsRehearsal, Status: r.Status}
	if r.Report != nil {
		out.Report.SetTo(reportToAPI(*r.Report))
	}
	if r.OperationID != nil {
		out.OperationID.SetTo(*r.OperationID)
	}
	if r.StartedAt != nil {
		out.StartedAt.SetTo(*r.StartedAt)
	}
	if r.FinishedAt != nil {
		out.FinishedAt.SetTo(*r.FinishedAt)
	}
	return out
}

// CreateMigrationRun implements POST /v1/migration-runs.
func (e *Endpoints) CreateMigrationRun(ctx context.Context, req *gen.MigrationRunCreate, _ gen.CreateMigrationRunParams) (gen.CreateMigrationRunRes, error) {
	r, err := e.CreateRun(ctx, NewRun{ConnectorID: req.ConnectorID, Scope: Scope{LegalEntityIDs: req.Scope.LegalEntityIds, FiscalYear: req.Scope.FiscalYear},
		Mode: req.Mode, IsRehearsal: req.IsRehearsal})
	if err != nil {
		return nil, err
	}
	return &gen.MigrationRunHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Location: gen.NewOptString("/v1/migration-runs/" + r.ID.String()),
		Response: runToAPI(r)}, nil
}

// GetMigrationRun implements GET /v1/migration-runs/{id}.
func (e *Endpoints) GetMigrationRun(ctx context.Context, params gen.GetMigrationRunParams) (gen.GetMigrationRunRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	r, err := getRun(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetMigrationRunOKHeaders{ETag: gen.NewOptString(etag.Format(r.Version)), Response: runToAPI(r)}, nil
}

var runSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListMigrationRuns implements GET /v1/migration-runs, newest first; runs
// with a legal entity outside the caller's access are left out.
func (e *Endpoints) ListMigrationRuns(ctx context.Context, params gen.ListMigrationRunsParams) (gen.ListMigrationRunsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(runSort, "listMigrationRuns|"+params.Status.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+runColumns+` FROM integration.migration_run WHERE ($1 = '' OR status = $1)
		AND ($2 OR NOT EXISTS (SELECT FROM jsonb_array_elements_text(scope->'legal_entity_ids') le WHERE NOT le::uuid = ANY($3)))`,
		[]any{params.Status.Value, access.All, access.IDs})
	rows, _ := tx.Query(ctx, q, args...)
	runs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: list migration runs: %w", err)
	}
	items, next := apitypes.PageOf(ks, runs, func(r Run) (string, uuid.UUID) {
		return r.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00"), r.ID
	}, runToAPI)
	return &gen.MigrationRunPage{Items: items, NextPageToken: next}, nil
}

func operationOK(op operations.Operation) *gen.OperationHeaders {
	return &gen.OperationHeaders{Location: gen.NewOptString("/v1/operations/" + op.ID.String()), Response: operations.ToAPI(op)}
}

// StartMigrationRun implements POST /v1/migration-runs/{id}:start.
func (e *Endpoints) StartMigrationRun(ctx context.Context, params gen.StartMigrationRunParams) (gen.StartMigrationRunRes, error) {
	op, err := e.StartRun(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return operationOK(op), nil
}

// ReconcileMigrationRun implements POST /v1/migration-runs/{id}:reconcile.
func (e *Endpoints) ReconcileMigrationRun(ctx context.Context, params gen.ReconcileMigrationRunParams) (gen.ReconcileMigrationRunRes, error) {
	op, err := e.ReconcileRun(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return operationOK(op), nil
}
