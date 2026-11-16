package audit

import (
	"bytes"
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// CreateAuditEvidenceExport implements POST /v1/audit-evidence-exports:
// 201 for the export queued.
func (e *Endpoints) CreateAuditEvidenceExport(ctx context.Context, req *gen.AuditEvidenceExportCreate, _ gen.CreateAuditEvidenceExportParams) (gen.CreateAuditEvidenceExportRes, error) {
	x, err := e.RequestExport(ctx, req.LegalEntityIds, req.From, req.To)
	if err != nil {
		return nil, err
	}
	return &gen.AuditEvidenceExportHeaders{Location: gen.NewOptString("/v1/audit-evidence-exports/" + x.ID.String()), Response: exportToAPI(x)}, nil
}

// GetAuditEvidenceExport implements GET /v1/audit-evidence-exports/{id}.
func (e *Endpoints) GetAuditEvidenceExport(ctx context.Context, params gen.GetAuditEvidenceExportParams) (gen.GetAuditEvidenceExportRes, error) {
	x, err := e.Export(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := exportToAPI(x)
	return &out, nil
}

// ListAuditEvidenceExports implements GET /v1/audit-evidence-exports.
func (e *Endpoints) ListAuditEvidenceExports(ctx context.Context, params gen.ListAuditEvidenceExportsParams) (gen.ListAuditEvidenceExportsRes, error) {
	xs, next, err := e.Exports(ctx, ExportsQuery{LegalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID), Status: params.Status.Value,
		Page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)})
	if err != nil {
		return nil, err
	}
	page := gen.AuditEvidenceExportPage{Items: make([]gen.AuditEvidenceExport, len(xs)), NextPageToken: apitypes.OptText(next)}
	for i, x := range xs {
		page.Items[i] = exportToAPI(x)
	}
	return &page, nil
}

// GetAuditEvidenceExportFile implements GET
// /v1/audit-evidence-exports/{id}/files/{name}.
func (e *Endpoints) GetAuditEvidenceExportFile(ctx context.Context, params gen.GetAuditEvidenceExportFileParams) (gen.GetAuditEvidenceExportFileRes, error) {
	content, err := e.File(ctx, params.ID, params.Name)
	if err != nil {
		return nil, err
	}
	return &gen.GetAuditEvidenceExportFileOK{Data: bytes.NewReader(content)}, nil
}

// exportToAPI gives the export in the API's form.
func exportToAPI(x Export) gen.AuditEvidenceExport {
	out := gen.AuditEvidenceExport{ID: x.ID, Companies: make([]gen.AuditEvidenceCompany, len(x.Companies)), From: x.From, To: x.To,
		WithClientAddresses: x.WithClientAddresses, Status: x.Status, Files: []gen.AuditEvidenceFile{}, AuditEventCount: apitypes.OptInt32(x.AuditEvents),
		JournalEntryCount: apitypes.OptInt32(x.JournalEntries), ApprovalDecisionCount: apitypes.OptInt32(x.Decisions), FailureCode: apitypes.OptString(x.FailureCode),
		CompletedAt: apitypes.OptDateTime(x.CompletedAt), RequestedBy: x.RequestedBy, RequestedByName: x.RequestedByName, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt}
	for i, c := range x.Companies {
		out.Companies[i] = gen.AuditEvidenceCompany{LegalEntityID: c.ID, CompanyCode: c.Code}
	}
	for _, f := range x.Files {
		file := gen.AuditEvidenceFile{Name: f.Name, SHA256: f.SHA256, SizeBytes: f.SizeBytes, Records: f.Records}
		if f.Part > 0 {
			file.Part.SetTo(f.Part)
		}
		out.Files = append(out.Files, file)
	}
	if x.ManifestKey != nil { // a completed export: the database check sets every figure with the key
		out.ManifestSHA256.SetTo(hexOf(x.ManifestSHA256))
		chain := gen.AuditEvidenceChain{Verified: *x.ChainVerified, LastSeq: *x.ChainLastSeq, Checked: *x.ChainChecked, AnchorsChecked: *x.AnchorsChecked}
		if x.ChainBrokenSeq != nil {
			chain.FirstBrokenSeq.SetTo(*x.ChainBrokenSeq)
		}
		out.Chain.SetTo(chain)
	}
	return out
}
