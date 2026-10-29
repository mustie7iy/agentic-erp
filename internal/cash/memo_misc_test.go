package cash_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mustie7iy/agentic-erp/api/gen"

	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// TestListMemoRecords: the records of the caller's companies, filtered,
// sorted and paged (ADR-0051 §7).
func TestListMemoRecords(t *testing.T) {
	e := setupMemo(t)
	a, _ := e.create(t, e.usd("1.00")+`, "value_date": "2026-04-23", "liquidity_item": "REC_CUSTOMERS"`)
	b, _ := e.create(t, e.usd("2.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`)
	c, _ := e.create(t, `"amount": {"amount": "3.00", "currency": "USD"}, "value_date": "2026-04-22", "liquidity_item": "REC_CUSTOMERS"`)
	ids := func(query string) string {
		p, _ := e.Must(200, "GET", "/v1/memo-records?"+query, "", "")
		var out []string
		for _, it := range p["items"].([]any) {
			wantBankCode(t, it.(map[string]any)) // on a later page as on the first
			out = append(out, id(it.(map[string]any)))
		}
		if next, ok := p["next_page_token"]; ok {
			out = append(out, "more:"+fmt.Sprint(next != ""))
		}
		return fmt.Sprint(out)
	}
	// The list joins ref.currency; the next pages prove the keyset condition still resolves.
	after := func(query string) string {
		p, _ := e.Must(200, "GET", "/v1/memo-records?"+query, "", "")
		return query + "&page_token=" + p["next_page_token"].(string)
	}
	for name, c := range map[string]struct{ query, want string }{
		"the last page":             {after("page_size=2"), fmt.Sprint([]string{id(a)})},
		"the next page by creation": {after("page_size=1&order_by=created_at"), fmt.Sprint([]string{id(b), "more:true"})},
		"by value date":             {"legal_entity_id=" + e.le.String(), fmt.Sprint([]string{id(b), id(c), id(a)})},
		"by creation, latest":       {"order_by=created_at%20desc", fmt.Sprint([]string{id(c), id(b), id(a)})},
		"a house bank":              {"house_bank_id=" + e.positionEnv.usd.String(), fmt.Sprint([]string{id(b), id(a)})},
		"value dates":               {"value_from=2026-04-22&value_to=2026-04-22", fmt.Sprint([]string{id(c)})},
		"a status":                  {"status=cancelled", "[]"},
		"a page":                    {"page_size=2", fmt.Sprint([]string{id(b), id(c), "more:true"})},
		"a page by creation":        {"page_size=1&order_by=created_at", fmt.Sprint([]string{id(a), "more:true"})},
		"another legal entity":      {"legal_entity_id=" + e.US.String(), "[]"},
	} {
		t.Run(name, func(t *testing.T) { want(t, "ids", ids(c.query), c.want) })
	}
	e.Problem(400, "request.invalid_order_by", "GET", "/v1/memo-records?order_by=amount", "", "")
	other := e.Reader(t, "reader of 1000", e.US)
	p, _ := e.As(other).Must(200, "GET", "/v1/memo-records", "", "")
	want(t, "another company's reader", p["items"], []any{})
	e.As(other).Problem(404, "request.not_found", "GET", path(a, ""), "", "")
	e.As(other).Problem(404, "request.not_found", "POST", path(a, "cancel"), ``, `"1"`)
	e.Problem(404, "request.not_found", "GET", "/v1/memo-records/"+uuid.NewString(), "", "")
	e.Problem(412, "request.version_mismatch", "POST", path(a, "cancel"), ``, `"9"`)
	got, _ := e.Must(200, "PATCH", path(c, ""), `{"house_bank_id": "`+e.positionEnv.usd.String()+`"}`, `"1"`)
	want(t, "a house bank given", fmt.Sprint(got["house_bank_id"], " ", got["house_bank_code"]), e.positionEnv.usd.String()+" USD")
}

// TestLiquidityItemInUse: a configuration that takes away a liquidity
// item an open memo record names is refused, in the check and when it is
// activated; one a closed record names is not (ADR-0051 §4, §7).
func TestLiquidityItemInUse(t *testing.T) {
	e := setupMemo(t)
	m, tag := e.create(t, `"amount": {"amount": "-1.00", "currency": "USD"}, "value_date": "2026-04-23", "liquidity_item": "TAX"`)
	without := withCash(t)
	without.Cash.LiquidityItems = without.Cash.LiquidityItems[:2] // no TAX, no REC_CUSTOMERS
	without.Cash.Derivation.Defaults.Inflow = ""
	check := func() []config.Finding {
		var fs []config.Finding
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			var err error
			fs, err = cash.Setup{}.Check(ctx, withCash(t), without)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return fs
	}
	want(t, "check", fmt.Sprint(check()), "[[error] cash.liquidity_items[TAX]: open memo records name this liquidity item, which the document no longer has, or no longer has for their direction; move them to another item, or cancel them, first]")
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, without) })
	pgtest.WantErr(t, err, "cash.liquidity_item_in_use")
	flipped := withCash(t)
	for i := range flipped.Cash.LiquidityItems {
		if flipped.Cash.LiquidityItems[i].Code == "TAX" {
			flipped.Cash.LiquidityItems[i].Direction = config.DirectionInflow
		}
	}
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, flipped) })
	pgtest.WantErr(t, err, "cash.liquidity_item_in_use")
	both := withCash(t)
	for i := range both.Cash.LiquidityItems {
		if both.Cash.LiquidityItems[i].Code == "TAX" {
			both.Cash.LiquidityItems[i].Direction = config.DirectionBoth
		}
	}
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, both) }); err != nil {
		t.Errorf("an item for both directions: got %v, want none", err)
	}
	e.Must(200, "POST", path(m, "cancel"), ``, tag)
	want(t, "check after the cancellation", len(check()), 0)
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return cash.Setup{}.Apply(ctx, without) }); err != nil {
		t.Errorf("apply after the cancellation: got %v, want none", err)
	}
}

