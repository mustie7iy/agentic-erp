// Package grc is the governance, risk and compliance context (ADR-0078;
// 02-architecture §2.2, 03-data-model §3.30): the tenant's control
// library, the results of testing its controls, and the control matrix
// of what enforces each control. The library is the tenant's own: no
// control is shipped (ADR-0078 §2). A test is a person's statement of
// what they saw, recorded once and never changed (ADR-0078 §3). A
// control is linked to a permission, a segregation-of-duties rule or a
// configuration path, and the matrix says on read whether each link
// still holds (ADR-0078 §7, §8).
//
// The context is a leaf (ADR-0078 §1): it reads the identity context
// and the active configuration through its ports (People, Access,
// Settings), by types of its own, imports no other context, posts
// nothing, and no context imports it.
package grc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func init() {
	// A code already taken in the library, whether it is found by a
	// request after the first or by one racing it (ADR-0078 §2).
	problem.RegisterConstraint("control_code_once", func() *problem.Problem {
		return problem.Conflict("grc.control_code_taken", "The control library already has a control with this code.")
	})
	// A second active link of a control to a target (ADR-0078 §7).
	problem.RegisterConstraint("control_link_once", func() *problem.Problem {
		return problem.Conflict("grc.control_link_exists", "The control is already linked to this; a control has one link to a target.")
	})
}

// Person is a principal as the context names it: its name, kind (human,
// agent, service) and status (active, blocked, deprovisioned).
type Person struct {
	Name, Kind, Status string
}

// People reads principals (identity context), keyed by id; an id the
// tenant has no principal of is absent.
type People interface {
	Named(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Person, error)
}

// Endpoints implements the grc part of the generated API.
type Endpoints struct {
	Pages    *pagination.Codec
	People   People
	Access   Access
	Settings Settings
}

// The statuses of a control.
const (
	Active  = "active"
	Retired = "retired"
)

// What a control and a test may say (ADR-0078 §2, §3).
var (
	frequencies = []string{"continuous", "daily", "weekly", "monthly", "quarterly", "yearly", "ad_hoc"}
	natures     = []string{"manual", "automated"}
	results     = []string{"passed", "failed"}
)

// Control is a row of grc.control with its last test: the one with the
// latest test date, the one recorded last among several of one date; nil
// while the control was never tested.
type Control struct {
	ID        uuid.UUID
	Code      string
	Objective string
	Process   string
	OwnerID   uuid.UUID
	Frequency string
	Nature    string
	Status    string
	// FrameworkReferences are the tenant's references to a framework it
	// names, in its order; never nil.
	FrameworkReferences []string
	Version             int32
	CreatedAt           time.Time
	CreatedBy           uuid.UUID
	UpdatedAt           time.Time
	UpdatedBy           uuid.UUID
	LastTest            *Test
}

// Test is a row of grc.control_test with its control's code.
type Test struct {
	ID          uuid.UUID
	ControlID   uuid.UUID
	ControlCode string
	TestedOn    time.Time
	Result      string
	Note        string
	TestedBy    uuid.UUID
	CreatedAt   time.Time
}

// controlSelect reads controls under the alias c, each with its last
// test under t (control_test_latest reads it first). The test's id is
// test_id there, so a list's keyset finds one id, the control's.
const controlSelect = `SELECT c.id, c.code, c.objective, c.process, c.owner_id, c.frequency, c.nature, c.status, c.framework_references,
		c.version, c.created_at, c.created_by, c.updated_at, c.updated_by, t.test_id, t.tested_on, t.result, t.note, t.tested_by, t.recorded_at
	FROM grc.control c LEFT JOIN LATERAL (
		SELECT ct.id AS test_id, ct.tested_on, ct.result, ct.note, ct.tested_by, ct.created_at AS recorded_at
		FROM grc.control_test ct WHERE ct.tenant_id = c.tenant_id AND ct.control_id = c.id
		ORDER BY ct.tested_on DESC, ct.id DESC LIMIT 1) t ON true`

func scanControl(row pgx.Row) (Control, error) {
	var (
		c        Control
		testID   *uuid.UUID
		testedOn *time.Time
		result   *string
		note     *string
		testedBy *uuid.UUID
		recorded *time.Time
	)
	if err := row.Scan(&c.ID, &c.Code, &c.Objective, &c.Process, &c.OwnerID, &c.Frequency, &c.Nature, &c.Status, &c.FrameworkReferences,
		&c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &testID, &testedOn, &result, &note, &testedBy, &recorded); err != nil {
		return Control{}, errs.Wrapf(err, "scan control")
	}
	if testID != nil {
		c.LastTest = &Test{ID: *testID, ControlID: c.ID, ControlCode: c.Code, TestedOn: *testedOn, Result: *result, Note: *note,
			TestedBy: *testedBy, CreatedAt: *recorded}
	}
	return c, nil
}

// control reads a control of the tenant, locked for a change when
// forUpdate: recording a test locks it too, so a test and a retirement
// never pass each other.
func control(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Control, error) {
	q := controlSelect + ` WHERE c.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF c`
	}
	c, err := scanControl(tx.QueryRow(ctx, q, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Control{}, problem.NotFoundOf("control")
	case err != nil:
		return Control{}, fmt.Errorf("grc: read control %s: %w", id, err)
	}
	return c, nil
}

// GetControl returns a control of the tenant with its last test.
func GetControl(ctx context.Context, id uuid.UUID) (Control, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Control{}, err
	}
	return control(ctx, tx, id, false)
}

// errRetired refuses a change of a retired control and a test of one.
func errRetired(code string) *problem.Problem {
	return problem.Conflict("grc.control_retired", "Control "+code+" is retired: it changes no more and takes no test.")
}

// oneOf refuses a value of an open enumeration the context does not
// know, naming the field and what it accepts.
func oneOf(field, value string, accepted []string) error {
	if slices.Contains(accepted, value) {
		return nil
	}
	list := strings.Join(accepted, ", ")
	return problem.Unprocessable("request.validation_failed", "The request is not valid: "+field+" is one of "+list+".",
		problem.FieldError{Field: field, Code: "invalid", Message: "one of " + list})
}

// owner checks the person a control names as its owner: an active
// person of the tenant, never an agent or a service.
func (e *Endpoints) owner(ctx context.Context, id uuid.UUID) error {
	people, err := e.People.Named(ctx, []uuid.UUID{id})
	if err != nil {
		return fmt.Errorf("grc: owner %s of a control: %w", id, err)
	}
	p, ok := people[id]
	switch {
	case !ok:
		return problem.ReferenceNotFound("owner_id", "principal", "no principal with this id")
	case p.Kind != "human" || p.Status != "active":
		return problem.Invalid("grc.invalid_owner", "owner_id", "The owner of a control is an active person, not an agent or a service.")
	}
	return nil
}
