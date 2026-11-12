package controlling

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Allocation cycles (ADR-0064 §5, T60f): a cycle of a company has one
// sender cost center and receiver cost centers with fixed shares adding
// up to the whole. Its kind says how a run charges the receivers: a
// distribution moves the sender's primary costs to them on their original
// accounts (allocationrun.go). An assessment (T60g) is a further kind of
// the same cycle, with its assessment account.

// CycleDistribution is the kind of a distribution cycle.
const CycleDistribution = "distribution"

// wholeBP is the whole in basis points: the receivers' shares add up to it.
const wholeBP = 10000

// cycleReceiver is a receiver of a cycle with its share in basis points;
// Code is named by the ledger's port when the cycle is shown.
type cycleReceiver struct {
	CostCenterID uuid.UUID
	Code         string
	ShareBP      int32
}

// cycle is an allocation cycle with its receivers in their order.
// SenderCode is named by the ledger's port when the cycle is shown.
type cycle struct {
	ID, LegalEntityID uuid.UUID
	Code, Name, Kind  string
	SenderID          uuid.UUID
	SenderCode        string
	Receivers         []cycleReceiver
	Version           int32
	CreatedAt         time.Time
	CreatedBy         uuid.UUID
	UpdatedAt         time.Time
	UpdatedBy         uuid.UUID
}

const cycleColumns = `c.id, c.legal_entity_id, c.code, c.name, c.kind, c.sender_cost_center_id, c.version, c.created_at, c.created_by, c.updated_at, c.updated_by`

func scanCycle(r pgx.Row) (cycle, error) {
	var c cycle
	err := r.Scan(&c.ID, &c.LegalEntityID, &c.Code, &c.Name, &c.Kind, &c.SenderID, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "controlling: scan allocation cycle")
}

// percent is a share in basis points as a percentage with two decimals.
func percent(bp int32) string { return fmt.Sprintf("%d.%02d", bp/100, bp%100) }

