package materials

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/numbering"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Material change kinds and statuses (ADR-0082 §9). A block is applied
// when it is requested; the others wait for their approval.
const (
	KindCreate  = "create"
	KindUpdate  = "update"
	KindBlock   = "block"
	KindUnblock = "unblock"

	ChangePending   = "pending"
	ChangeApplied   = "applied"
	ChangeRejected  = "rejected"
	ChangeCancelled = "cancelled"
)

// MaxPossibleDuplicates bounds the materials a create change lists: a
// common description may match many, and the approver needs the first
// few to judge.
const MaxPossibleDuplicates = 20

func init() {
	problem.RegisterConstraint("material_change_one_pending", func() *problem.Problem {
		return problem.Conflict("materials.change_pending", "The material has a pending change; it is decided first.")
	})
}

// Opener opens approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// RequestedPlant is a plant a change extends a material to, with its
// company and storage locations.
type RequestedPlant struct {
	PlantID          uuid.UUID   `json:"plant_id"`
	LegalEntityID    uuid.UUID   `json:"legal_entity_id"`
	StorageLocations []uuid.UUID `json:"storage_location_ids"`
}

// RequestedUnit is an alternative unit a change gives a material.
type RequestedUnit struct {
	Code        string `json:"unit"`
	Numerator   int32  `json:"numerator"`
	Denominator int32  `json:"denominator"`
}

// Scope is the companies whose plants a change replaces: those its
// requester could act in when it was requested.
type Scope struct {
	All       bool        `json:"all"`
	Companies []uuid.UUID `json:"legal_entity_ids"`
}

// Requested is what a change asks for, its payload: the fields given, nil
// when it leaves them. A create's names the whole material.
type Requested struct {
	Description *string           `json:"description,omitempty"`
	BaseUnit    *string           `json:"base_unit,omitempty"`
	Type        *string           `json:"material_type,omitempty"`
	Group       *string           `json:"material_group,omitempty"`
	Status      *string           `json:"status,omitempty"`
	Plants      *[]RequestedPlant `json:"plants,omitempty"`
	Units       *[]RequestedUnit  `json:"alternative_units,omitempty"`
	Scope       *Scope            `json:"plant_scope,omitempty"`
}

// Duplicate is an existing material a new one may duplicate: by id,
// number and description.
type Duplicate struct {
	MaterialID  uuid.UUID `json:"material_id"`
	Number      string    `json:"number"`
	Description string    `json:"description"`
}

// MaterialChange is a row of materials.material_change.
type MaterialChange struct {
	ID                 uuid.UUID
	Number             string
	Kind               string
	MaterialID         uuid.UUID
	Requested          Requested
	Reason             string
	PossibleDuplicates []Duplicate
	Status             string
	WorkflowRequestID  *uuid.UUID
	RequestedBy        uuid.UUID
	AppliedAt          *time.Time
	Version            int32
	CreatedAt          time.Time
	CreatedBy          uuid.UUID
	UpdatedAt          time.Time
	UpdatedBy          uuid.UUID
}

const changeColumns = `id, number, kind, material_id, payload, reason, possible_duplicates, status, workflow_request_id, requested_by,
	applied_at, version, created_at, created_by, updated_at, updated_by`

