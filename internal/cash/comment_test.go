package cash_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// commentEnv is setupSnapshot's Maple Inc, read on 17 April, with its
// snapshot taken on demand that day and a daily one of 10 April, whose
// people may comment on the variance.
type commentEnv struct {
	drillEnv
	board, daily string
}

func setupComments(t *testing.T) commentEnv {
	t.Helper()
	e := commentEnv{drillEnv: setupSnapshot(t)}
	e.Tenant.Grant(t, "cash.variance.comment", e.Tenant.Human)
	e.board = id(e.take(t, "Board"))
	e.daily = e.snapshotOn(t, "2026-04-10", "2027-04-02")
	return e
}

// snapshotOn writes a daily snapshot of Maple Inc of the day, forecast up
// to horizon.
func (e commentEnv) snapshotOn(t *testing.T, day, horizon string) string {
	t.Helper()
	sid := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, 'daily', $4, 'America/Toronto', $5, 1, $6)`, e.Tenant.ID, sid, e.le, day, horizon, cash.SnapshotterActor)
	return sid.String()
}

// comment writes a comment on Maple Inc's variance through the API.
func (e commentEnv) comment(t *testing.T, fields string) map[string]any {
	t.Helper()
	c, _ := e.Must(201, "POST", "/v1/liquidity-forecast-variance/comments", e.body(fields), "")
	return c
}

func (e commentEnv) body(fields string) string {
	return fmt.Sprintf(`{"legal_entity_id": %q, %s}`, e.le, fields)
}

// commentText renders a comment as "from..to lag item|unclassified
// (name) snapshot-date status: text".
func commentText(c map[string]any) string {
	row := fmt.Sprint(c["liquidity_item"], " (", c["liquidity_item_name"], ")")
	if c["unclassified"] == true {
		row = "unclassified"
	}
	return fmt.Sprintf("%v..%v lag %v %s %v %v: %v", c["from"], c["to"], c["lag"], row, c["snapshot_date"], c["status"], c["text"])
}

// listed renders a page of comments.
func (e commentEnv) listed(t *testing.T, query string) []string {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments"+query, "", "")
	out := []string{}
	for _, c := range p["items"].([]any) {
		out = append(out, commentText(c.(map[string]any)))
	}
	return out
}

// TestVarianceComments: a person comments on a liquidity item's row of the
// day's variance at lag 0 (against the snapshot taken on demand that day)
// and on the unclassified row at lag 7 (against the daily snapshot of a
// week before); each is accepted as written, decided by its author,
// audited and published, read again and listed by its filters, newest
// first, in pages.
func TestVarianceComments(t *testing.T) {
	e := setupComments(t)
	item := e.comment(t, `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "OP_SUPPLIERS", "text": "The run of the 17th paid as forecast."`)
	want(t, "the item's comment", commentText(item), "2026-04-17..2026-04-17 lag 0 OP_SUPPLIERS (Supplier payments) 2026-04-17 accepted: The run of the 17th paid as forecast.")
	want(t, "its snapshot, the board's", item["snapshot_id"], e.board)
	want(t, "its company", item["company_code"], "3000")
	want(t, "its author", item["created_by"], e.Tenant.Human.ID.String())
	want(t, "decided by its author", item["decided_by"], e.Tenant.Human.ID.String())
	want(t, "decided when written", item["decided_at"], item["created_at"])
	other := e.comment(t, `"from": "2026-04-17", "to": "2026-04-17", "unclassified": true, "text": "Receipts not matched yet."`)
	want(t, "the unclassified row's comment", commentText(other), "2026-04-17..2026-04-17 lag 7 unclassified 2026-04-10 accepted: Receipts not matched yet.")
	want(t, "its snapshot, the daily one of the 10th", other["snapshot_id"], e.daily)
	got, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments/"+id(item), "", "")
	want(t, "read again", fmt.Sprint(got), fmt.Sprint(item))

	both := []string{commentText(other), commentText(item)}
	for query, wanted := range map[string][]string{
		"":                                  both,
		"?legal_entity_id=" + e.le.String(): both,
		"?legal_entity_id=" + e.US.String(): {},
		"?liquidity_item=OP_SUPPLIERS":      {commentText(item)},
		"?unclassified=true":                {commentText(other)},
		"?lag=7":                            {commentText(other)},
		"?status=accepted":                  both,
		"?status=proposed":                  {},
		"?from=2026-04-17&to=2026-04-17":    both,
		"?from=2026-04-18":                  {},
		"?to=2026-04-16":                    {},
		"?order_by=created_at":              {commentText(item), commentText(other)},
	} {
		t.Run("listed "+query, func(t *testing.T) { want(t, "comments", e.listed(t, query), wanted) })
	}
	first, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments?page_size=1", "", "")
	want(t, "the first page", len(first["items"].([]any)), 1)
	next, _ := e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments?page_size=1&page_token="+fmt.Sprint(first["next_page_token"]), "", "")
	want(t, "the second page", commentText(next["items"].([]any)[0].(map[string]any)), commentText(item))
	want(t, "the last page's token", next["next_page_token"], nil)

	events := apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "variance_comment_decided"})
	want(t, "events", len(events), 2)
	want(t, "the event", fmt.Sprint(events[0]["variance_comment_id"], " ", events[0]["liquidity_item"], " ", events[0]["status"], " ", events[0]["snapshot_id"],
		" ", events[0]["from"], " ", events[0]["lag"]), fmt.Sprint(id(item), " OP_SUPPLIERS accepted ", e.board, " 2026-04-17 0"))
	want(t, "the unclassified row's event", events[1]["liquidity_item"], nil)
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'cash.variance_comment.create' AND object_id = ANY($2)`,
		[]any{e.Tenant.ID, []string{id(item), id(other)}}, &audited)
	want(t, "audited", audited, 2)

	reader := e.Reader(t, "reader of 1000", e.US)
	e.As(reader).Problem(404, "request.not_found", "GET", "/v1/liquidity-forecast-variance/comments/"+id(item), "", "")
	p, _ := e.As(reader).Must(200, "GET", "/v1/liquidity-forecast-variance/comments", "", "")
	want(t, "listed for a reader of another company", len(p["items"].([]any)), 0)
}

