package cash_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// memoPerms are what the memo record tests' people and agent hold.
var memoPerms = []string{"cash.position.read", "cash.memo.read", "cash.memo.write", "cash.memo.propose", "agents.session.record", "agents.session.read"}

// withMemos is Maple Inc's position document (withPosition) with the
// liquidity items of withCash: OPERATING (both), OP_SUPPLIERS and TAX
// (outflows), REC_CUSTOMERS (inflows).
func withMemos(doc *config.Config) {
	withPosition(doc)
	setCash(doc)
}

// memoEnv is Maple Inc with its USD and CAD (MAIN) house banks and the
// liquidity items, read on Friday 17 April 2026.
type memoEnv struct{ positionEnv }

func setupMemo(t *testing.T) memoEnv {
	t.Helper()
	return memoEnv{setupPositionWith(t, withMemos, memoPerms...)}
}

// body is a memo record's JSON for Maple Inc with the fields given.
func (e memoEnv) body(fields string) string {
	return `{"legal_entity_id": "` + e.le.String() + `", ` + fields + `}`
}

// usd is a body's house bank and amount in USD.
func (e memoEnv) usd(amount string) string {
	return `"house_bank_id": "` + e.positionEnv.usd.String() + `", "amount": {"amount": "` + amount + `", "currency": "USD"}`
}

// create creates a memo record and returns it with its ETag.
func (e memoEnv) create(t *testing.T, fields string) (map[string]any, string) {
	t.Helper()
	m, tag := e.Must(201, "POST", "/v1/memo-records", e.body(fields), "")
	wantBankCode(t, m)
	return m, tag
}

// wantBankCode: each memo record names its house bank by the code the
// payments context holds, exactly when it names the bank.
func wantBankCode(t *testing.T, records ...map[string]any) {
	t.Helper()
	for _, m := range records {
		apitest.WantNamed(t, "memo record", m, "house_bank_id", "house_bank_code", apitest.HouseBankCode)
	}
}

func id(m map[string]any) string { return fmt.Sprint(m["id"]) }

func path(m map[string]any, verb string) string {
	if verb == "" {
		return "/v1/memo-records/" + id(m)
	}
	return "/v1/memo-records/" + id(m) + ":" + verb
}

