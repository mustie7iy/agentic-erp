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
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Variance comments (ADR-0051 §11, §13, §15; T53f4): a person's
// explanation of one row of a company's forecast variance, a liquidity
// item or what no rule assigns, over a period at a lag, against the
// snapshot that variance compares with. A person's comment is decided as
// it is written (accepted, decided by its author); the forecast agent's
// proposals (T53i) are the other statuses. A comment keeps its snapshot
// past the snapshot's retention (cash.delete_expired_snapshots skips it),
// so the variance it explains can still be read.

// CommentAccepted is the status of a person's comment, decided as it is
// written; an agent's proposal (T53i) is proposed until a person accepts
// or rejects it.
const CommentAccepted = "accepted"

// maxCommentText is the longest text of a comment, in characters (the
// contract's and variance_comment_text's limit).
const maxCommentText = 2000

func init() {
	problem.RegisterConstraint("variance_comment_text", textInvalid)
	// A snapshot deleted at its retention while the comment naming it was
	// being written: the variance can no longer be read.
	problem.RegisterConstraint("variance_comment_snapshot", func() *problem.Problem {
		return problem.Conflict("cash.snapshot_not_taken", "The snapshot the variance compares against was deleted at its retention; "+
			"the variance can no longer be read.")
	})
}

// CommentInput is a comment to write: on the legal entity's variance from
// From to To at Lag, of the liquidity item Item, or of what no rule
// assigns when Unclassified.
type CommentInput struct {
	LegalEntity  uuid.UUID
	From, To     time.Time
	Lag          int
	Item         string
	Unclassified bool
	Text         string
}

// VarianceComment is a comment as stored, with its company's code, its
// item's name and its snapshot's date. Item is nil for the row of what no
// rule assigns; DecidedBy and DecidedAt are nil while it is proposed; a
// person's comment has no session and cites nothing.
type VarianceComment struct {
	ID, LegalEntity uuid.UUID
	CompanyCode     string
	From, To        time.Time
	Lag             int
	Item, ItemName  *string
	Snapshot        uuid.UUID
	SnapshotDate    time.Time
	Text, Status    string
	DecidedBy       *uuid.UUID
	DecidedAt       *time.Time
	CreatedAt       time.Time
	CreatedBy       uuid.UUID
	Session         *uuid.UUID // the agent's session a proposal was made in
	Citations       []Citation // the documents a proposal cites
	DecisionReason  *string    // why a person rejected a proposal
	ProposedText    *string    // the agent's text, when a person accepted it in their own words
	Version         int32
}

// Citation is a document a proposal cites, by its kind and id.
type Citation struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
}

// commentSelect reads comments with their item's name and snapshot's date.
// The item and snapshot are read by subquery, not joined, so that the
// listing's keyset may name the comment's own id unqualified.
const commentSelect = `SELECT c.id, c.legal_entity_id, c.period_from, c.period_to, c.lag, c.liquidity_item,
		(SELECT i.name FROM cash.liquidity_item i WHERE i.code = c.liquidity_item),
		c.snapshot_id, (SELECT s.snapshot_date FROM cash.snapshot s WHERE s.id = c.snapshot_id),
		c.text, c.status, c.decided_by, c.decided_at, c.created_at, c.created_by, c.session_id, c.citations, c.decision_reason, c.proposed_text, c.version
	FROM cash.variance_comment c`

func scanComment(r pgx.Row) (VarianceComment, error) {
	var c VarianceComment
	err := r.Scan(&c.ID, &c.LegalEntity, &c.From, &c.To, &c.Lag, &c.Item, &c.ItemName, &c.Snapshot, &c.SnapshotDate, &c.Text, &c.Status,
		&c.DecidedBy, &c.DecidedAt, &c.CreatedAt, &c.CreatedBy, &c.Session, &c.Citations, &c.DecisionReason, &c.ProposedText, &c.Version)
	return c, errs.Wrapf(err, "scan variance comment")
}

