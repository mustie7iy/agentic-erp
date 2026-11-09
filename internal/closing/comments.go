package closing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Statuses of a variance comment (ADR-0058 §5, §10).
const (
	CommentProposed   = "proposed"
	CommentAccepted   = "accepted"
	CommentRejected   = "rejected"
	CommentSuperseded = "superseded"
)

// citation is a journal entry a comment cites, as it was cited.
type citation struct {
	EntryID     uuid.UUID `json:"journal_entry_id"`
	FiscalYear  int16     `json:"fiscal_year"`
	Number      string    `json:"document_number"`
	PostingDate string    `json:"posting_date"`
}

// comment is a stored variance comment with its close's legal entity and
// period, and its citations in the order given.
type comment struct {
	ID, CloseID, LegalEntityID uuid.UUID
	FiscalYear, FiscalPeriod   int16
	Basis                      string
	AccountID                  uuid.UUID
	AccountNumber, AccountName string
	Text                       string
	ProposedText               *string
	AuthorKind                 string
	AuthorID                   uuid.UUID
	SessionID                  *uuid.UUID
	Currency                   string
	Actual, Comparison         money.Minor
	Status                     string
	DecidedBy                  *uuid.UUID
	DecidedAt                  *time.Time
	Reason                     *string
	Version                    int32
	CreatedAt, UpdatedAt       time.Time
	Citations                  []citation
}

const commentSelect = `SELECT v.id, v.close_id, c.legal_entity_id, c.fiscal_year, c.fiscal_period, v.basis, v.account_id, v.account_number, v.account_name,
		v.text, v.proposed_text, v.author_kind, v.author_id, v.session_id, v.currency, v.actual, v.comparison, v.status, v.decided_by, v.decided_at,
		v.decision_reason, v.version, v.created_at, v.updated_at,
		coalesce((SELECT jsonb_agg(jsonb_build_object('journal_entry_id', ci.journal_entry_id, 'fiscal_year', ci.journal_entry_year,
			'document_number', ci.document_number, 'posting_date', ci.posting_date) ORDER BY ci.seq)
			FROM closing.variance_citation ci WHERE ci.tenant_id = v.tenant_id AND ci.comment_id = v.id), '[]')
	FROM closing.variance_comment v JOIN closing.period_close c ON c.tenant_id = v.tenant_id AND c.id = v.close_id`

func scanComment(r pgx.Row) (comment, error) {
	var c comment
	err := r.Scan(&c.ID, &c.CloseID, &c.LegalEntityID, &c.FiscalYear, &c.FiscalPeriod, &c.Basis, &c.AccountID, &c.AccountNumber, &c.AccountName,
		&c.Text, &c.ProposedText, &c.AuthorKind, &c.AuthorID, &c.SessionID, &c.Currency, &c.Actual, &c.Comparison, &c.Status, &c.DecidedBy, &c.DecidedAt,
		&c.Reason, &c.Version, &c.CreatedAt, &c.UpdatedAt, &c.Citations)
	return c, errs.Wrapf(err, "scan variance comment")
}

// readComment reads a comment of a close of a legal entity the caller may
// act in; any other is not found.
func readComment(ctx context.Context, tx pgx.Tx, id uuid.UUID) (comment, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return comment{}, err
	}
	c, err := scanComment(tx.QueryRow(ctx, commentSelect+` WHERE v.id = $1`, id))
	if err = authz.Visible(err, access, c.LegalEntityID, problem.NotFoundOf("variance comment"), "closing: read variance comment %s", id); err != nil {
		return comment{}, err
	}
	return c, nil
}