// basisPoints reads a percentage above 0 and at most 100 with up to two
// decimals as basis points, exactly.
func basisPoints(pct string) (int32, bool) {
	r, err := money.ParseDecimalRat(pct)
	if err != nil {
		return 0, false
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || r.Sign() <= 0 || r.Cmp(big.NewRat(wholeBP, 1)) > 0 {
		return 0, false
	}
	return int32(r.Num().Int64()), true
}

// withReceivers gives the cycles their receivers, in their order, and
// names every center, read from the ledger in one call.
func (e *Endpoints) withReceivers(ctx context.Context, tx pgx.Tx, cycles ...cycle) ([]cycle, error) {
	ids, centers := make([]uuid.UUID, len(cycles)), make([]uuid.UUID, len(cycles))
	at := make(map[uuid.UUID]int, len(cycles))
	for i, c := range cycles {
		ids[i], centers[i], at[c.ID] = c.ID, c.SenderID, i
	}
	rows, _ := tx.Query(ctx, `SELECT cycle_id, cost_center_id, share_bp FROM controlling.allocation_receiver WHERE cycle_id = ANY($1)
		ORDER BY cycle_id, position`, ids)
	var id uuid.UUID
	var r cycleReceiver
	if _, err := pgx.ForEachRow(rows, []any{&id, &r.CostCenterID, &r.ShareBP}, func() error {
		cycles[at[id]].Receivers = append(cycles[at[id]].Receivers, r)
		centers = append(centers, r.CostCenterID)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("controlling: receivers of allocation cycles: %w", err)
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, centers)
	if err != nil {
		return nil, fmt.Errorf("controlling: cost centers of allocation cycles: %w", err)
	}
	for i := range cycles {
		cycles[i].SenderCode = refs[cycles[i].SenderID].Code
		for j := range cycles[i].Receivers {
			cycles[i].Receivers[j].Code = refs[cycles[i].Receivers[j].CostCenterID].Code
		}
	}
	return cycles, nil
}

// cycleInCompany reads a cycle with its receivers and its company with
// the digits of the company's currency, FOR UPDATE when it is to be run.
// A cycle that does not exist, or of a company the caller may not act in,
// is answered as missing (04 §4.9).
func (e *Endpoints) cycleInCompany(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (cycle, tenancy.Entity, int, error) {
	lock := ``
	if forUpdate {
		lock = ` FOR UPDATE`
	}
	c, err := scanCycle(tx.QueryRow(ctx, `SELECT `+cycleColumns+` FROM controlling.allocation_cycle c WHERE c.id = $1`+lock, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return cycle{}, tenancy.Entity{}, 0, problem.NotFoundOf("allocation cycle")
	}
	if err != nil {
		return cycle{}, tenancy.Entity{}, 0, fmt.Errorf("controlling: read allocation cycle %s: %w", id, err)
	}
	le, exp, ok, err := e.company(ctx, tx, c.LegalEntityID)
	if err != nil {
		return cycle{}, tenancy.Entity{}, 0, err
	}
	if !ok {
		return cycle{}, tenancy.Entity{}, 0, problem.NotFoundOf("allocation cycle")
	}
	cs, err := e.withReceivers(ctx, tx, c)
	if err != nil {
		return cycle{}, tenancy.Entity{}, 0, err
	}
	return cs[0], le, exp, nil
}

func cycleToAPI(c cycle) gen.AllocationCycle {
	out := gen.AllocationCycle{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), LegalEntityID: c.LegalEntityID, Code: c.Code, Name: c.Name, Kind: c.Kind, SenderCostCenterID: c.SenderID,
		SenderCostCenterCode: c.SenderCode, Receivers: make([]gen.AllocationReceiver, len(c.Receivers))}
	for i, r := range c.Receivers {
		out.Receivers[i] = gen.AllocationReceiver{CostCenterID: r.CostCenterID, CostCenterCode: r.Code, Percentage: percent(r.ShareBP)}
	}
	return out
}

// receiversDetail are a cycle's receivers as its audit record and its
// event carry them, in their order.
func receiversDetail(c cycle) []map[string]any {
	out := make([]map[string]any, len(c.Receivers))
	for i, r := range c.Receivers {
		out[i] = map[string]any{"cost_center_id": r.CostCenterID, "cost_center_code": r.Code, "percentage": percent(r.ShareBP)}
	}
	return out
}

// recordCycle audits a cycle's change and publishes it.
func recordCycle(ctx context.Context, c cycle, action, change string) error {
	detail := map[string]any{"legal_entity_id": c.LegalEntityID, "code": c.Code, "name": c.Name, "kind": c.Kind, "sender_cost_center_id": c.SenderID,
		"sender_cost_center_code": c.SenderCode, "receivers": receiversDetail(c)}
	return audit.Record(ctx, "controlling.allocation_cycle."+action, "controlling.allocation_cycle", c.ID, detail,
		outbox.Event{Context: "controlling", Name: "allocation_cycle_changed", AggregateType: "controlling.allocation_cycle", AggregateID: c.ID,
			Payload: map[string]any{"allocation_cycle_id": c.ID, "legal_entity_id": c.LegalEntityID, "code": c.Code, "name": c.Name, "kind": c.Kind,
				"sender_cost_center_id": c.SenderID, "sender_cost_center_code": c.SenderCode, "receivers": receiversDetail(c), "change": change}})
}

// centerState is why a cost center cannot be a cycle's sender or
// receiver: unknown, not the company's, or not active; "" when it can.
func centerState(refs map[uuid.UUID]ledger.CostCenterRef, id, le uuid.UUID) string {
	ref, known := refs[id]
	switch {
	case !known:
		return "unknown"
	case ref.LegalEntityID != le:
		return "not_in_company"
	case !ref.Active:
		return "not_active"
	}
	return ""
}

// cycleCenters checks the centers a new cycle names: the sender and each
// receiver an active cost center of the company, no receiver the sender
// or named twice. It returns the field errors and the centers' refs.
func (e *Endpoints) cycleCenters(ctx context.Context, le uuid.UUID, req *gen.AllocationCycleCreate) ([]problem.FieldError, map[uuid.UUID]ledger.CostCenterRef, error) {
	ids := []uuid.UUID{req.SenderCostCenterID}
	for _, r := range req.Receivers {
		ids = append(ids, r.CostCenterID)
	}
	refs, err := e.Ledger.CostCenterRefs(ctx, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("controlling: cost centers of a new allocation cycle: %w", err)
	}
	const want = "an active cost center of the legal entity"
	var bad []problem.FieldError
	if reason := centerState(refs, req.SenderCostCenterID, le); reason != "" {
		bad = append(bad, problem.FieldError{Field: "sender_cost_center_id", Code: reason, Message: want})
	}
	seen := map[uuid.UUID]bool{}
	for i, r := range req.Receivers {
		field, reason, message := fmt.Sprintf("receivers[%d].cost_center_id", i), centerState(refs, r.CostCenterID, le), want
		switch {
		case r.CostCenterID == req.SenderCostCenterID:
			reason, message = "is_sender", "a cost center other than the sender"
		case seen[r.CostCenterID]:
			reason, message = "duplicate", "a cost center named once"
		}
		seen[r.CostCenterID] = true
		if reason != "" {
			bad = append(bad, problem.FieldError{Field: field, Code: reason, Message: message})
		}
	}
	return bad, refs, nil
}

// cycleShares reads the receivers' percentages as basis points: each
// above 0 and at most 100 with up to two decimals, adding up to 100.
func cycleShares(req *gen.AllocationCycleCreate) ([]int32, []problem.FieldError) {
	shares := make([]int32, len(req.Receivers))
	var bad []problem.FieldError
	var total int32
	for i, r := range req.Receivers {
		bp, ok := basisPoints(r.Percentage)
		if !ok {
			bad = append(bad, problem.FieldError{Field: fmt.Sprintf("receivers[%d].percentage", i), Code: "invalid",
				Message: "a percentage above 0 and at most 100 with up to two decimals"})
		}
		shares[i] = bp
		total += bp
	}
	if len(bad) == 0 && total != wholeBP {
		bad = append(bad, problem.FieldError{Field: "receivers", Code: "not_100", Message: "percentages adding up to 100"})
	}
	return shares, bad
}

// newCycle checks a create request and returns the cycle it describes,
// its centers named; every field that cannot stand is named (422).
func (e *Endpoints) newCycle(ctx context.Context, le uuid.UUID, req *gen.AllocationCycleCreate) (cycle, error) {
	var bad []problem.FieldError
	if req.Kind != CycleDistribution {
		bad = append(bad, problem.FieldError{Field: "kind", Code: "unsupported", Message: CycleDistribution})
	}
	centers, refs, err := e.cycleCenters(ctx, le, req)
	if err != nil {
		return cycle{}, err
	}
	shares, sharesBad := cycleShares(req)
	if bad = append(append(bad, centers...), sharesBad...); len(bad) > 0 {
		return cycle{}, problem.Unprocessable("controlling.allocation_cycle_invalid", "The allocation cycle cannot be defined as given.", bad...)
	}
	c := cycle{LegalEntityID: le, Code: req.Code, Name: req.Name, Kind: req.Kind, SenderID: req.SenderCostCenterID,
		SenderCode: refs[req.SenderCostCenterID].Code, Receivers: make([]cycleReceiver, len(req.Receivers))}
	for i, r := range req.Receivers {
		c.Receivers[i] = cycleReceiver{CostCenterID: r.CostCenterID, Code: refs[r.CostCenterID].Code, ShareBP: shares[i]}
	}
	return c, nil
}

// CreateAllocationCycle implements POST /v1/allocation-cycles.
func (e *Endpoints) CreateAllocationCycle(ctx context.Context, req *gen.AllocationCycleCreate, _ gen.CreateAllocationCycleParams) (gen.CreateAllocationCycleRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if err := problem.RequireText("name", req.Name); err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, req.LegalEntityID, "legal_entity_id")
	if err != nil {
		return nil, err
	}
	c, err := e.newCycle(ctx, le.ID, req)
	if err != nil {
		return nil, err
	}
	row, err := scanCycle(tx.QueryRow(ctx, `INSERT INTO controlling.allocation_cycle AS c (tenant_id, id, legal_entity_id, code, name, kind,
			sender_cost_center_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+cycleColumns, p.TenantID, uuid.Must(uuid.NewV7()), le.ID, c.Code, c.Name, c.Kind, c.SenderID, p.ID))
	if err != nil {
		return nil, fmt.Errorf("controlling: insert allocation cycle %s of %s: %w", req.Code, le.ID, err)
	}
	row.SenderCode, row.Receivers = c.SenderCode, c.Receivers
	centers, shares := make([]uuid.UUID, len(c.Receivers)), make([]int32, len(c.Receivers))
	for i, r := range c.Receivers {
		centers[i], shares[i] = r.CostCenterID, r.ShareBP
	}
	if _, err := tx.Exec(ctx, `INSERT INTO controlling.allocation_receiver (tenant_id, cycle_id, position, cost_center_id, share_bp)
		SELECT $1, $2, r.n, r.center, r.bp FROM unnest($3::uuid[], $4::int[]) WITH ORDINALITY AS r(center, bp, n)`, p.TenantID, row.ID, centers, shares); err != nil {
		return nil, fmt.Errorf("controlling: insert the receivers of allocation cycle %s: %w", row.ID, err)
	}
	if err := recordCycle(ctx, row, "create", "created"); err != nil {
		return nil, err
	}
	return &gen.AllocationCycleHeaders{ETag: gen.NewOptString(etag.Format(row.Version)), Location: gen.NewOptString("/v1/allocation-cycles/" + row.ID.String()),
		Response: cycleToAPI(row)}, nil
}

// GetAllocationCycle implements GET /v1/allocation-cycles/{id}.
func (e *Endpoints) GetAllocationCycle(ctx context.Context, params gen.GetAllocationCycleParams) (gen.GetAllocationCycleRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, _, _, err := e.cycleInCompany(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetAllocationCycleOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: cycleToAPI(c)}, nil
}

var cycleSort = pagination.Sort{Default: "code", Fields: []pagination.Field{{Name: "code", Column: "c.code", Type: "text"}}}

// ListAllocationCycles implements GET /v1/allocation-cycles.
func (e *Endpoints) ListAllocationCycles(ctx context.Context, params gen.ListAllocationCyclesParams) (gen.ListAllocationCyclesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	le, sender := apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.SenderCostCenterID)
	scope := fmt.Sprintf("listAllocationCycles|%v|%s|%s|%v", le, params.Code.Value, params.Kind.Value, sender)
	ks, err := e.Pages.Keyset(cycleSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+cycleColumns+` FROM controlling.allocation_cycle c WHERE ($1 OR c.legal_entity_id = ANY($2))
		AND ($3::uuid IS NULL OR c.legal_entity_id = $3) AND ($4 = '' OR c.code = $4) AND ($5 = '' OR c.kind = $5)
		AND ($6::uuid IS NULL OR c.sender_cost_center_id = $6)`, []any{access.All, access.IDs, le, params.Code.Value, params.Kind.Value, sender})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cycle, error) { return scanCycle(r) })
	if err != nil {
		return nil, fmt.Errorf("controlling: list allocation cycles: %w", err)
	}
	if cs, err = e.withReceivers(ctx, tx, cs...); err != nil {
		return nil, err
	}
	items, next := apitypes.PageOf(ks, cs, func(c cycle) (string, uuid.UUID) { return c.Code, c.ID }, cycleToAPI)
	return &gen.AllocationCyclePage{Items: items, NextPageToken: next}, nil
}