// CommentVariance writes a person's comment on the legal entity's
// variance, accepted as written, audits it and publishes
// VarianceCommentDecided. The comment is checked as the variance's
// request is: a legal entity the caller cannot act in is not found; a lag
// other than 0, 7 and 30, a period ending before it starts, longer than
// the variance's longest, after the company's date or past the
// snapshot's horizon is refused; a period whose snapshot was not taken is
// a conflict. A liquidity item not the tenant's, or both an item and the
// unclassified row, is refused.
func (e *Endpoints) CommentVariance(ctx context.Context, in CommentInput) (VarianceComment, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	from, to := clock.Day(in.From), clock.Day(in.To)
	if err := checkComment(in, from, to); err != nil {
		return VarianceComment{}, err
	}
	co, s, err := e.commented(ctx, tx, in.LegalEntity, from, to, in.Lag)
	if err != nil {
		return VarianceComment{}, err
	}
	c := VarianceComment{ID: uuid.Must(uuid.NewV7()), LegalEntity: in.LegalEntity, CompanyCode: co.Code, From: from, To: to, Lag: in.Lag,
		Snapshot: s.ID, SnapshotDate: s.Date, Text: in.Text, Status: CommentAccepted, DecidedBy: &p.ID, CreatedBy: p.ID, Citations: []Citation{}, Version: 1}
	if !in.Unclassified {
		if c.ItemName, err = itemName(ctx, tx, in.Item); err != nil {
			return VarianceComment{}, err
		}
		c.Item = &in.Item
	}
	err = tx.QueryRow(ctx, `INSERT INTO cash.variance_comment (tenant_id, id, legal_entity_id, period_from, period_to, lag, liquidity_item, snapshot_id,
			text, status, decided_by, decided_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now(), $11) RETURNING created_at`,
		p.TenantID, c.ID, c.LegalEntity, c.From, c.To, c.Lag, c.Item, c.Snapshot, c.Text, c.Status, p.ID).Scan(&c.CreatedAt)
	if err != nil {
		return VarianceComment{}, fmt.Errorf("cash: comment on the variance of %s: %w", in.LegalEntity, err)
	}
	c.DecidedAt = &c.CreatedAt
	return c, decided(ctx, c, "cash.variance_comment.create")
}

// textInvalid refuses a comment's text of nothing but spaces or of more
// than 2,000 characters; a new problem each time, since a problem is
// changed as it is written.
func textInvalid() *problem.Problem {
	return problem.Unprocessable("request.validation_failed", "A comment explains the variance in words, in at most 2,000 characters.",
		problem.FieldError{Field: "text", Code: "invalid", Message: "1 to 2,000 characters, not blank"})
}

// checkComment refuses what the comment says before anything is read: a
// blank text (by unicode.IsSpace, wider than the database's [:space:], so
// the database's rule is never what refuses it), a lag not offered, a
// period backwards or longer than any grain's limit (36 months), both an
// item and the unclassified row, or neither.
func checkComment(in CommentInput, from, to time.Time) error {
	if strings.TrimSpace(in.Text) == "" || utf8.RuneCountInString(in.Text) > maxCommentText {
		return textInvalid()
	}
	if err := checkLag(in.Lag); err != nil {
		return err
	}
	if _, err := actualBuckets(from, to, config.GrainMonth); err != nil {
		return err
	}
	switch {
	case in.Item != "" && in.Unclassified:
		return problem.Unprocessable("request.validation_failed", "A liquidity item and the row of what no rule assigns are two different rows.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	case in.Item == "" && !in.Unclassified:
		return problem.Unprocessable("request.validation_failed", "A comment explains one row of the variance: a liquidity item, or what no rule assigns.",
			problem.FieldError{Field: "liquidity_item", Code: "required", Message: "a liquidity item, or unclassified true"})
	}
	return nil
}

// commented is the company of the variance a comment explains and the
// snapshot that variance compares with, refused as the variance refuses
// its request.
func (e *Endpoints) commented(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, from, to time.Time, lag int) (Company, Snapshot, error) {
	co, err := e.company(ctx, legalEntity)
	if err != nil {
		return Company{}, Snapshot{}, err
	}
	doc, _, err := e.Settings.Document(ctx)
	if err != nil {
		return Company{}, Snapshot{}, fmt.Errorf("cash: configuration: %w", err)
	}
	read, _, err := e.readDate(doc, co.Code)
	if err != nil {
		return Company{}, Snapshot{}, err
	}
	if err := checkPassed(to, read); err != nil {
		return Company{}, Snapshot{}, err
	}
	s, err := snapshotOn(ctx, tx, legalEntity, from.AddDate(0, 0, -lag))
	if err != nil {
		return Company{}, Snapshot{}, err
	}
	return co, s, checkHorizon(to, s)
}

// itemName is the name of the tenant's liquidity item, active or not (a
// snapshot keeps the items it was taken with); one the tenant has not is
// refused.
func itemName(ctx context.Context, tx pgx.Tx, code string) (*string, error) {
	var name string
	err := tx.QueryRow(ctx, `SELECT name FROM cash.liquidity_item WHERE code = $1`, code).Scan(&name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, problem.Unprocessable("request.validation_failed", "The liquidity item is not one of the tenant's.",
			problem.FieldError{Field: "liquidity_item", Code: "invalid", Message: "a liquidity item of the tenant"})
	case err != nil:
		return nil, fmt.Errorf("cash: liquidity item %s: %w", code, err)
	}
	return &name, nil
}

// decided audits a comment's decision and publishes VarianceCommentDecided.
func decided(ctx context.Context, c VarianceComment, action string) error {
	payload := map[string]any{"variance_comment_id": c.ID, "legal_entity_id": c.LegalEntity, "company_code": c.CompanyCode,
		"from": c.From.Format(time.DateOnly), "to": c.To.Format(time.DateOnly), "lag": c.Lag, "liquidity_item": c.Item,
		"snapshot_id": c.Snapshot, "status": c.Status, "decided_by": c.DecidedBy}
	detail := map[string]any{"text": c.Text}
	if c.DecisionReason != nil {
		detail["reason"] = *c.DecisionReason
	}
	if c.ProposedText != nil {
		detail["proposed_text"] = *c.ProposedText
	}
	for k, v := range payload {
		detail[k] = v
	}
	return audit.Record(ctx, action, "cash.variance_comment", c.ID, detail,
		outbox.Event{Context: "cash", Name: "variance_comment_decided", AggregateType: "cash.variance_comment", AggregateID: c.ID, Payload: payload})
}

// VarianceCommentOf is a comment on a legal entity the caller may act in;
// any other is not found.
func (e *Endpoints) VarianceCommentOf(ctx context.Context, id uuid.UUID) (VarianceComment, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return VarianceComment{}, err
	}
	c, err := scanComment(tx.QueryRow(ctx, commentSelect+` WHERE c.id = $1`, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !access.Allows(c.LegalEntity)):
		return VarianceComment{}, problem.NotFoundOf("variance comment")
	case err != nil:
		return VarianceComment{}, fmt.Errorf("cash: variance comment %s: %w", id, err)
	}
	cs := []VarianceComment{c}
	if err := e.coded(ctx, cs); err != nil {
		return VarianceComment{}, err
	}
	return cs[0], nil
}