func scanChange(row pgx.Row) (MaterialChange, error) {
	var c MaterialChange
	err := row.Scan(&c.ID, &c.Number, &c.Kind, &c.MaterialID, &c.Requested, &c.Reason, &c.PossibleDuplicates, &c.Status, &c.WorkflowRequestID,
		&c.RequestedBy, &c.AppliedAt, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan material change")
}

// getChange reads a material change; lock takes its row lock.
func getChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (MaterialChange, error) {
	q := `SELECT ` + changeColumns + ` FROM materials.material_change WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	c, err := scanChange(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, problem.NotFoundOf("material change")
	}
	if err != nil {
		return c, fmt.Errorf("materials: read material change %s: %w", id, err)
	}
	return c, nil
}

// insertChange writes a change of the material, pending or, for a block,
// applied.
func insertChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, kind string, material uuid.UUID, req Requested, reason string,
	dups []Duplicate, applied bool) (MaterialChange, error) {
	number, err := numbering.Next(ctx, tx, p.TenantID, "materials", "material_change", "MC")
	if err != nil {
		return MaterialChange{}, fmt.Errorf("materials: number a material change: %w", err)
	}
	status := ChangePending
	if applied {
		status = ChangeApplied
	}
	if dups == nil {
		dups = []Duplicate{}
	}
	c, err := scanChange(tx.QueryRow(ctx, `INSERT INTO materials.material_change (tenant_id, id, number, kind, material_id, payload, reason,
			possible_duplicates, status, requested_by, applied_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, CASE WHEN $11 THEN now() END, $10, $10) RETURNING `+changeColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), number, kind, material, req, reason, dups, status, p.ID, applied))
	if err != nil {
		return c, fmt.Errorf("materials: insert %s change %s of material %s: %w", kind, number, material, err)
	}
	return c, nil
}

// duplicates are the materials a new one described as description may
// duplicate (ADR-0082 §9): those with its description, white space and
// case aside, whatever their status, by number, at most
// MaxPossibleDuplicates. A material whose create change was rejected or
// withdrawn never came to be and is left out. The description's advisory
// lock is taken first, so of two materials created at once with one
// description the second waits and then finds the first (ADR-0008).
func duplicates(ctx context.Context, tx pgx.Tx, description string) ([]Duplicate, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('materials.description:' || platform.require_tenant() || ':'
		|| platform.normalized_text($1), 0))`, description); err != nil {
		return nil, fmt.Errorf("materials: lock the description %q: %w", description, err)
	}
	rows, _ := tx.Query(ctx, `SELECT m.id, m.number, m.description FROM materials.material m
		WHERE platform.normalized_text(m.description) = platform.normalized_text($1)
			AND (m.status <> 'pending' OR EXISTS (SELECT FROM materials.material_change c
				WHERE c.tenant_id = m.tenant_id AND c.material_id = m.id AND c.kind = 'create' AND c.status = 'pending'))
		ORDER BY m.number, m.id LIMIT $2`, description, MaxPossibleDuplicates)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Duplicate])
	if err != nil {
		return nil, fmt.Errorf("materials: possible duplicates of %q: %w", description, err)
	}
	return out, nil
}

// confirmed refuses a new material whose description another has unless
// its creator confirms it is another material (409
// materials.duplicate_description, naming those materials); the approver
// sees them on the change.
func confirmed(dups []Duplicate, confirm bool) error {
	if len(dups) == 0 || confirm {
		return nil
	}
	named := make([]string, len(dups))
	for i, d := range dups {
		named[i] = d.Number + " " + d.Description
	}
	return problem.Conflict("materials.duplicate_description", "Materials with this description exist: "+strings.Join(named, ", ")+
		"; create it with confirm_duplicate true if it is another material, and its approver sees them.")
}

