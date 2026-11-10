package taxfiling_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/countrypack"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

var returnPerms = []string{perm, "tax.return.read", "ledger.entry.post", "ledger.entry.read", "ledger.account.read"}

// test is company 2000's quarterly registration filing TEST-1, the test
// pack's return (testdata/packs/de/2026.1, not Germany's); undefined one
// filing a return the pack does not define.
func test() config.TaxRegistration {
	return config.TaxRegistration{Code: "TEST", Authority: "TAX", Number: "DE000000001", ReturnType: "TEST-1", Frequency: config.TaxQuarterly,
		FirstPeriodStart: "2026-01-01"}
}

// returnsEnv is a tenant whose company 2000 adopted the test pack, with an
// output code S5 (5% on 240000) and the registrations test and UNDEF; company
// 1000 has gst, without a pack, and vat, a group with 2000 in another
// currency. The API carries the test packs alone.
func returnsEnv(t *testing.T, perms ...string) fixture.Env {
	t.Helper()
	e := fixture.NewWith(t, nil, func(doc *config.Config) {
		registered(gst(), vat())(doc)
		de := doc.Company("2000")
		de.ChartOfAccounts = append(de.ChartOfAccounts, config.Account{Number: "240000", Name: "Output tax", Type: config.Liability})
		de.TaxCodes = append(de.TaxCodes, config.TaxCode{Code: "S5", Description: "Output tax 5%", Direction: config.TaxOutput, Kind: config.TaxStandard,
			Category: "S", Components: []config.TaxComponent{{Key: "vat", AccountKey: config.KeyOutput, Account: "240000",
				Rates: []config.TaxRate{{RatePercent: "5", ValidFrom: "2026-01-01"}}}}})
		undefined := test()
		undefined.Code, undefined.Authority, undefined.Number, undefined.ReturnType = "UNDEF", "TAX2", "DE000000002", "TEST-9"
		de.TaxRegistrations = []config.TaxRegistration{test(), undefined}
		doc.Packs = append(doc.Packs, config.PackAdoption{Company: "2000", Country: "DE", Version: "2026.1", AppliedOn: "2026-01-01"})
	}, slices.Concat(returnPerms, perms)...)
	e.API = apitest.NewWith(t, func(c *app.Config) { c.Packs = apitest.Packs(t, "testdata/packs") })
	return e
}

// post posts a journal entry of company 2000 in EUR on the day, its lines
// "account amount [code]", with the tax date when given.
func post(t *testing.T, e fixture.Env, day, taxDate string, lines ...string) string {
	t.Helper()
	je, _ := e.Must(201, "POST", "/v1/journal-entries", entryBody(t, e, day, taxDate, lines...), "")
	return apitest.String(t, je, "id")
}

