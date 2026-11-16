package records

import (
	"context"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// GetPersonalDataDeletionPreview implements GET
// /v1/personal-data-deletion-preview.
func (e *Endpoints) GetPersonalDataDeletionPreview(ctx context.Context, params gen.GetPersonalDataDeletionPreviewParams) (gen.GetPersonalDataDeletionPreviewRes, error) {
	items, count, version, next, err := e.Preview(ctx, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	return &gen.PersonalDataDeletionPreview{ConfigurationVersion: version, Count: int32(count), Items: subjectsToAPI(items), // #nosec G115 -- a tenant's partners
		NextPageToken: apitypes.OptText(next)}, nil
}

// ListPersonalDataDeletionSubjects implements GET
// /v1/personal-data-deletion-runs/{id}/subjects.
func (e *Endpoints) ListPersonalDataDeletionSubjects(ctx context.Context, params gen.ListPersonalDataDeletionSubjectsParams) (gen.ListPersonalDataDeletionSubjectsRes, error) {
	items, next, err := e.Subjects(ctx, params.ID, params.Status.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	return &gen.PersonalDataDeletionSubjectPage{Items: subjectsToAPI(items), NextPageToken: apitypes.OptText(next)}, nil
}

// CreatePersonalDataDeletionRun implements POST
// /v1/personal-data-deletion-runs: 201 for a run queued now, 200 for the
// run queued before.
func (e *Endpoints) CreatePersonalDataDeletionRun(ctx context.Context, req *gen.ReasonInput, _ gen.CreatePersonalDataDeletionRunParams) (gen.CreatePersonalDataDeletionRunRes, error) {
	r, created, err := e.RequestDeletion(ctx, req.Reason)
	if err != nil {
		return nil, err
	}
	out := deletionRunToAPI(r)
	if !created {
		return &out, nil
	}
	return &gen.PersonalDataDeletionRunHeaders{Location: gen.NewOptString("/v1/personal-data-deletion-runs/" + r.ID.String()), Response: out}, nil
}

// GetPersonalDataDeletionRun implements GET
// /v1/personal-data-deletion-runs/{id}.
func (e *Endpoints) GetPersonalDataDeletionRun(ctx context.Context, params gen.GetPersonalDataDeletionRunParams) (gen.GetPersonalDataDeletionRunRes, error) {
	r, err := e.DeletionRun(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := deletionRunToAPI(r)
	return &out, nil
}

// subjectsToAPI gives the subjects in the API's form.
func subjectsToAPI(ss []Subject) []gen.PersonalDataDeletionSubject {
	out := make([]gen.PersonalDataDeletionSubject, len(ss))
	for i, s := range ss {
		out[i] = gen.PersonalDataDeletionSubject{PartnerID: s.PartnerID, PartnerNumber: s.Number, PersonalDataBlockedAt: s.BlockedAt,
			RetentionYears: int32(s.Years), RetentionEnds: s.Ends, SettledAt: apitypes.OptDateTime(s.SettledAt)}
		if s.Status != "" {
			out[i].Status.SetTo(s.Status)
		}
	}
	return out
}

// deletionRunToAPI gives the run in the API's form.
func deletionRunToAPI(r DeletionRun) gen.PersonalDataDeletionRun {
	return gen.PersonalDataDeletionRun{ID: r.ID, Status: r.Status, ConfigurationVersion: r.ConfigVersion, SubjectCount: r.Count, PendingCount: r.Pending,
		BlankedCount: r.Blanked, SkippedCount: r.Skipped,
		FailureCode: apitypes.OptString(r.FailureCode), CompletedAt: apitypes.OptDateTime(r.CompletedAt), RequestedBy: r.RequestedBy,
		RequestedByName: r.RequestedByName, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