// open opens the approval request of a pending change and links it; a
// change the configuration needs no approval for is applied at once.
func (e *Endpoints) open(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c MaterialChange, number string) (MaterialChange, error) {
	id, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.MaterialChange, ObjectID: c.ID,
		Title: "Material change " + c.Number + " (" + c.Kind + ") of " + number})
	if err != nil {
		return MaterialChange{}, fmt.Errorf("materials: open the approval of material change %s: %w", c.Number, err)
	}
	if !needed {
		return e.Apply(ctx, c.ID)
	}
	linked, err := scanChange(tx.QueryRow(ctx, `UPDATE materials.material_change SET workflow_request_id = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+changeColumns, c.ID, id, p.ID))
	if err != nil {
		return linked, fmt.Errorf("materials: link material change %s to approval request %s: %w", c.Number, id, err)
	}
	return linked, nil
}

// decided locks a change's material, then the change, in the order its
// request took them (ADR-0008: a request locks the material before it
// writes the change), and requires the change pending (409
// materials.invalid_status).
func decided(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string) (MaterialChange, Material, error) {
	c, err := getChange(ctx, tx, id, false)
	if err != nil {
		return MaterialChange{}, Material{}, err
	}
	m, err := load(ctx, tx, c.MaterialID, true)
	if err != nil {
		return MaterialChange{}, Material{}, err
	}
	if c, err = getChange(ctx, tx, id, true); err != nil {
		return MaterialChange{}, Material{}, err
	}
	if c.Status != ChangePending {
		return c, m, problem.Conflict("materials.invalid_status", "Material change "+c.Number+" is "+c.Status+"; "+action+" requires it pending.")
	}
	return c, m, nil
}

// Apply applies an approved pending change (ADR-0082 §9): a create makes
// the material active, an update or unblock writes what it asks for on
// the material as it is now, over the plants of the companies its
// requester could act in. The approvals context calls it when the
// change's request is approved.
func (e *Endpoints) Apply(ctx context.Context, id uuid.UUID) (MaterialChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MaterialChange{}, err
	}
	c, m, err := decided(ctx, tx, id, "applying")
	if err != nil {
		return MaterialChange{}, err
	}
	name := "activated"
	if c.Kind == KindCreate {
		_, err = tx.Exec(ctx, `UPDATE materials.material SET status = 'active', version = version + 1, updated_by = $2 WHERE id = $1`, m.ID, p.ID)
	} else {
		name, err = applied(c.Kind), write(ctx, tx, p, m, c.Requested)
	}
	if err != nil {
		return MaterialChange{}, fmt.Errorf("materials: apply material change %s: %w", c.Number, err)
	}
	after, err := load(ctx, tx, m.ID, false)
	if err != nil {
		return MaterialChange{}, err
	}
	if err := e.record(ctx, after, change{action: "apply", name: name, detail: map[string]any{"number": after.Number, "material_change_id": c.ID,
		"material_change_number": c.Number, "kind": c.Kind}}, &m.Status); err != nil {
		return MaterialChange{}, err
	}
	return closeChange(ctx, tx, p, c, after, ChangeApplied, "apply")
}

// applied is what MaterialChanged calls the applying of an update or an
// unblock.
func applied(kind string) string {
	if kind == KindUnblock {
		return "unblocked"
	}
	return "changed"
}

// Reject rejects a pending change: the material stays as it is, and a
// rejected create leaves it pending, out of the duplicate checks.
func (e *Endpoints) Reject(ctx context.Context, id uuid.UUID) (MaterialChange, error) {
	return e.decide(ctx, id, ChangeRejected, "reject")
}

// Cancel withdraws a pending change at its requester's request, as a
// rejection does.
func (e *Endpoints) Cancel(ctx context.Context, id uuid.UUID) (MaterialChange, error) {
	return e.decide(ctx, id, ChangeCancelled, "cancel")
}

func (e *Endpoints) decide(ctx context.Context, id uuid.UUID, status, action string) (MaterialChange, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return MaterialChange{}, err
	}
	c, m, err := decided(ctx, tx, id, action)
	if err != nil {
		return MaterialChange{}, err
	}
	return closeChange(ctx, tx, p, c, m, status, action)
}

// closeChange moves a pending change to its decision, with its audit
// record and MaterialChangeChanged.
func closeChange(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c MaterialChange, m Material, status, action string) (MaterialChange, error) {
	closed, err := scanChange(tx.QueryRow(ctx, `UPDATE materials.material_change SET status = $2, applied_at = CASE WHEN $2 = 'applied' THEN now() END,
			version = version + 1, updated_by = $3 WHERE id = $1 AND status = 'pending' RETURNING `+changeColumns, c.ID, status, p.ID))
	if err != nil {
		return MaterialChange{}, fmt.Errorf("materials: %s material change %s: %w", action, c.Number, err)
	}
	before := ChangePending
	return closed, recordChange(ctx, closed, m, "materials.change."+action, &before, nil)
}

// recordChange adds the audit record of a material change's step and
// publishes MaterialChangeChanged in the same transaction (ADR-0007,
// ADR-0011); before is its status before, nil when it was requested.
// detail is the audit's, nil for the change's own.
func recordChange(ctx context.Context, c MaterialChange, m Material, action string, before *string, detail map[string]any) error {
	payload := map[string]any{"material_change_id": c.ID, "number": c.Number, "kind": c.Kind, "material_id": m.ID, "material_number": m.Number,
		"status": c.Status, "status_before": before, "workflow_request_id": c.WorkflowRequestID, "version": c.Version}
	if detail == nil {
		detail = map[string]any{"number": c.Number, "kind": c.Kind, "material_id": m.ID, "material_number": m.Number, "status": c.Status}
	}
	if err := audit.Record(ctx, action, "materials.material_change", c.ID, detail,
		outbox.Event{Context: "materials", Name: "material_change_changed", AggregateType: "materials.material_change", AggregateID: c.ID,
			Payload: payload}); err != nil {
		return fmt.Errorf("materials: record %s of material change %s: %w", action, c.Number, err)
	}
	return nil
}

// Approvals moves material changes on when their approval requests
// complete (approvals.Handler, approvals.Locator).
type Approvals struct{ Endpoints *Endpoints }

// Approved applies the change.
func (a Approvals) Approved(ctx context.Context, change, _ uuid.UUID) error {
	_, err := a.Endpoints.Apply(ctx, change)
	return err
}

// Rejected rejects the change.
func (a Approvals) Rejected(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.Reject(ctx, change)
	return err
}

// Cancelled withdraws the change: its requester cancelled its request.
func (a Approvals) Cancelled(ctx context.Context, change, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.Cancel(ctx, change)
	return err
}

// Resources is where the API reads each material change.
func (Approvals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths("/v1/material-changes/%s", ids), nil
}