// TestCreateMemoRecord: a person's one-off expected receipt on a house
// bank, active, expiring seven days after its value date, audited and
// published; and one for the forecast only (ADR-0051 §7).
func TestCreateMemoRecord(t *testing.T) {
	e := setupMemo(t)
	m, tag := e.create(t, e.usd("1000.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "description": "Northwind", "reference": "INV-1"`)
	want(t, "record", fmt.Sprint(m["status"], " ", m["origin"], " ", m["direction"], " ", amount(m["amount"]), " ", m["value_date"], " ", m["expires_on"], " ",
		m["certainty"], " ", m["liquidity_item"], " ", m["description"], " ", m["reference"], " ", m["house_bank_id"]),
		"active manual inflow 1000.00 USD 2026-04-21 2026-04-28 expected REC_CUSTOMERS Northwind INV-1 "+e.positionEnv.usd.String())
	want(t, "ETag", tag, `"1"`)
	want(t, "its house bank's code", m["house_bank_code"], "USD")
	got, tag := e.Must(200, "GET", path(m, ""), "", "")
	want(t, "read back", fmt.Sprint(got["id"], " ", tag, " ", got["house_bank_code"]), fmt.Sprint(m["id"], ` "1" USD`))

	f, _ := e.create(t, `"amount": {"amount": "-500.00", "currency": "CAD"}, "value_date": "2026-04-30", "expires_on": "2026-05-15", "certainty": "planned", "liquidity_item": "TAX"`)
	want(t, "forecast only", fmt.Sprint(f["house_bank_id"], " ", f["house_bank_code"], " ", f["direction"], " ", f["expires_on"], " ", f["certainty"]),
		"<nil> <nil> outflow 2026-05-15 planned")

	evs := apitest.Published(t, e.Tenant, outbox.Event{Context: "cash", Name: "memo_record_changed"})
	want(t, "events", len(evs), 2)
	want(t, "event", fmt.Sprint(evs[0]["memo_record_id"], " ", evs[0]["change"], " ", evs[0]["status"], " ", evs[0]["version"]), fmt.Sprint(m["id"], " created active 1"))
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'cash.memo_record.create'`, []any{e.Tenant.ID}, &audited)
	want(t, "audited", audited, 2)
}

// TestCreateMemoRecordRefusals: what a memo record must be (ADR-0051 §7).
func TestCreateMemoRecordRefusals(t *testing.T) {
	e := setupMemo(t)
	main := `"house_bank_id": "` + e.main.String() + `"`
	for name, c := range map[string]struct {
		status      int
		code, field string
	}{
		`"amount": {"amount": "1.00", "currency": "XXZ"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`: {422, "cash.unknown_currency", "amount.currency"},
		`"amount": {"amount": "1.0", "currency": "USD"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`:  {400, "request.invalid_amount", "amount.amount"},
		`"amount": {"amount": "0.00", "currency": "USD"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`: {422, "cash.invalid_amount", "amount.amount"},
		main + `, "amount": {"amount": "1.00", "currency": "USD"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`: {422, "cash.house_bank_invalid",
			"house_bank_id"},
		`"house_bank_id": "` + uuid.NewString() + `", "amount": {"amount": "1.00", "currency": "USD"}, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`: {
			422, "cash.house_bank_invalid", "house_bank_id"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "UNKNOWN"`:                                   {422, "cash.liquidity_item_invalid", "liquidity_item"},
		e.usd("-1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`:                            {422, "cash.liquidity_item_invalid", "liquidity_item"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "TAX"`:                                       {422, "cash.liquidity_item_invalid", "liquidity_item"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "certainty": "actual"`:      {422, "request.validation_failed", "certainty"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "expires_on": "2026-04-20"`: {422, "request.validation_failed", "expires_on"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "daily"}`: {422, "request.validation_failed",
			"recurrence.frequency"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly", "until": "2026-06-01", "count": 3}`: {
			422, "request.validation_failed", "recurrence.until"},
		e.usd("1.00") + `, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS", "recurrence": {"frequency": "weekly", "until": "2026-04-01"}`: {
			422, "request.validation_failed", "recurrence.until"},
	} {
		t.Run(name, func(t *testing.T) {
			p := e.In(t).Problem(c.status, c.code, "POST", "/v1/memo-records", e.body(name), "")
			want(t, "field", fieldOf(p), c.field)
		})
	}
	t.Run("a legal entity the caller cannot act in", func(t *testing.T) {
		other := e.Reader(t, "reader of 1000", e.US)
		e.As(other).In(t).Problem(404, "request.not_found", "POST", "/v1/memo-records", e.body(e.usd("1.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`), "")
	})
	t.Run("an agent", func(t *testing.T) {
		e.Tenant.AllCompanies(t, e.Tenant.Agent)
		e.As(e.Tenant.Agent).In(t).Problem(403, "identity.agent_not_allowed", "POST", "/v1/memo-records",
			e.body(e.usd("1.00")+`, "value_date": "2026-04-21", "liquidity_item": "REC_CUSTOMERS"`), "")
	})
}

// fieldOf is a problem's first field error's field.
func fieldOf(p map[string]any) string {
	errs, _ := p["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	return fmt.Sprint(errs[0].(map[string]any)["field"])
}

// fields are all of a problem's field errors' fields.
func fields(p map[string]any) string {
	errs, _ := p["errors"].([]any)
	out := make([]string, len(errs))
	for i, fe := range errs {
		out[i] = fmt.Sprint(fe.(map[string]any)["field"])
	}
	return strings.Join(out, " ")
}
