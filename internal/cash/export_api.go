package cash

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// CreateCashExport implements POST /v1/cash-snapshots/{id}/exports.
func (e *Endpoints) CreateCashExport(ctx context.Context, req *gen.CashExportCreate, params gen.CreateCashExportParams) (gen.CreateCashExportRes, error) {
	x, err := e.RequestExport(ctx, params.ID, req.ConnectorID)
	if err != nil {
		return nil, err
	}
	return &gen.CashExportHeaders{Location: gen.NewOptString("/v1/cash-exports/" + x.ID.String()), Response: exportToAPI(x)}, nil
}

// GetCashExport implements GET /v1/cash-exports/{id}.
func (e *Endpoints) GetCashExport(ctx context.Context, params gen.GetCashExportParams) (gen.GetCashExportRes, error) {
	x, err := e.Export(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := exportToAPI(x)
	return &out, nil
}

// ListCashExports implements GET /v1/cash-exports.
func (e *Endpoints) ListCashExports(ctx context.Context, params gen.ListCashExportsParams) (gen.ListCashExportsRes, error) {
	xs, next, err := e.Exports(ctx, ExportsQuery{Snapshot: apitypes.Ptr[uuid.UUID](params.CashSnapshotID), LegalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID),
		Connector: apitypes.Ptr[uuid.UUID](params.ConnectorID),
		Status:    params.Status.Value, Page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)})
	if err != nil {
		return nil, err
	}
	out := gen.CashExportPage{Items: make([]gen.CashExport, len(xs)), NextPageToken: apitypes.OptText(next)}
	for i, x := range xs {
		out.Items[i] = exportToAPI(x)
	}
	return &out, nil
}

func exportToAPI(x Export) gen.CashExport {
	out := gen.CashExport{ID: x.ID, CashSnapshotID: x.Snapshot, LegalEntityID: x.LegalEntity, CompanyCode: x.CompanyCode, SnapshotDate: x.SnapshotDate,
		ConnectorID: x.Connector, ConnectorName: x.ConnectorName, Format: x.Format, Status: x.Status, Attempts: int32(x.Attempts), // #nosec G115 -- at most six, a database check
		NextAttemptAt: apitypes.OptDateTime(x.NextAttemptAt), FileName: apitypes.OptString(x.FileName), LastErrorCode: apitypes.OptString(x.LastError),
		DeliveredAt: apitypes.OptDateTime(x.DeliveredAt), RequestedBy: x.RequestedBy, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
	if x.SHA256 != nil {
		out.SHA256.SetTo(x.SHA256Hex())
	}
	if x.Resend != nil {
		out.Resend.SetTo(*x.Resend)
	}
	return out
}
