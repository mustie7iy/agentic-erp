package grc

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// linkToAPI is a link as the API gives it: the field of its kind, what
// the target is now and whether it holds.
func linkToAPI(l Link) gen.ControlLink {
	out := gen.ControlLink{ID: l.ID, Version: l.Version, CreatedAt: l.CreatedAt, CreatedBy: l.CreatedBy, UpdatedAt: l.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(l.UpdatedBy), ControlID: l.ControlID, ControlCode: l.ControlCode, Kind: l.Kind, Status: l.Status, Holds: l.Holds}
	if l.Reason != "" {
		out.BrokenReason.SetTo(l.Reason)
	}
	switch l.Kind {
	case KindPermission:
		out.PermissionCode.SetTo(l.Target)
		out.PermissionDescription = apitypes.OptString(l.Permission)
	case KindSodRule:
		out.SodRuleID.SetTo(l.RuleID)
		if r := l.Rule; r != nil {
			out.SodRule.SetTo(gen.SodRuleSummary{PermissionA: r.PermissionA, PermissionB: r.PermissionB, Description: r.Description,
				ValidFrom: r.ValidFrom, ValidTo: apitypes.OptDate(r.ValidTo)})
		}
	default:
		out.ConfigurationPath.SetTo(l.Target)
		out.ConfigurationVersion.SetTo(l.ConfigVersion)
	}
	return out
}

// CreateControlLink implements POST /v1/controls/{id}/links.
func (e *Endpoints) CreateControlLink(ctx context.Context, req *gen.ControlLinkCreate, params gen.CreateControlLinkParams) (gen.CreateControlLinkRes, error) {
	l, err := e.Link(ctx, params.ID, NewLink{Kind: req.Kind, PermissionCode: apitypes.Ptr[string](req.PermissionCode),
		SodRuleID: apitypes.Ptr[uuid.UUID](req.SodRuleID), ConfigurationPath: apitypes.Ptr[string](req.ConfigurationPath)})
	if err != nil {
		return nil, err
	}
	return &gen.ControlLinkHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Response: linkToAPI(l)}, nil
}

// RemoveControlLink implements POST /v1/controls/{id}/links/{link_id}:remove.
func (e *Endpoints) RemoveControlLink(ctx context.Context, params gen.RemoveControlLinkParams) (gen.RemoveControlLinkRes, error) {
	l, err := e.Unlink(ctx, params.ID, params.LinkID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return &gen.ControlLinkHeaders{ETag: gen.NewOptString(etag.Format(l.Version)), Response: linkToAPI(l)}, nil
}

// GetControlMatrix implements GET /v1/control-matrix: the active
// controls the filters select, by code, each with its links judged at
// the read, and the totals of all that is selected (ADR-0078 §8). The
// operation reads one database state (x-snapshot), so the totals and the
// page agree.
func (e *Endpoints) GetControlMatrix(ctx context.Context, params gen.GetControlMatrixParams) (gen.GetControlMatrixRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	f := MatrixFilter{Process: apitypes.Ptr[string](params.Process), Reference: apitypes.Ptr[string](params.FrameworkReference),
		LinkState: apitypes.Ptr[string](params.LinkState)}
	scope := fmt.Sprintf("getControlMatrix|%q|%q|%s", params.Process.Value, params.FrameworkReference.Value, params.LinkState.Value)
	ks, err := e.Pages.Keyset(controlSort, scope, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, fmt.Errorf("grc: page of the control matrix: %w", err)
	}
	s, err := e.selected(ctx, tx, f)
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(controlSelect+` WHERE c.id = ANY($1)`, []any{s.ids})
	rows, _ := tx.Query(ctx, q, args...)
	controls, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Control, error) { return scanControl(r) })
	if err != nil {
		return nil, fmt.Errorf("grc: controls of a page of the matrix: %w", err)
	}
	items, next, err := apitypes.PageWith(ks, controls, func(c Control) (string, uuid.UUID) { return c.Code, c.ID },
		func(page []Control) (map[uuid.UUID]Person, error) { return e.names(ctx, page, nil) },
		func(c Control, people map[uuid.UUID]Person) gen.ControlMatrixRow {
			links := make([]gen.ControlLink, len(s.links[c.ID]))
			for i, l := range s.links[c.ID] {
				links[i] = linkToAPI(l)
			}
			return gen.ControlMatrixRow{Control: controlToAPI(c, people), LinkState: s.states[c.ID], Links: links}
		})
	if err != nil {
		return nil, err
	}
	t := s.totals
	return &gen.ControlMatrix{AsOf: s.asOf, Items: items, NextPageToken: next,
		Totals: gen.ControlMatrixTotals{Controls: t.Controls, ControlsHolding: t.Holding, ControlsBroken: t.Broken, ControlsWithoutLinks: t.WithoutLinks,
			Links: t.Links, LinksHolding: t.LinksHolding, LinksBroken: t.LinksBroken}}, nil
}
