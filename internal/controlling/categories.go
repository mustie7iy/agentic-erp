package controlling

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Category is a plan category (ADR-0050 §8).
type Category struct {
	ID         uuid.UUID
	Code, Name string
	Locked     bool
	Version    int32
	CreatedAt  time.Time
	CreatedBy  uuid.UUID
	UpdatedAt  time.Time
	UpdatedBy  uuid.UUID
}

const categoryColumns = `id, code, name, locked, version, created_at, created_by, updated_at, updated_by`

func scanCategory(r pgx.Row) (Category, error) {
	var c Category
	err := r.Scan(&c.ID, &c.Code, &c.Name, &c.Locked, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "controlling: scan plan category")
}

// getCategory reads a category, FOR UPDATE when it is to change, or 404.
func getCategory(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Category, error) {
	lock := ``
	if forUpdate {
		lock = ` FOR UPDATE`
	}
	c, err := scanCategory(tx.QueryRow(ctx, `SELECT `+categoryColumns+` FROM controlling.plan_category WHERE id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, problem.NotFoundOf("plan category")
	}
	if err != nil {
		return Category{}, fmt.Errorf("controlling: read plan category %s: %w", id, err)
	}
	return c, nil
}

// recordCategory audits a category's change and publishes it.
func recordCategory(ctx context.Context, c Category, action, change string) error {
	return audit.Record(ctx, "controlling.plan_category."+action, "controlling.plan_category", c.ID,
		map[string]any{"code": c.Code, "name": c.Name, "locked": c.Locked},
		outbox.Event{Context: "controlling", Name: "plan_category_changed", AggregateType: "controlling.plan_category", AggregateID: c.ID,
			Payload: map[string]any{"plan_category_id": c.ID, "code": c.Code, "name": c.Name, "locked": c.Locked, "change": change}})
}

func categoryToAPI(c Category) gen.PlanCategory {
	return gen.PlanCategory{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Code: c.Code, Name: c.Name, Locked: c.Locked}
}

// CreatePlanCategory implements POST /v1/plan-categories.
func (e *Endpoints) CreatePlanCategory(ctx context.Context, req *gen.PlanCategoryCreate, _ gen.CreatePlanCategoryParams) (gen.CreatePlanCategoryRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if err := problem.RequireText("name", req.Name); err != nil {
		return nil, err
	}
	c, err := scanCategory(tx.QueryRow(ctx, `INSERT INTO controlling.plan_category (tenant_id, id, code, name, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $5) RETURNING `+categoryColumns, p.TenantID, uuid.Must(uuid.NewV7()), req.Code, req.Name, p.ID))
	if err != nil {
		return nil, fmt.Errorf("controlling: insert plan category %s: %w", req.Code, err)
	}
	if err := recordCategory(ctx, c, "create", "created"); err != nil {
		return nil, err
	}
	return &gen.PlanCategoryHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/plan-categories/" + c.ID.String()),
		Response: categoryToAPI(c)}, nil
}

// GetPlanCategory implements GET /v1/plan-categories/{id}.
func (e *Endpoints) GetPlanCategory(ctx context.Context, params gen.GetPlanCategoryParams) (gen.GetPlanCategoryRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getCategory(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetPlanCategoryOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: categoryToAPI(c)}, nil
}

var categorySort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "code", Type: "text"}}}

// ListPlanCategories implements GET /v1/plan-categories.
func (e *Endpoints) ListPlanCategories(ctx context.Context, params gen.ListPlanCategoriesParams) (gen.ListPlanCategoriesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(categorySort, "listPlanCategories|"+params.Code.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+categoryColumns+` FROM controlling.plan_category WHERE ($1 = '' OR code = $1)`, []any{params.Code.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Category, error) { return scanCategory(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list plan categories: %w", err)
	}
	items, next := apitypes.PageOf(ks, cs, func(c Category) (string, uuid.UUID) { return c.Code, c.ID }, categoryToAPI)
	return &gen.PlanCategoryPage{Items: items, NextPageToken: next}, nil
}

// LockPlanCategory implements POST /v1/plan-categories/{id}:lock.
func (e *Endpoints) LockPlanCategory(ctx context.Context, params gen.LockPlanCategoryParams) (gen.LockPlanCategoryRes, error) {
	c, err := setLocked(ctx, params.ID, etag.IfMatch(params.IfMatch), true)
	if err != nil {
		return nil, err
	}
	return &gen.LockPlanCategoryOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: categoryToAPI(c)}, nil
}

// UnlockPlanCategory implements POST /v1/plan-categories/{id}:unlock.
func (e *Endpoints) UnlockPlanCategory(ctx context.Context, params gen.UnlockPlanCategoryParams) (gen.UnlockPlanCategoryRes, error) {
	c, err := setLocked(ctx, params.ID, etag.IfMatch(params.IfMatch), false)
	if err != nil {
		return nil, err
	}
	return &gen.LockPlanCategoryOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: categoryToAPI(c)}, nil
}

// setLocked locks or unlocks a category read FOR UPDATE at the version the
// caller read: a batch under way holds the category, so the lock waits for
// it, and a batch after the lock finds it locked.
func setLocked(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, locked bool) (Category, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Category{}, err
	}
	c, err := getCategory(ctx, tx, id, true)
	if err != nil {
		return Category{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Category{}, err
	}
	if c.Locked == locked {
		if locked {
			return Category{}, categoryLocked()
		}
		return Category{}, problem.Conflict("controlling.plan_category_open", "The plan category is open.")
	}
	c, err = scanCategory(tx.QueryRow(ctx, `UPDATE controlling.plan_category SET locked = $2, version = version + 1, updated_by = $3
		WHERE id = $1 AND version = $4 RETURNING `+categoryColumns, id, locked, p.ID, c.Version))
	if err != nil {
		return Category{}, fmt.Errorf("controlling: lock plan category %s: %w", id, err)
	}
	action, change := "lock", "locked"
	if !locked {
		action, change = "unlock", "unlocked"
	}
	return c, recordCategory(ctx, c, action, change)
}
