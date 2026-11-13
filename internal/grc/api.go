package grc

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// names reads the names of the people the controls and tests name by id
// (owners and testers), in one call: what a consumer needs beside an id
// (REVIEW.md, API-consumer classes).
func (e *Endpoints) names(ctx context.Context, controls []Control, tests []Test) (map[uuid.UUID]Person, error) {
	var ids []uuid.UUID
	for _, c := range controls {
		ids = append(ids, c.OwnerID)
		if c.LastTest != nil {
			ids = append(ids, c.LastTest.TestedBy)
		}
	}
	for _, t := range tests {
		ids = append(ids, t.TestedBy)
	}
	people, err := e.People.Named(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("grc: names of %d people: %w", len(ids), err)
	}
	return people, nil
}

// testToAPI is a test as the API gives it, with its tester's name.
func testToAPI(t Test, people map[uuid.UUID]Person) gen.ControlTest {
	return gen.ControlTest{ID: t.ID, ControlID: t.ControlID, ControlCode: t.ControlCode, TestedOn: t.TestedOn, Result: t.Result, Note: t.Note,
		TestedBy: t.TestedBy, TestedByName: people[t.TestedBy].Name, CreatedAt: t.CreatedAt}
}

// controlToAPI is a control as the API gives it, with its owner's name
// and its last test.
func controlToAPI(c Control, people map[uuid.UUID]Person) gen.Control {
	out := gen.Control{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Code: c.Code, Objective: c.Objective, Process: c.Process, OwnerID: c.OwnerID,
		OwnerName: people[c.OwnerID].Name, Frequency: c.Frequency, Nature: c.Nature, Status: c.Status, FrameworkReferences: c.FrameworkReferences}
	if c.LastTest != nil {
		out.LastTest.SetTo(testToAPI(*c.LastTest, people))
	}
	return out
}

// one is a single control as the API gives it, with its ETag.
func (e *Endpoints) one(ctx context.Context, c Control, err error) (gen.Control, gen.OptString, error) {
	if err != nil {
		return gen.Control{}, gen.OptString{}, err
	}
	people, err := e.names(ctx, []Control{c}, nil)
	if err != nil {
		return gen.Control{}, gen.OptString{}, err
	}
	return controlToAPI(c, people), gen.NewOptString(etag.Format(c.Version)), nil
}