// rowComments reads a close's comments of a basis that are accepted or
// proposed, by account.
func rowComments(ctx context.Context, tx pgx.Tx, closeID uuid.UUID, basis string) (map[uuid.UUID][]comment, error) {
	rows, _ := tx.Query(ctx, commentSelect+` WHERE v.close_id = $1 AND v.basis = $2 AND v.status IN ('accepted', 'proposed') ORDER BY v.id`, closeID, basis)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (comment, error) { return scanComment(r) })
	if err != nil {
		return nil, fmt.Errorf("closing: comments of close %s against %s: %w", closeID, basis, err)
	}
	out := map[uuid.UUID][]comment{}
	for _, c := range cs {
		out[c.AccountID] = append(out[c.AccountID], c)
	}
	return out, nil
}

// commentInput is a comment a person or an agent writes; Session is the
// agent's, nil for a person's.
type commentInput struct {
	CloseID, AccountID uuid.UUID
	Basis, Text        string
	Citations          []uuid.UUID
	Session            *uuid.UUID
}

// ProposeVarianceComment implements POST /v1/variance-comment-proposals
// (ADR-0058 §10): the calling agent's comment on a row, in its running
// session, proposed for a person to decide; the same proposal again
// answers it as it is.
func (e *Endpoints) ProposeVarianceComment(ctx context.Context, req *gen.VarianceCommentProposal, _ gen.ProposeVarianceCommentParams) (gen.ProposeVarianceCommentRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	if p.Kind != reqctx.KindAgent {
		return nil, problem.Forbidden("agents.not_an_agent", "Only agents propose variance comments; a person writes one (POST /v1/variance-comments).")
	}
	if err := e.Sessions.Running(ctx, req.SessionID); err != nil {
		return nil, err
	}
	c, created, err := e.writeComment(ctx, tx, p, commentInput{CloseID: req.CloseID, AccountID: req.AccountID, Basis: req.Basis, Text: req.Text,
		Citations: req.Citations, Session: &req.SessionID})
	if err != nil {
		return nil, err
	}
	out, err := e.commentOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if !created {
		return &gen.VarianceCommentHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(commentPath(c.ID)), Response: out}, nil
	}
	return &gen.ProposeVarianceCommentCreatedHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(commentPath(c.ID)),
		Response: out}, nil
}

// CreateVarianceComment implements POST /v1/variance-comments (ADR-0058
// §10): a person's comment on a row, accepted at once.
func (e *Endpoints) CreateVarianceComment(ctx context.Context, req *gen.VarianceCommentInput, _ gen.CreateVarianceCommentParams) (gen.CreateVarianceCommentRes, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, _, err := e.writeComment(ctx, tx, p, commentInput{CloseID: req.CloseID, AccountID: req.AccountID, Basis: req.Basis, Text: req.Text, Citations: req.Citations})
	if err != nil {
		return nil, err
	}
	out, err := e.commentOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.ProposeVarianceCommentCreatedHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString(commentPath(c.ID)),
		Response: out}, nil
}

func commentPath(id uuid.UUID) string { return "/v1/variance-comments/" + id.String() }

// writeComment writes a comment on a row of the close's report against
// the basis, under the close's lock (ADR-0058 §5): the row must be one of
// the report's and each citation an entry with a line on its account in a
// period the row compares. An agent's comment is proposed, and the
// session's proposal for the row answers again when it is the same
// (created false); a person's is accepted, superseding the row's comment
// accepted before.
func (e *Endpoints) writeComment(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in commentInput) (comment, bool, error) {
	if err := problem.RequireText("text", in.Text); err != nil {
		return comment{}, false, err
	}
	if _, err := lockClose(ctx, tx, periodCloses, in.CloseID); err != nil {
		return comment{}, false, err
	}
	unlocked, err := e.writable(ctx, tx, periodCloses, in.CloseID)
	if err != nil {
		return comment{}, false, err
	}
	// A retried proposal answers as it is stored, whatever happened to the
	// row since.
	if in.Session != nil {
		if c, found, err := proposedBefore(ctx, tx, in); found || err != nil {
			return c, false, err
		}
	}
	r, err := e.report(ctx, tx, in.CloseID, in.Basis)
	if err == nil {
		err = r.refused()
	}
	if err != nil {
		return comment{}, false, err
	}
	row, ok := r.row(in.AccountID)
	if !ok {
		return comment{}, false, problem.Unprocessable("closing.variance_row_unknown", "The variance report of the close against "+in.Basis+
			" has no row of this account: it is no profit and loss account of the company with a figure in either period.",
			problem.FieldError{Field: "account_id", Code: "unknown", Message: "an account with a row of the report against the basis"})
	}
	cited, err := e.cite(ctx, r, in.AccountID, in.Citations)
	if err != nil {
		return comment{}, false, err
	}
	if err := reopen(ctx, tx, periodCloses, p, in.CloseID, unlocked); err != nil {
		return comment{}, false, err
	}
	c, err := insertComment(ctx, tx, p, in, r, row, cited)
	return c, err == nil, err
}

