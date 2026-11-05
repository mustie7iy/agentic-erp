package cash

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
)

// CreateForecastVarianceComment implements POST /v1/liquidity-forecast-variance/comments.
func (e *Endpoints) CreateForecastVarianceComment(ctx context.Context, req *gen.ForecastVarianceCommentCreate, _ gen.CreateForecastVarianceCommentParams) (gen.CreateForecastVarianceCommentRes, error) {
	c, err := e.CommentVariance(ctx, CommentInput{LegalEntity: req.LegalEntityID, From: req.From, To: req.To, Lag: int(req.Lag.Or(DefaultLag)),
		Item: req.LiquidityItem.Value, Unclassified: req.Unclassified.Value, Text: req.Text})
	if err != nil {
		return nil, err
	}
	return &gen.CreateForecastVarianceCommentCreatedHeaders{Location: gen.NewOptString(commentPath(c)), Response: commentToAPI(c)}, nil
}

// commentPath is a comment's resource.
func commentPath(c VarianceComment) string {
	return "/v1/liquidity-forecast-variance/comments/" + c.ID.String()
}

// ProposeForecastVarianceComment implements POST /v1/liquidity-forecast-variance/comment-proposals.
func (e *Endpoints) ProposeForecastVarianceComment(ctx context.Context, req *gen.ForecastVarianceCommentProposal, _ gen.ProposeForecastVarianceCommentParams) (gen.ProposeForecastVarianceCommentRes, error) {
	citations := make([]Citation, len(req.Citations))
	for i, c := range req.Citations {
		citations[i] = Citation{Kind: c.Kind, ID: c.ID}
	}
	c, err := e.ProposeComment(ctx, CommentInput{LegalEntity: req.LegalEntityID, From: req.From, To: req.To, Lag: int(req.Lag.Or(DefaultLag)),
		Item: req.LiquidityItem.Value, Unclassified: req.Unclassified.Value, Text: req.Text}, req.SessionID, citations)
	if err != nil {
		return nil, err
	}
	return &gen.ForecastVarianceCommentHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(commentPath(c)),
		Response: commentToAPI(c)}, nil
}

// AcceptForecastVarianceCommentProposal implements POST /v1/liquidity-forecast-variance/comments/{id}:acceptProposal.
func (e *Endpoints) AcceptForecastVarianceCommentProposal(ctx context.Context, params gen.AcceptForecastVarianceCommentProposalParams) (gen.AcceptForecastVarianceCommentProposalRes, error) {
	c, err := e.AcceptComment(ctx, params.ID, etag.IfMatch(params.IfMatch))
	if err != nil {
		return nil, err
	}
	return decidedOut(c), nil
}

// RejectForecastVarianceCommentProposal implements POST /v1/liquidity-forecast-variance/comments/{id}:rejectProposal.
func (e *Endpoints) RejectForecastVarianceCommentProposal(ctx context.Context, req *gen.ReasonInput, params gen.RejectForecastVarianceCommentProposalParams) (gen.RejectForecastVarianceCommentProposalRes, error) {
	c, err := e.RejectComment(ctx, params.ID, etag.IfMatch(params.IfMatch), req.Reason)
	if err != nil {
		return nil, err
	}
	return decidedOut(c), nil
}

// decidedOut is a decided comment with its new version.
func decidedOut(c VarianceComment) *gen.AcceptForecastVarianceCommentProposalOKHeaders {
	return &gen.AcceptForecastVarianceCommentProposalOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: commentToAPI(c)}
}

// GetForecastVarianceComment implements GET /v1/liquidity-forecast-variance/comments/{id}.
func (e *Endpoints) GetForecastVarianceComment(ctx context.Context, params gen.GetForecastVarianceCommentParams) (gen.GetForecastVarianceCommentRes, error) {
	c, err := e.VarianceCommentOf(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return &gen.GetForecastVarianceCommentOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: commentToAPI(c)}, nil
}

// ListForecastVarianceComments implements GET /v1/liquidity-forecast-variance/comments.
func (e *Endpoints) ListForecastVarianceComments(ctx context.Context, params gen.ListForecastVarianceCommentsParams) (gen.ListForecastVarianceCommentsRes, error) {
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
	out := gen.ForecastVarianceCommentPage{Items: make([]gen.ForecastVarianceComment, len(cs)), NextPageToken: apitypes.OptText(next)}
	for i, c := range cs {
		out.Items[i] = commentToAPI(c)
	}
	return &out, nil
}

// commentToAPI shows a comment.
func commentToAPI(c VarianceComment) gen.ForecastVarianceComment {
	out := gen.ForecastVarianceComment{ID: c.ID, LegalEntityID: c.LegalEntity, CompanyCode: c.CompanyCode, From: c.From, To: c.To, Lag: int32(c.Lag),
		LiquidityItem: apitypes.OptString(c.Item), LiquidityItemName: apitypes.OptString(c.ItemName), Unclassified: c.Item == nil,
		SnapshotID: c.Snapshot, SnapshotDate: c.SnapshotDate, Text: c.Text, Status: c.Status, DecidedBy: apitypes.OptUUID(c.DecidedBy),
		CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, SessionID: apitypes.OptUUID(c.Session), DecisionReason: apitypes.OptString(c.DecisionReason),
		Citations: make([]gen.ForecastVarianceCitation, len(c.Citations))}
	if c.DecidedAt != nil {
		out.DecidedAt.SetTo(*c.DecidedAt)
	}
	for i, ci := range c.Citations {
		out.Citations[i] = gen.ForecastVarianceCitation{Kind: ci.Kind, ID: ci.ID}
	}
	return out
}