// entryBody is post's journal entry.
func entryBody(t *testing.T, e fixture.Env, day, taxDate string, lines ...string) string {
	t.Helper()
	out := make([]string, len(lines))
	for i, l := range lines {
		f := strings.Fields(l)
		code := ""
		if len(f) == 3 {
			code = fmt.Sprintf(`, "tax_code": %q`, f[2])
		}
		out[i] = fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}%s}`, e.Account(t, "2000", f[0]), f[1], code)
	}
	td := ""
	if taxDate != "" {
		td = fmt.Sprintf(`"tax_date": %q, `, taxDate)
	}
	return fmt.Sprintf(`{"legal_entity_id": %q, "document_date": %q, "posting_date": %q, %s"currency": "EUR", "lines": [%s]}`,
		e.DE, day, day, td, strings.Join(out, ", "))
}

// sale posts a sale of 100.00 with 5.00 of S5; purchase a purchase of
// 100.00 with 19.00 of V1.
func sale(t *testing.T, e fixture.Env, day, taxDate string) string {
	t.Helper()
	return post(t, e, day, taxDate, "100000 105.00", "400000 -100.00 S5", "240000 -5.00 S5")
}

func purchase(t *testing.T, e fixture.Env, day string) string {
	t.Helper()
	return post(t, e, day, "", "520000 100.00 V1", "140000 19.00 V1", "100000 -119.00")
}

// boxes renders a draft's boxes as code=amount/prior-period part[#items].
func boxes(m map[string]any) string {
	var out []string
	for _, it := range m["boxes"].([]any) {
		b := it.(map[string]any)
		s := fmt.Sprintf("%s=%s/%s", b["code"], b["amount"].(map[string]any)["amount"], b["prior_period_amount"].(map[string]any)["amount"])
		if n, ok := b["item_count"]; ok {
			s += fmt.Sprintf("#%v", n)
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func compute(id, start string) string {
	return "/v1/tax-registrations/" + id + "/draft-return?period_start=" + start
}

// TestGetTaxReturnDraft: a draft return sums the unreported tax items of
// the registration's companies into the pack's boxes (ADR-0049 §11, T61h):
// selection boxes by measure, negated where credits show positive, the
// expression box from them; a later period's draft carries the earlier
// period's unreported items apart as prior-period items, their net
// shortfall of payable tax held to the pack's threshold; computing again
// gives the same draft and claims no item; a reported item is left out.
func TestGetTaxReturnDraft(t *testing.T) {
	e := returnsEnv(t)
	sale(t, e, "2026-02-10", "")
	q1Purchase := purchase(t, e, "2026-03-10")
	sale(t, e, "2026-04-15", "")
	sale(t, e, "2026-07-02", "2026-06-30") // posted late, dated in Q2
	testID := id(t, e, "TEST")

	q1, _ := e.Must(200, "GET", compute(testID, "2026-01-01"), "", "")
	want(t, "Q1's boxes", boxes(q1), "sales=100.00/0.00#1 out=5.00/0.00#1 in=19.00/0.00#1 cost=0.00/0.00#1 net=-14.00/0.00")
	want(t, "Q1", fmt.Sprintf("%v %v %v %v %v %v %v", q1["registration_code"], q1["return_type"], q1["return_name"], q1["period_end"], q1["currency"],
		q1["item_count"], q1["pack"]), "TEST TEST-1 Test return 2026-03-31 EUR 2 map[country:DE version:2026.1]")
	want(t, "Q1's prior period", fmt.Sprint(q1["prior_period"]),
		"map[above_threshold:false item_count:0 tax:map[amount:0.00 currency:EUR] threshold:map[amount:10.00 currency:EUR]]")

	q2, _ := e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
	want(t, "Q2's boxes", boxes(q2), "sales=300.00/100.00#3 out=15.00/5.00#3 in=19.00/19.00#1 cost=0.00/0.00#1 net=-4.00/-14.00")
	want(t, "Q2's prior period", fmt.Sprint(q2["prior_period"]),
		"map[above_threshold:false item_count:2 tax:map[amount:14.00 currency:EUR] threshold:map[amount:10.00 currency:EUR]]")
	again, _ := e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
	want(t, "computed again", fmt.Sprint(again), fmt.Sprint(q2))
	var claimed int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.tax_item WHERE tenant_id = $1 AND reported_in IS NOT NULL`, []any{e.Tenant.ID}, &claimed)
	want(t, "items claimed by a draft", claimed, 0)

	// Reported in a return of another registration, not this one's: its
	// check that a return claims it is off (T61i's prepare claims them).
	pgtest.Exec(t, fmt.Sprintf(`DO $$ BEGIN SET LOCAL session_replication_role = replica;
		UPDATE ledger.tax_item SET reported_in = gen_random_uuid() WHERE journal_entry_id = '%s'; END $$`, q1Purchase))
	q2, _ = e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
	want(t, "Q2 without the reported purchase", boxes(q2), "sales=300.00/100.00#3 out=15.00/5.00#3 in=0.00/0.00#0 cost=0.00/0.00#0 net=15.00/5.00")
	want(t, "a shortfall of 5.00", fmt.Sprint(q2["prior_period"].(map[string]any)["above_threshold"]), "false")

	// Late sales of the first quarter: a shortfall of exactly the
	// threshold, then above it (po:2026-11-09, C11).
	sale(t, e, "2026-07-03", "2026-03-31")
	q2, _ = e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
	want(t, "a shortfall of 10.00", fmt.Sprint(q2["prior_period"].(map[string]any)["above_threshold"]), "false")
	sale(t, e, "2026-07-03", "2026-03-31")
	q2, _ = e.Must(200, "GET", compute(testID, "2026-04-01"), "", "")
	want(t, "a shortfall of 15.00", fmt.Sprint(q2["prior_period"].(map[string]any)["above_threshold"]), "true")
}