// CreateControl implements POST /v1/controls.
func (e *Endpoints) CreateControl(ctx context.Context, req *gen.ControlCreate, _ gen.CreateControlParams) (gen.CreateControlRes, error) {
	c, err := e.Record(ctx, NewControl{Code: req.Code, Objective: req.Objective, Process: req.Process, OwnerID: req.OwnerID,
		Frequency: req.Frequency, Nature: req.Nature, FrameworkReferences: req.FrameworkReferences})
	out, tag, err := e.one(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.ControlHeaders{ETag: tag, Location: gen.NewOptString("/v1/controls/" + c.ID.String()), Response: out}, nil
}

// GetControl implements GET /v1/controls/{id}.
func (e *Endpoints) GetControl(ctx context.Context, params gen.GetControlParams) (gen.GetControlRes, error) {
	c, err := GetControl(ctx, params.ID)
	out, tag, err := e.one(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.GetControlOKHeaders{ETag: tag, Response: out}, nil
}

// UpdateControl implements PATCH /v1/controls/{id}.
func (e *Endpoints) UpdateControl(ctx context.Context, req *gen.ControlUpdate, params gen.UpdateControlParams) (gen.UpdateControlRes, error) {
	ch := ControlChange{Objective: apitypes.Ptr[string](req.Objective), Process: apitypes.Ptr[string](req.Process),
		OwnerID: apitypes.Ptr[uuid.UUID](req.OwnerID), Frequency: apitypes.Ptr[string](req.Frequency), Nature: apitypes.Ptr[string](req.Nature)}
	if req.FrameworkReferences != nil { // absent from the patch: nil; given, even empty: a list
		ch.FrameworkReferences = &req.FrameworkReferences
	}
	c, err := e.Change(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	out, tag, err := e.one(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateControlOKHeaders{ETag: tag, Response: out}, nil
}

// RetireControl implements POST /v1/controls/{id}:retire.
func (e *Endpoints) RetireControl(ctx context.Context, params gen.RetireControlParams) (gen.RetireControlRes, error) {
	c, err := Retire(ctx, params.ID, etag.IfMatch(params.IfMatch))
	out, tag, err := e.one(ctx, c, err)
	if err != nil {
		return nil, err
	}
	return &gen.UpdateControlOKHeaders{ETag: tag, Response: out}, nil
}

var controlSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "c.code", Type: "text"}}}

// ListControls implements GET /v1/controls: the tenant's library by
// code, filtered by process, status, owner and the last test's result
// ("none": never tested).
func (e *Endpoints) ListControls(ctx context.Context, params gen.ListControlsParams) (gen.ListControlsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	process, status, last := apitypes.Ptr[string](params.Process), apitypes.Ptr[string](params.Status), apitypes.Ptr[string](params.LastResult)
	owner := apitypes.Ptr[uuid.UUID](params.OwnerID)
	scope := fmt.Sprintf("listControls|%q|%s|%s|%s", params.Process.Value, params.Status.Value, owner, params.LastResult.Value)
	ks, err := e.Pages.Keyset(controlSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("grc: page of controls: %w", err)
	}
	q, args := ks.SQL(controlSelect+` WHERE ($1::text IS NULL OR c.process = $1) AND ($2::text IS NULL OR c.status = $2)
		AND ($3::uuid IS NULL OR c.owner_id = $3)
		AND ($4::text IS NULL OR ($4 = 'none' AND t.test_id IS NULL) OR t.result = $4)`, []any{process, status, owner, last})
	rows, _ := tx.Query(ctx, q, args...)
	controls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Control, error) { return scanControl(r) })
	if err != nil {
		return nil, fmt.Errorf("grc: list controls: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, controls, func(c Control) (string, uuid.UUID) { return c.Code, c.ID },
		func(page []Control) (map[uuid.UUID]Person, error) { return e.names(ctx, page, nil) }, controlToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ControlPage{Items: items, NextPageToken: next}, nil
}

// CreateControlTest implements POST /v1/controls/{id}/tests.
func (e *Endpoints) CreateControlTest(ctx context.Context, req *gen.ControlTestCreate, params gen.CreateControlTestParams) (gen.CreateControlTestRes, error) {
	t, err := RecordTest(ctx, params.ID, NewTest{TestedOn: req.TestedOn, Result: req.Result, Note: req.Note})
	if err != nil {
		return nil, err
	}
	people, err := e.names(ctx, nil, []Test{t})
	if err != nil {
		return nil, err
	}
	return &gen.ControlTestHeaders{Response: testToAPI(t, people)}, nil
}

var testSort = pagination.Sort{Default: "tested_on", DefaultDesc: true, Fields: []pagination.Field{{Name: "tested_on", Column: "tested_on", Type: "date"}}}

// ListControlTests implements GET /v1/controls/{id}/tests: one control's
// tests, the latest test date first and the one recorded last first
// among those of one date (a test's id is a UUIDv7), so the first is the
// control's last test.
func (e *Endpoints) ListControlTests(ctx context.Context, params gen.ListControlTestsParams) (gen.ListControlTestsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := control(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	result := apitypes.Ptr[string](params.Result)
	ks, err := e.Pages.Keyset(testSort, fmt.Sprintf("listControlTests|%s|%s", c.ID, params.Result.Value),
		apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("grc: page of the tests of control %s: %w", c.ID, err)
	}
	q, args := ks.SQL(`SELECT id, tested_on, result, note, tested_by, created_at FROM grc.control_test
		WHERE control_id = $1 AND ($2::text IS NULL OR result = $2)`, []any{c.ID, result})
	rows, _ := tx.Query(ctx, q, args...)
	tests, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Test, error) {
		t := Test{ControlID: c.ID, ControlCode: c.Code}
		return t, errs.Wrapf(r.Scan(&t.ID, &t.TestedOn, &t.Result, &t.Note, &t.TestedBy, &t.CreatedAt), "scan control test")
	})
	if err != nil {
		return nil, fmt.Errorf("grc: list the tests of control %s: %w", c.ID, err)
	}
	items, next, err := apitypes.PageWith(ks, tests, func(t Test) (string, uuid.UUID) { return t.TestedOn.Format("2006-01-02"), t.ID },
		func(page []Test) (map[uuid.UUID]Person, error) { return e.names(ctx, nil, page) }, testToAPI)
	if err != nil {
		return nil, err
	}
	return &gen.ControlTestPage{Items: items, NextPageToken: next}, nil
}
