package cash_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/cash"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
)

// zoneCase is a memo record test's company zone and instant: Friday 17
// April in the zone, another day in UTC.
type zoneCase struct {
	zone string
	at   time.Time
	// lastDay is the record's last day: Maple's record is active on the
	// 17th when it is the 17th, expired when it is the 16th, and UTC's
	// date (Acme US has no zone) judges the other way.
	lastDay, maple, acme string
}

var zoneCases = []zoneCase{
	{"America/Toronto", time.Date(2026, 4, 18, 2, 0, 0, 0, time.UTC), "2026-04-17", "active", "expired"}, // 22:00 EDT on the 17th
	{"Asia/Dubai", time.Date(2026, 4, 16, 21, 0, 0, 0, time.UTC), "2026-04-16", "expired", "active"},     // 01:00 GST on the 17th
}

// setupMemoIn is setupMemo with Maple in zone and the API's clock at at.
func setupMemoIn(t *testing.T, zone string, at time.Time) memoEnv {
	t.Helper()
	e := memoEnv{setupPositionWith(t, func(doc *config.Config) {
		withMemos(doc)
		doc.Company("3000").TimeZone = zone
	}, memoPerms...)}
	e.API = apitest.NewWith(t, func(cfg *app.Config) { cfg.Now = func() time.Time { return at } })
	return e
}

// statuses lists memo records with the query, as "id status read_date
// time_zone" by id.
func (e memoEnv) statuses(t *testing.T, query string) map[string]string {
	t.Helper()
	p, _ := e.Must(200, "GET", "/v1/memo-records?page_size=100"+query, "", "")
	out := map[string]string{}
	for _, it := range p["items"].([]any) {
		m := it.(map[string]any)
		out[id(m)] = fmt.Sprint(m["status"], " ", m["read_date"], " ", m["time_zone"])
	}
	return out
}

// TestMemoRecordStatusInTheCompanysZone: a memo record's status is judged
// on its legal entity's date in its company's time zone (T72n2), shown as
// read_date and time_zone, in the create, read and list responses and the
// list's status filter alike, and the cash position counts it exactly
// when the list shows it active. At 22:00 on 17 April in Toronto a record
// whose last day is the 17th is still active, though UTC is on the 18th;
// at 01:00 on the 17th in Dubai one whose last day was the 16th has
// expired, though UTC is still on the 16th. Acme US, without a zone, is
// judged by UTC's date in the same list.
func TestMemoRecordStatusInTheCompanysZone(t *testing.T) {
	for _, c := range zoneCases {
		t.Run(c.zone, func(t *testing.T) {
			e := setupMemoIn(t, c.zone, c.at)
			utc := c.at.UTC().Format(time.DateOnly)
			fields := `"value_date": "` + c.lastDay + `", "expires_on": "` + c.lastDay + `", "liquidity_item": "REC_CUSTOMERS"`
			maple, _ := e.create(t, e.usd("70.00")+", "+fields)
			acme, _ := e.Must(201, "POST", "/v1/memo-records", `{"legal_entity_id": "`+e.US.String()+`", "amount": {"amount": "5.00", "currency": "USD"}, `+fields+`}`, "")
			mapleWant, acmeWant := c.maple+" 2026-04-17 "+c.zone, c.acme+" "+utc+" UTC"
			want(t, "created", fmt.Sprint(maple["status"], " ", maple["read_date"], " ", maple["time_zone"]), mapleWant)
			got, _ := e.Must(200, "GET", path(maple, ""), "", "")
			want(t, "read", fmt.Sprint(got["status"], " ", got["read_date"], " ", got["time_zone"]), mapleWant)
			got, _ = e.Must(200, "GET", path(acme, ""), "", "")
			want(t, "Acme's, read", fmt.Sprint(got["status"], " ", got["read_date"], " ", got["time_zone"]), acmeWant)

			all := e.statuses(t, "")
			want(t, "listed", all[id(maple)], mapleWant)
			want(t, "Acme's, listed", all[id(acme)], acmeWant)
			for _, status := range []string{"active", "expired"} {
				t.Run(status, func(t *testing.T) {
					filtered := e.statuses(t, "&status="+status)
					want(t, "Maple's filtered", filtered[id(maple)] != "", status == c.maple)
					want(t, "Acme's filtered", filtered[id(acme)] != "", status == c.acme)
					want(t, "Maple's filtered by its legal entity", e.statuses(t, "&status="+status+"&legal_entity_id="+e.le.String())[id(maple)] != "",
						status == c.maple)
				})
			}

			flows := days(t, bankOf(t, e.positionOf(t, "&days=1"), "USD")["days"])
			want(t, "counted in the position", strings.Contains(fmt.Sprint(flows), "memo/expected/late=false 70.00 USD"), c.maple == "active")
		})
	}
}