// proposedBefore finds the session's proposal for the row: the same one
// again (its text as proposed, before a person replaced it, and its
// citations) answers it, another is refused.
func proposedBefore(ctx context.Context, tx pgx.Tx, in commentInput) (comment, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM closing.variance_comment WHERE close_id = $1 AND basis = $2 AND account_id = $3 AND session_id = $4`,
		in.CloseID, in.Basis, in.AccountID, *in.Session).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return comment{}, false, nil
	}
	if err != nil {
		return comment{}, false, fmt.Errorf("closing: proposal of session %s: %w", *in.Session, err)
	}
	c, err := readComment(ctx, tx, id)
	if err != nil {
		return comment{}, false, err
	}
	cited := make([]uuid.UUID, len(c.Citations))
	for i, ci := range c.Citations {
		cited[i] = ci.EntryID
	}
	proposed := c.Text
	if c.ProposedText != nil {
		proposed = *c.ProposedText
	}
	if proposed != in.Text || !slices.Equal(cited, in.Citations) {
		return comment{}, false, problem.Conflict("closing.comment_already_proposed", "The session proposed comment "+c.ID.String()+
			" for this row already; one proposal per session and row.")
	}
	return c, true, nil
}

// cite checks each citation: a posted journal entry of the close's
// company in the leading ledger with a line on the account in the close's
// period or, for a prior basis, in the period compared against.
func (e *Endpoints) cite(ctx context.Context, r report, account uuid.UUID, ids []uuid.UUID) ([]citation, error) {
	found, err := e.Ledger.Cited(ctx, r.close.LegalEntityID, account, ids)
	if err != nil {
		return nil, fmt.Errorf("closing: entries cited on account %s: %w", account, err)
	}
	type yearPeriod struct{ year, period int16 }
	periods := []yearPeriod{{r.close.FiscalYear, r.close.FiscalPeriod}}
	if r.basis != config.BasisPlan {
		periods = append(periods, yearPeriod{r.compYear, r.compPeriod})
	}
	out := make([]citation, len(ids))
	for i, id := range ids {
		c, ok := found[id]
		if !ok || !slices.ContainsFunc(periods, func(y yearPeriod) bool { return y.year == c.FiscalYear && slices.Contains(c.Periods, y.period) }) {
			return nil, problem.Unprocessable("closing.citation_invalid", fmt.Sprintf("Journal entry %s has no line on the account in the leading ledger "+
				"in the periods the row compares; a comment cites the entries its figures are made of.", id),
				problem.FieldError{Field: fmt.Sprintf("citations[%d]", i), Code: "invalid", Message: "a journal entry with a line on the account in the row's periods"})
		}
		out[i] = citation{EntryID: id, FiscalYear: c.FiscalYear, Number: c.DocumentNumber, PostingDate: c.PostingDate.Format(time.DateOnly)}
	}
	return out, nil
}

// insertComment writes the comment with the row's figures and its
// citations: an agent's proposed, a person's accepted, superseding the
// row's comment accepted before; each change is audited and published.
func insertComment(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in commentInput, r report, row varianceRow, cited []citation) (comment, error) {
	status, kind, decided := CommentProposed, "agent", (*uuid.UUID)(nil)
	if in.Session == nil {
		status, kind, decided = CommentAccepted, "human", &p.ID
		if err := supersede(ctx, tx, p, in.CloseID, in.Basis, in.AccountID); err != nil {
			return comment{}, err
		}
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO closing.variance_comment (tenant_id, id, close_id, basis, account_id, account_number, account_name, text,
			author_kind, author_id, session_id, currency, actual, comparison, status, decided_by, decided_at, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, CASE WHEN $16::uuid IS NOT NULL THEN now() END, $10, $10)`,
		p.TenantID, id, in.CloseID, in.Basis, in.AccountID, row.Number, row.Name, in.Text, kind, p.ID, in.Session, r.company.LocalCurrency, row.Net,
		row.comparison, status, decided); err != nil {
		return comment{}, fmt.Errorf("closing: insert variance comment of close %s: %w", in.CloseID, err)
	}
	for i, ci := range cited {
		if _, err := tx.Exec(ctx, `INSERT INTO closing.variance_citation (tenant_id, comment_id, seq, journal_entry_year, journal_entry_id, document_number,
			posting_date) VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, i+1, ci.FiscalYear, ci.EntryID, ci.Number, ci.PostingDate); err != nil {
			return comment{}, fmt.Errorf("closing: cite entry %s in variance comment %s: %w", ci.EntryID, id, err)
		}
	}
	c, err := readComment(ctx, tx, id)
	if err != nil {
		return comment{}, err
	}
	action := "write"
	if in.Session != nil {
		action = "propose"
	}
	return c, recordComment(ctx, c, status, action)
}

// supersede sets the row's accepted comment superseded, for the one
// accepted or written now; the partial unique index holds one accepted
// comment per row (ADR-0058 §5).
func supersede(ctx context.Context, tx pgx.Tx, p reqctx.Principal, closeID uuid.UUID, basis string, account uuid.UUID) error {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `UPDATE closing.variance_comment SET status = 'superseded', version = version + 1, updated_by = $4
		WHERE close_id = $1 AND basis = $2 AND account_id = $3 AND status = 'accepted' RETURNING id`, closeID, basis, account, p.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("closing: supersede the comment of account %s in close %s: %w", account, closeID, err)
	}
	c, err := readComment(ctx, tx, id)
	if err != nil {
		return err
	}
	return recordComment(ctx, c, CommentSuperseded, "supersede")
}

// recordComment audits a change of a comment and publishes it, as the
// comment is after it.
func recordComment(ctx context.Context, c comment, change, action string) error {
	detail := map[string]any{"period_close_id": c.CloseID, "legal_entity_id": c.LegalEntityID, "basis": c.Basis, "account_id": c.AccountID, "text": c.Text,
		"proposed_text": c.ProposedText, "status": c.Status, "reason": c.Reason, "actual": c.Actual, "comparison": c.Comparison, "currency": c.Currency,
		"citations": c.Citations, "session_id": c.SessionID, "version": c.Version}
	return audit.Record(ctx, "closing.variance_comment."+action, "closing.variance_comment", c.ID, detail,
		outbox.Event{Context: "closing", Name: "variance_comment_changed", AggregateType: "closing.variance_comment", AggregateID: c.ID,
			Payload: map[string]any{"variance_comment_id": c.ID, "period_close_id": c.CloseID, "legal_entity_id": c.LegalEntityID, "fiscal_year": c.FiscalYear,
				"fiscal_period": c.FiscalPeriod, "basis": c.Basis, "account_id": c.AccountID, "change": change, "author_kind": c.AuthorKind,
				"edited": c.ProposedText != nil, "reason": c.Reason, "status": c.Status, "version": c.Version}})
}

// decision changes a proposed comment of a locked close.
type decision func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c comment) (change string, err error)

// decide runs a person's decision on a proposed comment (ADR-0058 §10):
// its close is locked first, so writes on the close serialise; the
// comment is read again under the lock and its version checked against
// If-Match; a signed-off close is reopened first, or refused while its
// periods are all locked (§7).
func (e *Endpoints) decide(ctx context.Context, id uuid.UUID, ifMatch, action string, do decision) (*gen.AcceptVarianceCommentOKHeaders, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := readComment(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if _, err := lockClose(ctx, tx, periodCloses, c.CloseID); err != nil {
		return nil, err
	}
	if c, err = readComment(ctx, tx, id); err != nil {
		return nil, err
	}
	if err := etag.IfMatch(ifMatch)(c.Version); err != nil {
		return nil, err
	}
	unlocked, err := e.writable(ctx, tx, periodCloses, c.CloseID)
	if err != nil {
		return nil, err
	}
	if c.Status != CommentProposed {
		return nil, problem.Conflict("closing.comment_not_proposed", "Variance comment "+c.ID.String()+" is "+c.Status+"; only a proposed one is accepted or rejected.")
	}
	if err := reopen(ctx, tx, periodCloses, p, c.CloseID, unlocked); err != nil {
		return nil, err
	}
	change, err := do(ctx, tx, p, c)
	if err != nil {
		return nil, err
	}
	if c, err = readComment(ctx, tx, id); err != nil {
		return nil, err
	}
	if err := recordComment(ctx, c, change, action); err != nil {
		return nil, err
	}
	out, err := e.commentOut(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	return &gen.AcceptVarianceCommentOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: out}, nil
}

// AcceptVarianceComment implements POST /v1/variance-comments/{id}:acceptProposal:
// a person accepts a proposed comment, its text as it is or replaced, and
// the row's comment accepted before is superseded.
func (e *Endpoints) AcceptVarianceComment(ctx context.Context, req gen.OptVarianceCommentAccept, params gen.AcceptVarianceCommentParams) (gen.AcceptVarianceCommentRes, error) {
	return e.decide(ctx, params.ID, params.IfMatch, "accept", func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c comment) (string, error) {
		text := c.Text
		if t, ok := req.Value.Text.Get(); ok {
			if err := problem.RequireText("text", t); err != nil {
				return "", err
			}
			text = t
		}
		if err := supersede(ctx, tx, p, c.CloseID, c.Basis, c.AccountID); err != nil {
			return "", err
		}
		_, err := tx.Exec(ctx, `UPDATE closing.variance_comment SET status = 'accepted', text = $2, proposed_text = CASE WHEN $2 <> text THEN text END,
				decided_by = $3, decided_at = now(), version = version + 1, updated_by = $3 WHERE id = $1 AND status = 'proposed'`, c.ID, text, p.ID)
		return CommentAccepted, errs.Wrapf(err, "closing: accept variance comment %s", c.ID)
	})
}

// RejectVarianceComment implements POST /v1/variance-comments/{id}:rejectProposal:
// a person rejects a proposed comment, for a reason.
func (e *Endpoints) RejectVarianceComment(ctx context.Context, req *gen.ReasonInput, params gen.RejectVarianceCommentParams) (gen.RejectVarianceCommentRes, error) {
	return e.decide(ctx, params.ID, params.IfMatch, "reject", func(ctx context.Context, tx pgx.Tx, p reqctx.Principal, c comment) (string, error) {
		if err := problem.RequireText("reason", req.Reason); err != nil {
			return "", err
		}
		_, err := tx.Exec(ctx, `UPDATE closing.variance_comment SET status = 'rejected', decision_reason = $2, decided_by = $3, decided_at = now(),
				version = version + 1, updated_by = $3 WHERE id = $1 AND status = 'proposed'`, c.ID, req.Reason, p.ID)
		return CommentRejected, errs.Wrapf(err, "closing: reject variance comment %s", c.ID)
	})
}