// TestComputeConcurrently: two drafts computed at once are the same and
// claim no item, so neither can take an item from the other: a draft
// writes nothing (ADR-0049 §11; T61i's prepare sets reported_in under a
// lock on the registration).
func TestComputeConcurrently(t *testing.T) {
	e := returnsEnv(t)
	sale(t, e, "2026-02-10", "")
	purchase(t, e, "2026-03-10")
	path := compute(id(t, e, "TEST"), "2026-01-01")
	drafts := make([]string, 2)
	var wg sync.WaitGroup
	for i := range drafts {
		wg.Go(func() {
			code, body, _ := e.Do("GET", path, "", "")
			b, _ := json.Marshal(body) // a decoded JSON body always encodes
			drafts[i] = fmt.Sprintf("%d %s", code, b)
		})
	}
	wg.Wait()
	if !strings.HasPrefix(drafts[0], "200 ") {
		t.Fatalf("draft: got %s, want 200", drafts[0])
	}
	want(t, "the second draft", drafts[1], drafts[0])
	var claimed int
	pgtest.Scan(t, `SELECT count(*) FROM ledger.tax_item WHERE tenant_id = $1 AND reported_in IS NOT NULL`, []any{e.Tenant.ID}, &claimed)
	want(t, "items claimed", claimed, 0)
}

// TestTaxReturnItems: a selection box drills down to its items, each with
// what it adds to the box, paged in tax date order; the amounts add up to
// the box's.
func TestTaxReturnItems(t *testing.T) {
	e := returnsEnv(t)
	sale(t, e, "2026-02-10", "")
	sale(t, e, "2026-04-15", "")
	sale(t, e, "2026-07-02", "2026-06-30")
	base := "/v1/tax-registrations/" + id(t, e, "TEST") + "/draft-return/items?period_start=2026-04-01&box=out"
	first, _ := e.Must(200, "GET", base+"&page_size=2", "", "")
	next, _ := e.Must(200, "GET", base+"&page_size=2&page_token="+first["next_page_token"].(string), "", "")
	if _, more := next["next_page_token"]; more {
		t.Error("last page: got a next page token, want none")
	}
	var got []string
	for _, page := range []map[string]any{first, next} {
		for _, it := range page["items"].([]any) {
			i := it.(map[string]any)
			if i["document_number"] == "" || i["posting_date"] == nil {
				t.Errorf("item %v: got no document number or posting date", i)
			}
			got = append(got, fmt.Sprintf("%s %s %s/%s %s %s->%s prior:%v", i["tax_date"], i["company_code"], i["tax_code"], i["component"], i["direction"],
				i["tax"].(map[string]any)["amount"], i["box_amount"].(map[string]any)["amount"], i["prior_period"]))
		}
	}
	want(t, "the box's items", strings.Join(got, "; "), "2026-02-10 2000 S5/vat output -5.00->5.00 prior:true; "+
		"2026-04-15 2000 S5/vat output -5.00->5.00 prior:false; 2026-06-30 2000 S5/vat output -5.00->5.00 prior:false")
	want(t, "the page", fmt.Sprintf("%v %v", first["box"], first["currency"]), "out EUR")
	for name, box := range map[string]string{"an expression box": "net", "no such box": "vat"} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(422, "tax.not_a_selection_box", "GET", strings.Replace(base, "box=out", "box="+box, 1), "", "")
		})
	}
	e.Problem(400, "request.invalid_page_token", "GET", base+"&page_token=x", "", "")
}