// TestMemoFailures calls every memo record function outside a unit of
// work and with each of its database statements failing in turn.
func TestMemoFailures(t *testing.T) {
	e := setupMemo(t)
	e.memoScenario(t)
	st := e.statement(t, e.positionEnv.usd, "USD", "2026-04-16", "2026-04-16", 0, 0, nil, "imported")
	e.line(t, st, 1, "2026-04-16", 100, "unmatched", nil)
	one, _ := e.create(t, e.usd("1.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`)
	series, _ := e.create(t, e.usd("1.00")+`, "value_date": "2026-04-03", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly"}`)
	session := e.session(t)
	proposal := e.propose(t, session, e.usd("1.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`)
	e.uploaded(t, "billing", "reference,value_date,amount,currency,liquidity_item\nINV-1,2026-04-21,1.00,USD,REC_CUSTOMERS\nINV-2,2026-04-21,2.00,USD,REC_CUSTOMERS\n")
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }, Pages: pages})
	uid := func(m map[string]any) uuid.UUID { return uuid.MustParse(id(m)) }
	anyVersion := func(int32) error { return nil }
	day := time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	in := cash.MemoInput{LegalEntityID: e.le, HouseBankID: &e.positionEnv.usd, Amount: "5.00", Currency: "USD", ValueDate: day, Item: "REC_CUSTOMERS"}
	override := in
	override.RecurrenceOf, override.OccurrenceDate = new(uid(series)), &day
	for name, call := range map[string]func(ctx context.Context) error{
		"create":   func(ctx context.Context) error { _, err := endpoints.CreateMemo(ctx, in); return err },
		"override": func(ctx context.Context) error { _, err := endpoints.CreateMemo(ctx, override); return err },
		"propose": func(ctx context.Context) error {
			_, err := endpoints.ProposeMemo(ctx, in, uuid.MustParse(session), "x", []cash.LineRef{{StatementID: st, LineNo: 1}})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := endpoints.UpdateMemo(ctx, uid(one), anyVersion, cash.MemoPatch{Amount: new("2.00")})
			return err
		},
		"update a series": func(ctx context.Context) error {
			_, err := endpoints.UpdateMemo(ctx, uid(series), anyVersion, cash.MemoPatch{ValueDate: new(day.AddDate(0, 0, -20))})
			return err
		},
		"realise": func(ctx context.Context) error {
			_, _, err := endpoints.RealiseMemo(ctx, uid(one), anyVersion, cash.Closing{Line: &cash.LineRef{StatementID: st, LineNo: 1}})
			return err
		},
		"cancel an occurrence": func(ctx context.Context) error {
			_, _, err := endpoints.CancelMemo(ctx, uid(series), anyVersion, cash.Closing{Occurrence: &day})
			return err
		},
		"accept": func(ctx context.Context) error {
			_, err := endpoints.AcceptMemo(ctx, uid(proposal), anyVersion, nil)
			return err
		},
		"reject": func(ctx context.Context) error {
			_, err := endpoints.RejectMemo(ctx, uid(proposal), anyVersion, "no")
			return err
		},
		"upload": func(ctx context.Context) error {
			_, err := endpoints.UploadMemos(ctx, e.le, "billing", []byte("reference,value_date,amount,currency,liquidity_item\nINV-1,2026-04-21,3.00,USD,REC_CUSTOMERS\n"+
				"INV-3,2026-04-21,3.00,EUR,REC_CUSTOMERS\n"))
			return err
		},
		"position": func(ctx context.Context) error {
			_, err := endpoints.Position(ctx, cash.Query{LegalEntity: e.le})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := endpoints.GetMemoRecord(ctx, gen.GetMemoRecordParams{ID: uid(one)})
			return err
		},
		"create handler": func(ctx context.Context) error {
			_, err := endpoints.CreateMemoRecord(ctx, &gen.MemoRecordCreate{LegalEntityID: e.le, HouseBankID: gen.NewOptUUID(e.positionEnv.usd),
				Amount: gen.Money{Amount: "5.00", Currency: "USD"}, ValueDate: day, LiquidityItem: "REC_CUSTOMERS"}, gen.CreateMemoRecordParams{})
			return err
		},
		"update handler": func(ctx context.Context) error {
			_, err := endpoints.UpdateMemoRecord(ctx, &gen.MemoRecordUpdate{Certainty: gen.NewOptString("confirmed")}, gen.UpdateMemoRecordParams{ID: uid(one), IfMatch: "*"})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := endpoints.ListMemoRecords(ctx, gen.ListMemoRecordsParams{OrderBy: gen.NewOptString("created_at")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); !errors.Is(err, reqctx.ErrOutsideRequest) {
				t.Errorf("outside a unit of work: got %v, want reqctx.ErrOutsideRequest", err)
			}
			as := e.Tenant.Human
			if name == "propose" {
				as = e.Tenant.Agent
			}
			apitest.EveryFailure(t, e.Tenant, as, name, call)
		})
	}
}

// TestMemoConstraintProblems: the database's memo record rules a request
// can only meet in a race (a configuration deactivating the item, another
// upload of the key) are the problems the application gives first.
func TestMemoConstraintProblems(t *testing.T) {
	for constraint, code := range map[string]string{
		"memo_record_item_active": "cash.liquidity_item_invalid", "memo_record_item_direction": "cash.liquidity_item_invalid",
		"memo_record_batch_reference": "cash.memo_upload_concurrent", "memo_record_override_once": "cash.occurrence_overridden",
	} {
		t.Run(constraint, func(t *testing.T) {
			if got := problem.From(&pgconn.PgError{Code: "23514", ConstraintName: constraint}).Code; got != code {
				t.Errorf("problem: got %s, want %s", got, code)
			}
		})
	}
}
