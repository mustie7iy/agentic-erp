package audit_test

import (
	"context"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/audit"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var perms = []string{"audit.event.read", "audit.chain.verify", "platform.operation.read"}

func setup(t *testing.T) apitest.Client {
	t.Helper()
	ten := pgtest.NewTenant(t, perms...)
	c := apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
	obj := uuid.MustParse("01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01")
	for i := range 3 {
		if err := apitest.Committed(t, ten, ten.Human, func(ctx context.Context) error {
			id := uuid.Nil
			if i < 2 {
				id = obj
			}
			return trail.Add(ctx, trail.Entry{Action: "test.thing.do", ObjectType: "test.thing", ObjectID: id, Outcome: "succeeded",
				Detail: map[string]any{"i": i}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func endpoints() *audit.Endpoints {
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	return &audit.Endpoints{Pages: pages}
}

func TestAuditEvents(t *testing.T) {
	c := setup(t)
	seqs := func(page map[string]any) []float64 {
		var out []float64
		for _, it := range page["items"].([]any) {
			out = append(out, it.(map[string]any)["seq"].(float64))
		}
		return out
	}
	page, _ := c.Must(200, "GET", "/v1/audit-events?page_size=2", "", "")
	first := page["items"].([]any)[0].(map[string]any)
	if got := seqs(page); len(got) != 2 || got[0] != 3 {
		t.Fatalf("newest first: got seqs %v, want 2 from seq 3", got)
	}
	if page["next_page_token"] == nil {
		t.Fatalf("newest first: got no next page token, want one")
	}
	if first["object_id"] != nil {
		t.Errorf("newest first object_id: got %v, want nil", first["object_id"])
	}
	if i := first["detail"].(map[string]any)["i"]; i != float64(2) {
		t.Errorf("newest first detail.i: got %v, want 2", i)
	}
	if want := c.Tenant.Human.ID.String(); first["actor_id"] != want {
		t.Errorf("newest first actor_id: got %v, want %s", first["actor_id"], want)
	}
	rest, _ := c.Must(200, "GET", "/v1/audit-events?page_size=2&page_token="+url.QueryEscape(page["next_page_token"].(string)), "", "")
	if got := seqs(rest); len(got) != 1 || got[0] != 1 {
		t.Fatalf("second page: got seqs %v, want [1]", got)
	}
	if rest["next_page_token"] != nil {
		t.Fatalf("second page: got next page token %v, want none", rest["next_page_token"])
	}
	from := url.QueryEscape(time.Now().Add(-time.Hour).Format(time.RFC3339))
	to := url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))
	asc, _ := c.Must(200, "GET", "/v1/audit-events?order_by=occurred_at&object_type=test.thing&object_id=01929a1e-4c2b-7a10-9b3e-1f2a3b4c5d01&occurred_from="+
		from+"&occurred_to="+to, "", "")
	if got := seqs(asc); len(got) != 2 || got[0] != 1 {
		t.Fatalf("filtered: %v", asc)
	}
	c.Problem(400, "request.invalid_order_by", "GET", "/v1/audit-events?order_by=action", "", "")
	c.Problem(400, "request.invalid_page_token", "GET", "/v1/audit-events?page_token=x", "", "")

	op, _ := c.Must(202, "POST", "/v1/audit-events:verify", `{}`, "")
	if op["status"] != "succeeded" {
		t.Fatalf("verify status: got %v, want succeeded: %v", op["status"], op)
	}
	result := op["result"].(map[string]any)
	if result["verified"] != true {
		t.Errorf("verify result verified: got %v, want true", result["verified"])
	}
	if result["last_seq"] != float64(3) {
		t.Errorf("verify result last_seq: got %v, want 3", result["last_seq"])
	}
	c.Must(202, "POST", "/v1/audit-events:verify", `{"from_seq": 2, "to_seq": 3}`, "")
	if last, _ := c.Must(200, "GET", "/v1/audit-events?page_size=1", "", ""); last["items"].([]any)[0].(map[string]any)["trace_id"] == nil {
		t.Fatalf("verification without its trace: %v", last)
	}
	for name, body := range map[string]string{"from past the head": `{"from_seq": 99}`, "to past the head": `{"to_seq": 99}`, "from after to": `{"from_seq": 3, "to_seq": 2}`} {
		t.Run(name, func(t *testing.T) {
			sc := c
			sc.T = t
			sc.Problem(422, "audit.invalid_range", "POST", "/v1/audit-events:verify", body, "")
		})
	}
	// A tampered chain is reported, not refused.
	admin := pgtest.Admin(t)
	if _, err := admin.Exec(context.Background(), "SET session_replication_role = replica"); err != nil {
		t.Fatalf("disable triggers: %v", err)
	}
	if _, err := admin.Exec(context.Background(), `UPDATE audit.audit_event SET detail = '{"i": 9}' WHERE tenant_id = $1 AND seq = 2`, c.Tenant.ID); err != nil {
		t.Fatal(err)
	}
	op, _ = c.Must(202, "POST", "/v1/audit-events:verify", `{}`, "")
	if r := op["result"].(map[string]any); r["verified"] != false || r["first_broken_seq"] != float64(2) {
		t.Fatalf("tampered: %v", op)
	}
}

func TestDatabaseFailures(t *testing.T) {
	c := setup(t)
	h := endpoints()
	from, to := int64(1), int64(2)
	for name, call := range map[string]func(ctx context.Context) error{
		"list": func(ctx context.Context) error {
			_, err := h.ListAuditEvents(ctx, gen.ListAuditEventsParams{})
			return err
		},
		"verify": func(ctx context.Context) error {
			_, err := h.VerifyAuditChain(ctx, &gen.AuditVerify{FromSeq: gen.NewOptInt64(from), ToSeq: gen.NewOptInt64(to)}, gen.VerifyAuditChainParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if call(context.Background()) == nil {
				t.Errorf("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, c.Tenant, c.Tenant.Human, name, call)
		})
	}
	if err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) error {
		_, err := h.ListAuditEvents(ctx, gen.ListAuditEventsParams{PageSize: gen.NewOptInt32(0)})
		return err
	}); err == nil {
		t.Error("page size 0 accepted")
	}
}

// TestAuditEventAddress: an event shows a person where its request came
// from, and null where no request made it (ADR-0011 as amended by T73c);
// an agent sees neither.
func TestAuditEventAddress(t *testing.T) {
	c := setup(t)
	ctx := reqctx.WithClientAddress(context.Background(), netip.MustParseAddr("2001:db8::24"))
	if err := pgtest.Pool(t).InTx(ctx, database.Scope{Role: "erp_app", TenantID: c.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
		return trail.Write(ctx, tx, reqctx.Principal{ID: c.Tenant.Human.ID, Kind: "human", TenantID: c.Tenant.ID}, "", time.Now(),
			trail.Entry{Action: "test.thing.do", ObjectType: "test.thing", Outcome: "succeeded"})
	}); err != nil {
		t.Fatal(err)
	}
	page, _ := c.Must(200, "GET", "/v1/audit-events?page_size=2", "", "")
	items := apitest.List(t, page, "items")
	for i, want := range []any{"2001:db8::24", nil} {
		got, present := items[i].(map[string]any)["client_address"]
		if !present {
			t.Errorf("event %d client_address: absent, want %v", i, want)
		}
		if got != want {
			t.Errorf("event %d client_address: got %v, want %v", i, got, want)
		}
	}
	// An agent holding audit.event.read reads the same events without the
	// address, absent rather than null (product owner 2026-10-22).
	c.As = c.Tenant.Agent
	page, _ = c.Must(200, "GET", "/v1/audit-events?page_size=2", "", "")
	for i, item := range apitest.List(t, page, "items") {
		if got, present := item.(map[string]any)["client_address"]; present {
			t.Errorf("agent's event %d client_address: got %v, want it absent", i, got)
		}
	}
}