// TestComputeRefusals: a draft of a period that is not the registration's,
// or has not ended, of a return the adopted pack does not define (or no
// pack adopted), across currencies, or of a registration covering a
// company the caller may not act in, is refused with a problem; one the
// caller does not see is not found.
func TestComputeRefusals(t *testing.T) {
	e := returnsEnv(t)
	testID, gstID, vatID := id(t, e, "TEST"), id(t, e, "GST"), id(t, e, "VAT")
	for name, c := range map[string]struct {
		path   string
		status int
		code   string
	}{
		"not a period's start": {compute(testID, "2026-02-01"), 422, "tax.not_a_period"},
		"before the first":     {compute(testID, "2025-10-01"), 422, "tax.not_a_period"},
		"not ended":            {compute(testID, "2099-01-01"), 409, "tax.period_not_ended"},
		"a return undefined":   {compute(id(t, e, "UNDEF"), "2026-01-01"), 409, "tax.return_undefined"},
		"no pack adopted":      {compute(gstID, "2026-01-01"), 409, "tax.return_undefined"},
		"two currencies":       {compute(vatID, "2026-01-15"), 409, "tax.return_currency_mixed"},
		"no registration":      {compute(uuid.NewString(), "2026-01-01"), 404, "request.not_found"},
		"its items too": {"/v1/tax-registrations/" + testID + "/draft-return/items?period_start=2099-01-01&box=out",
			409, "tax.period_not_ended"},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "GET", c.path, "", "")
		})
	}
	de := e.As(e.Reader(t, "Germany only", e.DE))
	de.Problem(403, "tax.registration_companies", "GET", compute(vatID, "2026-01-15"), "", "")
	de.Must(200, "GET", compute(testID, "2026-01-01"), "", "")
}

// overflowing is a ledger port whose items' sums overflow an amount.
type overflowing struct {
	ledger.TaxItems // claims and releases as the ledger does, unless claim is set
	groups          []ledger.TaxItemGroup
	items           []ledger.ReturnItem
	claim           bool // claims claimed, grouped as groups
	claimed         []ledger.ClaimedItem
}

func (o overflowing) Claim(ctx context.Context, les []uuid.UUID, from *time.Time, start, end time.Time, sels []ledger.TaxItemSelection,
	ret uuid.UUID) ([]ledger.TaxItemGroup, []ledger.ClaimedItem, error) {
	if o.claim {
		return o.groups, o.claimed, nil
	}
	return o.TaxItems.Claim(ctx, les, from, start, end, sels, ret)
}

func (o overflowing) Unreported(context.Context, []uuid.UUID, time.Time, time.Time) ([]ledger.TaxItemGroup, error) {
	return o.groups, nil
}

func (o overflowing) UnreportedItems(context.Context, []uuid.UUID, time.Time, ledger.TaxItemSelection, *ledger.TaxItemKey, int) ([]ledger.ReturnItem, error) {
	return o.items, nil
}

