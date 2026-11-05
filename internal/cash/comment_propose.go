package cash

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The forecast agent's comment proposals (ADR-0051 §13, T53i): the agent
// proposes an explanation of a row of the variance in its running session,
// citing the documents behind the attributed variance it explains, each
// checked against the attribution (VarianceDocuments) as the agent may see
// it; a person accepts it, as it stands or in their own words (the
// agent's kept as its proposed text), or rejects it with a reason, once,
// so what the agent said is kept as it said it.

// Statuses of a proposal and its decisions (CommentAccepted is the third).
const (
	CommentProposed = "proposed"
	CommentRejected = "rejected"
)

// ProposeComment records the calling agent's proposed comment in its
// running session, citing documents the variance's attribution lists for
// the comment's row: checked as a person's comment is, then the session,
// then each citation; a citation the attribution does not list is refused.
func (e *Endpoints) ProposeComment(ctx context.Context, in CommentInput, session uuid.UUID, citations []Citation) (VarianceComment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	from, to := clock.Day(in.From), clock.Day(in.To)
	if err := checkComment(in, from, to); err != nil {
		return VarianceComment{}, err
	}
	if err := e.Sessions.Running(ctx, session); err != nil {
		return VarianceComment{}, err
	}
	// The month's grain admits every period a comment may name; which
	// documents a row lists does not depend on the grain, only how each
	// is attributed.
	docs, err := e.varianceEntries(ctx, DocsQuery{VarianceQuery: VarianceQuery{LegalEntity: in.LegalEntity, From: from, To: to, Lag: in.Lag,
		Grain: config.GrainMonth}, Item: in.Item, Unclassified: in.Unclassified})
	if err == nil {
		err = e.showEntries(ctx, docs.Entries)
	}
	if err != nil {
		return VarianceComment{}, err
	}
	c := VarianceComment{ID: uuid.Must(uuid.NewV7()), LegalEntity: in.LegalEntity, CompanyCode: docs.CompanyCode, From: from, To: to, Lag: in.Lag,
		Snapshot: docs.Snapshot.ID, SnapshotDate: docs.Snapshot.Date, Text: in.Text, Status: CommentProposed, CreatedBy: p.ID, Session: &session,
		Citations: citations, Version: 1}
	if !in.Unclassified {
		if c.ItemName, err = itemName(ctx, tx, in.Item); err != nil {
			return VarianceComment{}, err
		}
		c.Item = &in.Item
	}
	if err := checkCitations(docs, citations); err != nil {
		return VarianceComment{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO cash.variance_comment (tenant_id, id, legal_entity_id, period_from, period_to, lag, liquidity_item, snapshot_id,
			text, status, created_by, session_id, citations)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING created_at`,
		p.TenantID, c.ID, c.LegalEntity, c.From, c.To, c.Lag, c.Item, c.Snapshot, c.Text, c.Status, p.ID, session, citations).Scan(&c.CreatedAt)
	if err != nil {
		return VarianceComment{}, fmt.Errorf("cash: propose a comment on the variance of %s: %w", in.LegalEntity, err)
	}
	return c, proposed(ctx, c)
}

// checkCitations refuses each citation the documents do not list.
func checkCitations(docs VarianceDocs, citations []Citation) error {
	listed := cites(docs.Entries)
	var wrong []problem.FieldError
	for i, c := range citations {
		if !listed[c.Kind+":"+c.ID.String()] {
			wrong = append(wrong, problem.FieldError{Field: fmt.Sprintf("citations[%d]", i), Code: "not_attributed",
				Message: fmt.Sprintf("%s %s is not behind the attributed variance of the row", c.Kind, c.ID)})
		}
	}
	if len(wrong) > 0 {
		return problem.Unprocessable("cash.citation_not_attributed", "A proposal cites only documents behind the attributed variance of the row it "+
			"explains, as listForecastVarianceDocuments lists them.", wrong...)
	}
	return nil
}

// proposed audits a proposal and publishes VarianceCommentProposed: the
// event names the row, period, lag, snapshot and session, not the text or
// citations, which the comment's read returns.
func proposed(ctx context.Context, c VarianceComment) error {
	payload := map[string]any{"variance_comment_id": c.ID, "legal_entity_id": c.LegalEntity, "company_code": c.CompanyCode,
		"from": c.From.Format(time.DateOnly), "to": c.To.Format(time.DateOnly), "lag": c.Lag, "liquidity_item": c.Item,
		"snapshot_id": c.Snapshot, "session_id": c.Session, "citation_count": len(c.Citations)}
	detail := map[string]any{"text": c.Text, "citations": c.Citations}
	for k, v := range payload {
		detail[k] = v
	}
	return audit.Record(ctx, "cash.variance_comment.propose", "cash.variance_comment", c.ID, detail,
		outbox.Event{Context: "cash", Name: "variance_comment_proposed", AggregateType: "cash.variance_comment", AggregateID: c.ID, Payload: payload})
}

// AcceptComment accepts the agent's proposed comment as the person's
// decision: as it stands, or with the person's own text (nil: none), the
// agent's then kept as its proposed text.
func (e *Endpoints) AcceptComment(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, text *string) (VarianceComment, error) {
	if text != nil && (strings.TrimSpace(*text) == "" || utf8.RuneCountInString(*text) > maxCommentText) {
		return VarianceComment{}, textInvalid()
	}
	return e.decideComment(ctx, id, ifMatch, CommentAccepted, nil, text)
}

// RejectComment turns down the agent's proposed comment, with the
// person's reason; it is kept as evidence of what the agent said.
func (e *Endpoints) RejectComment(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, reason string) (VarianceComment, error) {
	return e.decideComment(ctx, id, ifMatch, CommentRejected, &reason, nil)
}

// decideComment decides a proposal of a legal entity the caller may act
// in (any other is not found), read FOR UPDATE and guarded by its
// version (ADR-0008); a comment not proposed is a conflict.
func (e *Endpoints) decideComment(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, status string, reason, text *string) (VarianceComment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	c, err := scanComment(tx.QueryRow(ctx, commentSelect+` WHERE c.id = $1 FOR UPDATE OF c`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(c.LegalEntity)):
		return VarianceComment{}, problem.NotFoundOf("variance comment")
	case err != nil:
		return VarianceComment{}, fmt.Errorf("cash: variance comment %s: %w", id, err)
	}
	if err := ifMatch(c.Version); err != nil {
		return VarianceComment{}, err
	}
	if c.Status != CommentProposed {
		return VarianceComment{}, problem.Conflict("cash.variance_comment_not_proposed", "The comment is "+c.Status+", not the forecast agent's proposal to decide.")
	}
	if text != nil && *text != c.Text {
		proposedText := c.Text
		c.ProposedText, c.Text = &proposedText, *text
	}
	var at time.Time
	err = tx.QueryRow(ctx, `UPDATE cash.variance_comment SET status = $2, decided_by = $3, decided_at = now(), decision_reason = $4, text = $6,
			proposed_text = $7, version = version + 1
		WHERE id = $1 AND version = $5 RETURNING decided_at`, id, status, p.ID, reason, c.Version, c.Text, c.ProposedText).Scan(&at)
	if err != nil {
		return VarianceComment{}, fmt.Errorf("cash: decide variance comment %s: %w", id, err)
	}
	c.Status, c.DecidedBy, c.DecidedAt, c.DecisionReason, c.Version = status, &p.ID, &at, reason, c.Version+1
	cs := []VarianceComment{c}
	if err := e.coded(ctx, cs); err != nil {
		return VarianceComment{}, err
	}
	action := map[string]string{CommentAccepted: "accept", CommentRejected: "reject"}[status]
	return cs[0], decided(ctx, cs[0], "cash.variance_comment."+action)
}
