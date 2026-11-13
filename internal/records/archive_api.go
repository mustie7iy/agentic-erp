package records

import (
	"bytes"
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// CreateArchivingRun implements POST /v1/archiving-runs: 201 for a run
// queued now, 200 for the run of the company, kind and year queued or
// completed before.
func (e *Endpoints) CreateArchivingRun(ctx context.Context, req *gen.ArchivingRunCreate, _ gen.CreateArchivingRunParams) (gen.CreateArchivingRunRes, error) {
	r, created, err := e.RequestRun(ctx, req.LegalEntityID, req.DocumentKind, int16(req.FiscalYear)) // #nosec G115 -- 1900 to 9999, the contract's bounds
	if err != nil {
		return nil, err
	}
	out := runsToAPI([]Run{r})
	if !created {
		return &out[0], nil
	}
	return &gen.ArchivingRunHeaders{Location: gen.NewOptString("/v1/archiving-runs/" + r.ID.String()), Response: out[0]}, nil
}

// GetArchivingRun implements GET /v1/archiving-runs/{id}.
func (e *Endpoints) GetArchivingRun(ctx context.Context, params gen.GetArchivingRunParams) (gen.GetArchivingRunRes, error) {
	r, err := e.Run(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := runsToAPI([]Run{r})
	return &out[0], nil
}

// ListArchivingRuns implements GET /v1/archiving-runs.
func (e *Endpoints) ListArchivingRuns(ctx context.Context, params gen.ListArchivingRunsParams) (gen.ListArchivingRunsRes, error) {
	rs, next, err := e.Runs(ctx, RunsQuery{LegalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID), Kind: params.DocumentKind.Value,
		Status: params.Status.Value, FiscalYear: apitypes.Ptr[int32](params.FiscalYear), Page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)})
	if err != nil {
		return nil, err
	}
	return &gen.ArchivingRunPage{Items: runsToAPI(rs), NextPageToken: apitypes.OptText(next)}, nil
}

// GetArchivingRunFile implements GET /v1/archiving-runs/{id}/file.
func (e *Endpoints) GetArchivingRunFile(ctx context.Context, params gen.GetArchivingRunFileParams) (gen.GetArchivingRunFileRes, error) {
	content, err := e.File(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetArchivingRunFileOK{Data: bytes.NewReader(content)}, nil
}

// runsToAPI gives the runs in the API's form, their debit totals in their
// currency's digits.
func runsToAPI(rs []Run) []gen.ArchivingRun {
	out := make([]gen.ArchivingRun, len(rs))
	for i, r := range rs {
		out[i] = gen.ArchivingRun{ID: r.ID, LegalEntityID: r.LegalEntity, CompanyCode: r.CompanyCode, DocumentKind: r.Kind, FiscalYear: int32(r.FiscalYear),
			RetentionYears: int32(r.RetentionYears), ConfigurationVersion: r.ConfigVersion, Status: r.Status, FailureCode: apitypes.OptString(r.FailureCode),
			CompletedAt: apitypes.OptDateTime(r.CompletedAt), RequestedBy: r.RequestedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if r.ObjectKey != nil { // a completed run: the database check sets every figure with the key
			out[i].File.SetTo(gen.ArchiveFile{Name: fileName(r.ID), SHA256: hexOf(r.SHA256), SizeBytes: *r.SizeBytes, ManifestSHA256: hexOf(r.ManifestSHA256),
				EntryCount: *r.EntryCount, LineCount: *r.LineCount, DebitTotal: apitypes.Money(*r.DebitTotal, *r.Currency, r.exp)})
		}
	}
	return out
}