// TestMemoRecordsOfAnUnknownLegalEntity: a list filtered by a legal entity
// that does not exist is empty, judged by UTC's date.
func TestMemoRecordsOfAnUnknownLegalEntity(t *testing.T) {
	e := setupMemoIn(t, "America/Toronto", readAt)
	e.create(t, e.usd("70.00")+`, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS"`)
	want(t, "listed", len(e.statuses(t, "&legal_entity_id="+uuid.NewString())), 0)
}

// unprovisioned is the active configuration with a company 4000 in Dubai
// that has no legal entity, as no activated document has (activation
// refuses one), so it has no records.
type unprovisioned struct{ cash.Settings }

func (u unprovisioned) Document(ctx context.Context) (*config.Config, int32, error) {
	doc, n, err := u.Settings.Document(ctx)
	if err == nil {
		gulf := *doc.Company("3000")
		gulf.Code, gulf.Name, gulf.TimeZone = "4000", "Maple Gulf", "Asia/Dubai"
		doc.Companies = append(doc.Companies, gulf)
	}
	return doc, n, err
}

// TestMemoRecordsOfAnUnprovisionedCompany: a zoned company without a
// legal entity is passed over; the others' records are judged as ever.
func TestMemoRecordsOfAnUnprovisionedCompany(t *testing.T) {
	e := setupMemoIn(t, "America/Toronto", zoneCases[0].at)
	m, _ := e.create(t, e.usd("70.00")+`, "value_date": "2026-04-17", "expires_on": "2026-04-17", "liquidity_item": "REC_CUSTOMERS"`)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return zoneCases[0].at }, Pages: codec(t)})
	endpoints.Settings = unprovisioned{endpoints.Settings}
	var res gen.ListMemoRecordsRes
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) (err error) {
		res, err = endpoints.ListMemoRecords(ctx, gen.ListMemoRecordsParams{Status: gen.NewOptString("active")})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	items := res.(*gen.MemoRecordPage).Items
	if len(items) != 1 || items[0].ID.String() != id(m) {
		t.Fatalf("got %v, want Maple's record active", items)
	}
	want(t, "Maple's", fmt.Sprint(items[0].Status, " ", items[0].ReadDate.Format(time.DateOnly), " ", items[0].TimeZone), "active 2026-04-17 America/Toronto")
}

// codec is a page token codec for endpoints called directly.
func codec(t *testing.T) *pagination.Codec {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

// TestMemoRecordZoneLost: a company whose stored zone does not load fails
// a memo record's read and the list rather than judging in UTC.
func TestMemoRecordZoneLost(t *testing.T) {
	e := setupMemoIn(t, "America/Toronto", readAt)
	m, _ := e.create(t, e.usd("70.00")+`, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS"`)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }, Pages: codec(t)})
	endpoints.Settings = zoneLost{endpoints.Settings}
	for name, call := range map[string]func(ctx context.Context) error{
		"read": func(ctx context.Context) error {
			_, err := endpoints.GetMemoRecord(ctx, gen.GetMemoRecordParams{ID: uuid.MustParse(id(m))})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := endpoints.ListMemoRecords(ctx, gen.ListMemoRecordsParams{})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, call)
			if err == nil || !strings.Contains(err.Error(), "America/Nowhere") {
				t.Errorf("got %v, want the company's zone refused", err)
			}
		})
	}
}

// TestMemoZoneFailures: with a company in a zone, a memo record's read and
// the list fail with each of their database statements failing in turn,
// the legal entity found by its company code among them.
func TestMemoZoneFailures(t *testing.T) {
	e := setupMemoIn(t, "America/Toronto", readAt)
	m, _ := e.create(t, e.usd("70.00")+`, "value_date": "2026-04-17", "liquidity_item": "REC_CUSTOMERS"`)
	endpoints := app.Cash(app.Config{Now: func() time.Time { return readAt }, Pages: codec(t)})
	for name, call := range map[string]func(ctx context.Context) error{
		"read": func(ctx context.Context) error {
			_, err := endpoints.GetMemoRecord(ctx, gen.GetMemoRecordParams{ID: uuid.MustParse(id(m))})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := endpoints.ListMemoRecords(ctx, gen.ListMemoRecordsParams{Status: gen.NewOptString("active")})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, name, call) })
	}
}
