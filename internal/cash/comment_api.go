package cash

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
)

// CreateVarianceComment implements POST /v1/liquidity-forecast-variance/comments.
func (e *Endpoints) CreateVarianceComment(ctx context.Context, req *gen.VarianceCommentCreate, _ gen.CreateVarianceCommentParams) (gen.CreateVarianceCommentRes, error) {
	c, err := e.CommentVariance(ctx, CommentInput{LegalEntity: req.LegalEntityID, From: req.From, To: req.To, Lag: int(req.Lag.Or(DefaultLag)),
		Item: req.LiquidityItem.Value, Unclassified: req.Unclassified.Value, Text: req.Text})
	if err != nil {
		return nil, err
	}
	return &gen.VarianceCommentHeaders{Location: gen.NewOptString("/v1/liquidity-forecast-variance/comments/" + c.ID.String()), Response: commentToAPI(c)}, nil
}

// GetVarianceComment implements GET /v1/liquidity-forecast-variance/comments/{id}.
func (e *Endpoints) GetVarianceComment(ctx context.Context, params gen.GetVarianceCommentParams) (gen.GetVarianceCommentRes, error) {
	c, err := e.VarianceCommentOf(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	out := commentToAPI(c)
	return &out, nil
}

// ListVarianceComments implements GET /v1/liquidity-forecast-variance/comments.
func (e *Endpoints) ListVarianceComments(ctx context.Context, params gen.ListVarianceCommentsParams) (gen.ListVarianceCommentsRes, error) {
	q := CommentsQuery{LegalEntity: apitypes.Ptr[uuid.UUID](params.LegalEntityID), Item: params.LiquidityItem.Value, Unclassified: params.Unclassified.Value,
		Status: params.Status.Value, From: apitypes.Ptr[time.Time](params.From), To: apitypes.Ptr[time.Time](params.To),
		Page: apitypes.Page(params.OrderBy, params.PageSize, params.PageToken)}
	if lag, ok := params.Lag.Get(); ok {
		n := int(lag)
		q.Lag = &n
	}
	cs, next, err := e.VarianceComments(ctx, q)
	if err != nil {
		return nil, err
	}
	out := gen.VarianceCommentPage{Items: make([]gen.VarianceComment, len(cs)), NextPageToken: apitypes.OptText(next)}
	for i, c := range cs {
		out.Items[i] = commentToAPI(c)
	}
	return &out, nil
}

// commentToAPI shows a comment.
func commentToAPI(c VarianceComment) gen.VarianceComment {
	out := gen.VarianceComment{ID: c.ID, LegalEntityID: c.LegalEntity, CompanyCode: c.CompanyCode, From: c.From, To: c.To, Lag: int32(c.Lag),
		LiquidityItem: apitypes.OptString(c.Item), LiquidityItemName: apitypes.OptString(c.ItemName), Unclassified: c.Item == nil,
		SnapshotID: c.Snapshot, SnapshotDate: c.SnapshotDate, Text: c.Text, Status: c.Status, DecidedBy: apitypes.OptUUID(c.DecidedBy),
		CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy}
	if c.DecidedAt != nil {
		out.DecidedAt.SetTo(*c.DecidedAt)
	}
	return out
}