// TestVarianceCommentRefusals: a comment is refused as the variance's
// request is, and for its row; an agent, and a person without the
// permission, may not comment.
func TestVarianceCommentRefusals(t *testing.T) {
	e := setupComments(t)
	e.snapshotOn(t, "2026-04-15", "2026-04-16")
	const text = `"text": "Why."`
	for _, c := range []struct{ name, fields, field string }{
		{"a lag not offered", `"from": "2026-04-17", "to": "2026-04-17", "lag": 5, "liquidity_item": "TAX", ` + text, "lag"},
		{"a period backwards", `"from": "2026-04-17", "to": "2026-04-16", "lag": 0, "liquidity_item": "TAX", ` + text, "to"},
		{"more than 36 months", `"from": "2023-04-01", "to": "2026-04-17", "lag": 0, "liquidity_item": "TAX", ` + text, "to"},
		{"an item and the unclassified row", `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "TAX", "unclassified": true, ` + text, "unclassified"},
		{"no row", `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, ` + text, "liquidity_item"},
		{"an item not the tenant's", `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "NOPE", ` + text, "liquidity_item"},
		{"after the company's date", `"from": "2026-04-17", "to": "2026-04-18", "lag": 0, "liquidity_item": "TAX", ` + text, "to"},
		{"past the snapshot's horizon", `"from": "2026-04-15", "to": "2026-04-17", "lag": 0, "liquidity_item": "TAX", ` + text, "to"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := e.Problem(422, "request.validation_failed", "POST", "/v1/liquidity-forecast-variance/comments", e.body(c.fields), "")
			want(t, "field", fmt.Sprint(p["errors"].([]any)[0].(map[string]any)["field"]), c.field)
		})
	}
	e.Problem(400, "request.invalid_body", "POST", "/v1/liquidity-forecast-variance/comments",
		e.body(`"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "TAX", "text": " \n "`), "")
	e.Problem(409, "cash.snapshot_not_taken", "POST", "/v1/liquidity-forecast-variance/comments",
		e.body(`"from": "2026-04-16", "to": "2026-04-16", "lag": 0, "liquidity_item": "TAX", `+text), "")
	fields := `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "liquidity_item": "TAX", ` + text
	e.As(e.Reader(t, "reader of 1000", e.US)).Problem(404, "request.not_found", "POST", "/v1/liquidity-forecast-variance/comments", e.body(fields), "")
	e.Problem(404, "request.not_found", "POST", "/v1/liquidity-forecast-variance/comments",
		fmt.Sprintf(`{"legal_entity_id": %q, %s}`, uuid.New(), fields), "")
	e.Problem(404, "request.not_found", "GET", "/v1/liquidity-forecast-variance/comments/"+uuid.NewString(), "", "")
	e.Problem(422, "request.validation_failed", "GET", "/v1/liquidity-forecast-variance/comments?liquidity_item=TAX&unclassified=true", "", "")
	e.As(e.Tenant.Agent).Problem(403, "identity.agent_not_allowed", "POST", "/v1/liquidity-forecast-variance/comments", e.body(fields), "")
	e.Tenant.Revoke(t, "cash.variance.comment", e.Tenant.Human)
	e.Problem(403, "identity.permission_missing", "POST", "/v1/liquidity-forecast-variance/comments", e.body(fields), "")
	want(t, "comments written", len(e.listed(t, "")), 0)
}