// CommentsQuery filters the comments listed: the legal entity, the
// liquidity item or the row of what no rule assigns, the lag, the status,
// and periods that end on or after From and start on or before To.
type CommentsQuery struct {
	LegalEntity  *uuid.UUID
	Item         string
	Unclassified bool
	Lag          *int
	Status       string
	From, To     *time.Time
	Page         pagination.Request
}

var commentSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "c.created_at", Type: "timestamptz"},
}}

// VarianceComments lists a page of the comments on the legal entities the
// caller may act in, newest first, and the next page's token ("" on the
// last). Both an item and the unclassified row is refused.
func (e *Endpoints) VarianceComments(ctx context.Context, q CommentsQuery) ([]VarianceComment, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	if q.Item != "" && q.Unclassified {
		return nil, "", problem.Unprocessable("request.validation_failed", "A liquidity item and the row of what no rule assigns are two different selections.",
			problem.FieldError{Field: "unclassified", Code: "invalid", Message: "not with liquidity_item"})
	}
	scope := fmt.Sprintf("listForecastVarianceComments|%s|%s|%v|%v|%s|%s|%s", q.LegalEntity, q.Item, q.Unclassified, lagText(q.Lag), q.Status, dayText(q.From), dayText(q.To))
	ks, err := e.Pages.Keyset(commentSort, scope, q.Page)
	if err != nil {
		return nil, "", err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, "", err
	}
	query, args := ks.SQL(commentSelect+` WHERE ($1 OR c.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR c.legal_entity_id = $3)
		AND ($4 = '' OR c.liquidity_item = $4) AND (NOT $5 OR c.liquidity_item IS NULL) AND ($6::int IS NULL OR c.lag = $6)
		AND ($7 = '' OR c.status = $7) AND ($8::date IS NULL OR c.period_to >= $8) AND ($9::date IS NULL OR c.period_from <= $9)`,
		[]any{access.All, access.IDs, q.LegalEntity, q.Item, q.Unclassified, q.Lag, q.Status, q.From, q.To})
	rows, _ := tx.Query(ctx, query, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (VarianceComment, error) { return scanComment(r) })
	if err != nil {
		return nil, "", fmt.Errorf("cash: list variance comments: %w", err)
	}
	cs, next := pagination.Page(ks, cs, func(c VarianceComment) (string, uuid.UUID) { return c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ID })
	return cs, next, e.coded(ctx, cs)
}

// lagText is a lag filter as a page token's scope holds it.
func lagText(lag *int) string {
	if lag == nil {
		return ""
	}
	return fmt.Sprint(*lag)
}

// coded gives each comment its company's code.
func (e *Endpoints) coded(ctx context.Context, cs []VarianceComment) error {
	entities := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		entities[i] = c.LegalEntity
	}
	codes, err := e.companyCodes(ctx, entities)
	if err != nil {
		return fmt.Errorf("cash: variance comments: %w", err)
	}
	for i, c := range cs {
		cs[i].CompanyCode = codes[c.LegalEntity]
	}
	return nil
}