// TestReturnOverflow: sums beyond an amount fail the draft rather than
// wrap, in a box, the prior-period tax, an expression and an item's
// negation.
func TestReturnOverflow(t *testing.T) {
	e := returnsEnv(t)
	testID := uuid.MustParse(id(t, e, "TEST"))
	lib, err := countrypack.LoadAll(apitest.Packs(t, "testdata/packs"))
	if err != nil {
		t.Fatal(err)
	}
	h := endpoints(t, time.Now())
	h.Packs = lib
	big := money.Minor(math.MaxInt64)
	out := ledger.TaxItemGroup{Direction: "output", AccountKey: "output", Items: 1, BaseLC: -1, TaxLC: -big}
	in := ledger.TaxItemGroup{Direction: "input", AccountKey: "input", Items: 1, TaxLC: -big, RecoverableLC: -big}
	// Recovered in full, though non-deductible: no box's sum overflows,
	// the prior-period tax does.
	nonDeductible := ledger.TaxItemGroup{Direction: "input", AccountKey: "non_deductible", PriorPeriod: true, PriorTaxDate: day("2025-12-31"), TaxLC: big, RecoverableLC: big}
	for name, c := range map[string]overflowing{
		"a box":            {groups: []ledger.TaxItemGroup{out, out}},
		"prior-period tax": {groups: []ledger.TaxItemGroup{nonDeductible, nonDeductible}},
		// The tax sums to big, what is posted to 2·big: the second group's
		// tax is not recovered.
		"one period's posted tax": {groups: []ledger.TaxItemGroup{nonDeductible,
			{Direction: "input", AccountKey: "non_deductible", PriorPeriod: true, PriorTaxDate: day("2025-12-31"), TaxLC: -big}, nonDeductible}},
		"a box's prior":  {groups: []ledger.TaxItemGroup{{Direction: "input", AccountKey: "input", PriorPeriod: true, PriorTaxDate: day("2025-12-31"), RecoverableLC: big}, {Direction: "input", AccountKey: "input", PriorPeriod: true, PriorTaxDate: day("2025-12-31"), RecoverableLC: 1}}},
		"the expression": {groups: []ledger.TaxItemGroup{out, in}},
		"a negation":     {groups: []ledger.TaxItemGroup{{Direction: "output", AccountKey: "output", TaxLC: math.MinInt64}}},
	} {
		t.Run(name, func(t *testing.T) {
			h.Items = c
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.GetTaxReturnDraft(ctx, gen.GetTaxReturnDraftParams{ID: testID, PeriodStart: date("2026-01-01")})
				return err
			})
			if err == nil || problem.From(err).Status != 500 {
				t.Errorf("got %v, want an internal error", err)
			}
		})
	}
	// The threshold is 10.00 (1000 minor units). It is raised by a net
	// shortfall of payable tax above it in one earlier period: output tax
	// (a credit, negative) less recovered input tax (po:2026-11-09, C11).
	recoveredInPart := ledger.TaxItemGroup{Direction: "input", AccountKey: "input", PriorPeriod: true, PriorTaxDate: day("2026-02-10"), Items: 1,
		TaxLC: 1000, RecoverableLC: 400}
	for name, c := range map[string]struct {
		groups []ledger.TaxItemGroup
		want   bool
	}{
		"a shortfall above it":                 {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -1200)}, true},
		"an overpayment above it":              {[]ledger.TaxItemGroup{prior("input", "2026-02-10", 1200)}, false},
		"output tax reduced above it":          {[]ledger.TaxItemGroup{prior("output", "2026-02-10", 1200)}, false},
		"a shortfall of exactly it":            {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -1000)}, false},
		"a shortfall one minor unit above it":  {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -1001)}, true},
		"directions net within a period":       {[]ledger.TaxItemGroup{prior("output", "2026-01-10", -1500), prior("input", "2026-03-10", 600)}, false},
		"a period's items add up":              {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -600), prior("output", "2026-03-10", -600)}, true},
		"periods are not pooled":               {[]ledger.TaxItemGroup{prior("output", "2025-08-10", -600), prior("output", "2026-02-10", -600)}, false},
		"an overpayment offsets no period":     {[]ledger.TaxItemGroup{prior("output", "2025-08-10", -1200), prior("input", "2026-02-10", 5000)}, true},
		"before the first period, one bucket":  {[]ledger.TaxItemGroup{prior("output", "2025-11-10", -600), prior("output", "2025-12-10", -600)}, true},
		"the recovered part of input tax only": {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -1500), recoveredInPart}, true},
		"a shortfall net of recovered input":   {[]ledger.TaxItemGroup{prior("output", "2026-02-10", -1300), recoveredInPart}, false},
	} {
		t.Run(name, func(t *testing.T) {
			h.Items = overflowing{groups: c.groups}
			var draft *gen.TaxReturnDraft
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				res, err := h.GetTaxReturnDraft(ctx, gen.GetTaxReturnDraftParams{ID: testID, PeriodStart: date("2026-04-01")})
				draft, _ = res.(*gen.TaxReturnDraft)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want(t, "above the threshold", draft.PriorPeriod.AboveThreshold, c.want)
		})
	}
	t.Run("an item no box selects", func(t *testing.T) {
		// The test pack's input boxes take the input and non-deductible
		// account keys alone.
		h.Items = overflowing{groups: []ledger.TaxItemGroup{{Direction: "input", AccountKey: "self_input", Kind: "reverse_charge", Items: 1, TaxLC: 5,
			RecoverableLC: 5}, {Direction: "input", AccountKey: "input", PriorPeriod: true, PriorTaxDate: day("2025-12-31"), Items: 1, TaxLC: 3, RecoverableLC: 3}}}
		var draft *gen.TaxReturnDraft
		if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			res, err := h.GetTaxReturnDraft(ctx, gen.GetTaxReturnDraftParams{ID: testID, PeriodStart: date("2026-01-01")})
			draft, _ = res.(*gen.TaxReturnDraft)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		want(t, "items", fmt.Sprintf("%d %d %s", draft.ItemCount, draft.PriorPeriod.ItemCount, draft.PriorPeriod.Tax.Amount), "1 1 0.03")
	})
	t.Run("an item's negation", func(t *testing.T) {
		h.Items = overflowing{items: []ledger.ReturnItem{{TaxItem: ledger.TaxItem{AccountKey: "output", TaxLC: math.MinInt64}}}}
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListTaxReturnDraftItems(ctx, gen.ListTaxReturnDraftItemsParams{ID: testID, PeriodStart: date("2026-01-01"), Box: "out"})
			return err
		})
		if err == nil {
			t.Error("got nil, want an error")
		}
	})
	t.Run("a page size", func(t *testing.T) {
		err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.ListTaxReturnDraftItems(ctx, gen.ListTaxReturnDraftItemsParams{ID: testID, PeriodStart: date("2026-01-01"), Box: "out",
				PageSize: gen.NewOptInt32(0)})
			return err
		})
		want(t, "code", problem.From(err).Code, "request.invalid_page_size")
	})
}