// TestVarianceCommentKeepsSnapshot: a snapshot past its 25 months is kept
// while a comment refers to it, and one of the same age no comment refers
// to is deleted (ADR-0051 §6); run again, retention deletes nothing.
func TestVarianceCommentKeepsSnapshot(t *testing.T) {
	e := setupComments(t)
	commented, plain := e.snapshotOn(t, "2024-03-02", "2025-03-01"), e.snapshotOn(t, "2024-03-01", "2025-03-01")
	c := e.comment(t, `"from": "2024-03-02", "to": "2024-03-08", "lag": 0, "liquidity_item": "OP_SUPPLIERS", "text": "Kept for the audit of 2024."`)
	want(t, "the comment's snapshot", c["snapshot_id"], commented)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }})
	err := apitest.InRequest(t, e.Tenant, pgtest.Actor{ID: cash.SnapshotterActor}, func(ctx context.Context) error {
		for i, wanted := range []int{1, 0} {
			n, err := endpoints.PruneSnapshots(ctx, e.le, time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC))
			if err != nil {
				return err
			}
			want(t, fmt.Sprintf("deleted, run %d", i+1), n, wanted)
		}
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		var left []string
		if err := tx.QueryRow(ctx, `SELECT array_agg(id::text) FROM cash.snapshot WHERE id = ANY($1::uuid[])`, []string{commented, plain}).Scan(&left); err != nil {
			return err
		}
		want(t, "kept", left, []string{commented})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Must(200, "GET", "/v1/liquidity-forecast-variance/comments/"+id(c), "", "")
}

// TestVarianceCommentFailures: writing, reading and listing comments fail
// outside a unit of work, with each database statement, and with the
// directory down; the comment's snapshot deleted in a race is the
// problem the application gives first.
func TestVarianceCommentFailures(t *testing.T) {
	e := setupComments(t)
	c := e.comment(t, `"from": "2026-04-17", "to": "2026-04-17", "lag": 0, "unclassified": true, "text": "Why."`)
	day := time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)
	endpoints := func() *cash.Endpoints { return app.Cash(app.Config{Now: func() time.Time { return readAt }}) }
	for name, fn := range map[string]func(ctx context.Context) error{
		"write": func(ctx context.Context) error {
			_, err := endpoints().CommentVariance(ctx, cash.CommentInput{LegalEntity: e.le, From: day, To: day, Item: "TAX", Text: "Again."})
			return err
		},
		"read": func(ctx context.Context) error {
			_, err := endpoints().VarianceCommentOf(ctx, uuid.MustParse(id(c)))
			return err
		},
		"list": func(ctx context.Context) error {
			lag := 0
			_, _, err := endpoints().VarianceComments(ctx, cash.CommentsQuery{Lag: &lag})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want it refused")
			}
			apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, fn)
		})
	}
	written := uuid.MustParse(id(c))
	for name, fn := range map[string]func(ce *cash.Endpoints, ctx context.Context) error{
		"read": func(ce *cash.Endpoints, ctx context.Context) error {
			_, err := ce.VarianceCommentOf(ctx, written)
			return err
		},
		"list": func(ce *cash.Endpoints, ctx context.Context) error {
			_, _, err := ce.VarianceComments(ctx, cash.CommentsQuery{})
			return err
		},
	} {
		t.Run(name+" with the directory down", func(t *testing.T) {
			ce := endpoints()
			ce.LegalEntities = failingEntities{}
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return fn(ce, ctx) })
			if err == nil || !strings.Contains(err.Error(), "directory down") {
				t.Errorf("got %v, want the directory's error", err)
			}
		})
	}
	lost := endpoints()
	lost.Settings = zoneLost{lost.Settings}
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := lost.CommentVariance(ctx, cash.CommentInput{LegalEntity: e.le, From: day, To: day, Item: "TAX", Text: "Again."})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
		t.Errorf("zone lost: got %v, want the company's zone refused", err)
	}
	e.Problem(400, "request.invalid_page_token", "GET", "/v1/liquidity-forecast-variance/comments?page_token=nope", "", "")
	if got := problem.From(&pgconn.PgError{Code: "23503", ConstraintName: "variance_comment_snapshot"}).Code; got != "cash.snapshot_not_taken" {
		t.Errorf("the snapshot deleted in a race: got %s, want cash.snapshot_not_taken", got)
	}
}