// day is a pointer to the date.
func day(s string) *time.Time {
	d := date(s)
	return &d
}

// prior is a prior-period group of the direction's tax items of the day,
// their tax all recovered.
func prior(direction, on string, tax money.Minor) ledger.TaxItemGroup {
	key := direction
	if direction == "output" {
		return ledger.TaxItemGroup{Direction: direction, AccountKey: key, PriorPeriod: true, PriorTaxDate: day(on), Items: 1, TaxLC: tax}
	}
	return ledger.TaxItemGroup{Direction: direction, AccountKey: key, PriorPeriod: true, PriorTaxDate: day(on), Items: 1, TaxLC: tax, RecoverableLC: tax}
}

func date(s string) time.Time {
	d, _ := time.Parse(time.DateOnly, s) // the tests' dates are valid
	return d
}

// missing is the directory without one legal entity.
type missing struct {
	tenancy.Directory
	id uuid.UUID
}

func (m missing) Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error) {
	if id == m.id {
		return tenancy.Entity{}, false, nil
	}
	return m.Directory.Entity(ctx, id)
}

// unzoned is the active configuration with company 2000 in a time zone
// the binary does not know.
type unzoned struct{ configuration.Settings }

func (u unzoned) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, v, err := u.Settings.Document(ctx)
	if err == nil {
		doc.Company("2000").TimeZone = "Nowhere/Atlantis"
	}
	return doc, v, err
}

// TestReturnInconsistencies: a company the directory no longer has, or a
// time zone that cannot be loaded, fails the draft rather than guessing.
func TestReturnInconsistencies(t *testing.T) {
	e := returnsEnv(t)
	testID := uuid.MustParse(id(t, e, "TEST"))
	for name, h := range map[string]*taxfiling.Endpoints{
		"a company": func() *taxfiling.Endpoints {
			h := endpoints(t, time.Now())
			h.LegalEntities = missing{id: e.DE}
			return h
		}(),
		"a time zone": func() *taxfiling.Endpoints { h := endpoints(t, time.Now()); h.Settings = unzoned{}; return h }(),
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				_, err := h.GetTaxReturnDraft(ctx, gen.GetTaxReturnDraftParams{ID: testID, PeriodStart: date("2026-01-01")})
				return err
			})
			if err == nil || problem.From(err).Status != 500 {
				t.Errorf("got %v, want an internal error", err)
			}
		})
	}
}
