package taxfiling_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/taxfiling"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

const perm = "tax.registration.read"

// gst is company 1000's quarterly registration; vat a tax group's, held by
// 1000 for itself and 2000, monthly from the 15th.
func gst() config.TaxRegistration {
	return config.TaxRegistration{Code: "GST", Authority: "CRA", Number: "123456789 RT 0001", ReturnType: "GST34", Frequency: config.TaxQuarterly,
		FirstPeriodStart: "2026-01-01"}
}

func vat() config.TaxRegistration {
	return config.TaxRegistration{Code: "VAT", Authority: "FTA", Number: "100200300045678", ReturnType: "VAT201", Frequency: config.TaxMonthly,
		FirstPeriodStart: "2026-01-15", GroupMembers: []string{"2000"}}
}

// registered sets company 1000's registrations.
func registered(rs ...config.TaxRegistration) func(*config.Config) {
	return func(doc *config.Config) { doc.Companies[0].TaxRegistrations = rs }
}

// env is a tenant bootstrapped with gst and vat.
func env(t *testing.T) fixture.Env {
	t.Helper()
	return fixture.NewWith(t, nil, registered(gst(), vat()), perm)
}

func endpoints(t *testing.T, now time.Time) *taxfiling.Endpoints {
	t.Helper()
	pages, err := pagination.NewCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return &taxfiling.Endpoints{Pages: pages, LegalEntities: tenancy.Directory{}, Settings: configuration.Settings{}, Now: func() time.Time { return now }}
}

func want[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// codes are the registrations' codes of a listing.
func codes(m map[string]any) string {
	var out []string
	for _, it := range m["items"].([]any) {
		out = append(out, it.(map[string]any)["code"].(string))
	}
	return strings.Join(out, " ")
}

// TestListTaxRegistrations: the registrations the configuration set up,
// each company named by its code and the number by its last four
// characters; filtered by a company they cover and by authority, paged by
// code; a person of company 2000 alone sees only the group covering it.
func TestListTaxRegistrations(t *testing.T) {
	e := env(t)
	page, _ := e.Must(200, "GET", "/v1/tax-registrations", "", "")
	want(t, "codes", codes(page), "GST VAT")
	reg := page["items"].([]any)[1].(map[string]any)
	got := fmt.Sprintf("%v %v %v %v %v %v %v %v %v", reg["company_code"], reg["legal_entity_id"], reg["authority"], reg["number_last4"], reg["return_type"],
		reg["frequency"], reg["first_period_start"], reg["status"], reg["group_members"])
	want(t, "the group's registration", got, fmt.Sprintf("1000 %s FTA 5678 VAT201 monthly 2026-01-15 active [map[company_code:2000 legal_entity_id:%s]]", e.US, e.DE))
	_, hasEnd := reg["last_period_end"]
	want(t, "a running registration's end", hasEnd, false)
	want(t, "a registration of one company's members", fmt.Sprint(page["items"].([]any)[0].(map[string]any)["group_members"]), "[]")

	for name, c := range map[string]struct{ query, want string }{
		"by registrant":     {"legal_entity_id=" + e.US.String(), "GST VAT"},
		"by member":         {"legal_entity_id=" + e.DE.String(), "VAT"},
		"by authority":      {"authority=CRA", "GST"},
		"by both":           {"authority=CRA&legal_entity_id=" + e.DE.String(), ""},
		"newest code first": {"order_by=code%20desc", "VAT GST"},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", "/v1/tax-registrations?"+c.query, "", "")
			want(t, "codes", codes(page), c.want)
		})
	}
	first, _ := e.Must(200, "GET", "/v1/tax-registrations?page_size=1", "", "")
	next, _ := e.Must(200, "GET", "/v1/tax-registrations?page_size=1&page_token="+first["next_page_token"].(string), "", "")
	want(t, "pages", codes(first)+" "+codes(next), "GST VAT")
	if _, more := next["next_page_token"]; more {
		t.Error("last page: got a next page token, want none")
	}
	de := e.As(e.Reader(t, "Germany only", e.DE))
	page, _ = de.Must(200, "GET", "/v1/tax-registrations", "", "")
	want(t, "a reader of company 2000", codes(page), "VAT")
	nobody := e.As(e.Reader(t, "No company"))
	page, _ = nobody.Must(200, "GET", "/v1/tax-registrations", "", "")
	want(t, "a reader of no company", codes(page), "")
}

// periods are a listing's periods as "start..end".
func periods(m map[string]any) string {
	var out []string
	for _, it := range m["items"].([]any) {
		p := it.(map[string]any)
		out = append(out, fmt.Sprintf("%s..%s", p["start_date"], p["end_date"]))
	}
	return strings.Join(out, " ")
}

// id is the registration's id.
func id(t *testing.T, e fixture.Env, code string) string {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/tax-registrations?authority=", "", "")
	for _, it := range page["items"].([]any) {
		if r := it.(map[string]any); r["code"] == code {
			return r["id"].(string)
		}
	}
	t.Fatalf("no registration %s", code)
	return ""
}

// TestListTaxPeriods: a registration's periods follow from its first start
// by its frequency, up to the one starting by start_date_to, paged by
// start date; one the caller does not see is not found.
func TestListTaxPeriods(t *testing.T) {
	e := env(t)
	gstID, vatID := id(t, e, "GST"), id(t, e, "VAT")
	for name, c := range map[string]struct{ path, want string }{
		"a quarter's": {"/v1/tax-registrations/" + gstID + "/periods?start_date_to=2026-10-31",
			"2026-01-01..2026-03-31 2026-04-01..2026-06-30 2026-07-01..2026-09-30 2026-10-01..2026-12-31"},
		"next year's included": {"/v1/tax-registrations/" + gstID + "/periods?start_date_to=2027-01-01&order_by=start_date%20desc",
			"2027-01-01..2027-03-31 2026-10-01..2026-12-31 2026-07-01..2026-09-30 2026-04-01..2026-06-30 2026-01-01..2026-03-31"},
		"months from the 15th": {"/v1/tax-registrations/" + vatID + "/periods?start_date_to=2026-03-14",
			"2026-01-15..2026-02-14 2026-02-15..2026-03-14"},
		"before the first": {"/v1/tax-registrations/" + gstID + "/periods?start_date_to=2025-12-31", ""},
	} {
		t.Run(name, func(t *testing.T) {
			page, _ := e.In(t).Must(200, "GET", c.path, "", "")
			want(t, "periods", periods(page), c.want)
			want(t, "registration", page["registration_code"].(string)+" "+page["registration_id"].(string), map[bool]string{true: "GST " + gstID, false: "VAT " + vatID}[strings.Contains(c.path, gstID)])
			_, zone := page["time_zone"]
			want(t, "a zone for a date asked for", zone, false)
		})
	}
	base := "/v1/tax-registrations/" + gstID + "/periods?start_date_to=2026-12-31&page_size=3"
	first, _ := e.Must(200, "GET", base, "", "")
	next, _ := e.Must(200, "GET", base+"&page_token="+first["next_page_token"].(string), "", "")
	want(t, "pages", periods(first)+" | "+periods(next),
		"2026-01-01..2026-03-31 2026-04-01..2026-06-30 2026-07-01..2026-09-30 | 2026-10-01..2026-12-31")
	e.Problem(404, "request.not_found", "GET", "/v1/tax-registrations/"+uuid.NewString()+"/periods", "", "")
	de := e.As(e.Reader(t, "Germany only", e.DE))
	de.Problem(404, "request.not_found", "GET", "/v1/tax-registrations/"+gstID+"/periods", "", "")
	de.Must(200, "GET", "/v1/tax-registrations/"+vatID+"/periods?start_date_to=2026-01-31", "", "")
}

// TestTaxPeriodsToday: without start_date_to, the periods run to the one
// under way on the registrant's date in its time zone, which the answer
// names: 23:30 UTC on 31 December is already 1 January in Dubai, which
// starts the next quarter.
func TestTaxPeriodsToday(t *testing.T) {
	zone := func(doc *config.Config) {
		registered(gst())(doc)
		doc.Companies[0].TimeZone = "Asia/Dubai"
	}
	e := fixture.NewWith(t, nil, zone, perm)
	gstID := uuid.MustParse(id(t, e, "GST"))
	h := endpoints(t, time.Date(2026, 12, 31, 23, 30, 0, 0, time.UTC))
	var page *gen.TaxPeriodPage
	if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		res, err := h.ListTaxPeriods(ctx, gen.ListTaxPeriodsParams{ID: gstID})
		page, _ = res.(*gen.TaxPeriodPage)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want(t, "through", page.StartDateTo.Format(time.DateOnly)+" "+page.TimeZone.Value, "2027-01-01 Asia/Dubai")
	if len(page.Items) != 5 {
		t.Fatalf("periods: got %d, want 5", len(page.Items))
	}
	want(t, "the last", page.Items[4].StartDate.Format(time.DateOnly), "2027-01-01")
}

// stored reads the tenant's registrations as "code company members
// authority return frequency first last active version" lines.
func stored(t *testing.T, ten pgtest.Tenant) string {
	t.Helper()
	var lines string
	pgtest.Scan(t, `SELECT coalesce(string_agg(concat_ws(' ', r.code, l.code, cardinality(r.group_member_ids), r.authority, r.return_type, r.frequency,
			r.first_period_start, coalesce(r.last_period_end::text, '-'), r.is_active::text, r.version), ' | ' ORDER BY r.code), '')
		FROM tax.registration r JOIN tenancy.legal_entity l ON l.tenant_id = r.tenant_id AND l.id = r.legal_entity_id WHERE r.tenant_id = $1`,
		[]any{ten.ID}, &lines)
	return lines
}

// TestSetup: activation keeps the document's registrations, the number
// sealed and fingerprinted with the tenant's key; one the document no
// longer lists becomes inactive and comes back; an end is set; the same
// document again changes nothing.
func TestSetup(t *testing.T) {
	e := env(t)
	want(t, "after bootstrap", stored(t, e.Tenant), "GST 1000 0 CRA GST34 quarterly 2026-01-01 - true 1 | VAT 1000 1 FTA VAT201 monthly 2026-01-15 - true 1")
	var sealed []byte
	var print, last4 string
	pgtest.Scan(t, `SELECT number_cipher, number_fingerprint, number_last4 FROM tax.registration WHERE tenant_id = $1 AND code = 'GST'`,
		[]any{e.Tenant.ID}, &sealed, &print, &last4)
	number, err := apitest.FieldKeys().Open(context.Background(), e.Tenant.ID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "the number sealed", number, "123456789RT0001")
	want(t, "its fingerprint", print, apitest.Fingerprint(t, e.Tenant.ID, "123456789RT0001"))
	want(t, "its last four", last4, "0001")

	apply := func(rs ...config.TaxRegistration) {
		t.Helper()
		doc := fixture.Document(t)
		registered(rs...)(doc)
		if err := apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return setup().Apply(ctx, doc) }); err != nil {
			t.Fatal(err)
		}
	}
	ended := gst()
	ended.LastPeriodEnd = "2026-12-31"
	apply(ended)
	want(t, "after an end and a removal", stored(t, e.Tenant), "GST 1000 0 CRA GST34 quarterly 2026-01-01 2026-12-31 true 2 | VAT 1000 1 FTA VAT201 monthly 2026-01-15 - false 2")
	apply(ended, vat())
	apply(ended, vat())
	want(t, "back, and the same again", stored(t, e.Tenant), "GST 1000 0 CRA GST34 quarterly 2026-01-01 2026-12-31 true 2 | VAT 1000 1 FTA VAT201 monthly 2026-01-15 - true 3")
	apply(ended)
	page, _ := e.Must(200, "GET", "/v1/tax-registrations?authority=FTA", "", "")
	want(t, "a registration no longer listed", page["items"].([]any)[0].(map[string]any)["status"], any("inactive"))
	apply(ended, vat())
	page, _ = e.Must(200, "GET", "/v1/tax-registrations?authority=CRA", "", "")
	want(t, "the end shown", page["items"].([]any)[0].(map[string]any)["last_period_end"], any("2026-12-31"))
	gstID := id(t, e, "GST")
	page, _ = e.Must(200, "GET", "/v1/tax-registrations/"+gstID+"/periods?start_date_to=2027-12-31", "", "")
	want(t, "no period after the end", periods(page), "2026-01-01..2026-03-31 2026-04-01..2026-06-30 2026-07-01..2026-09-30 2026-10-01..2026-12-31")
}

func setup() taxfiling.Setup {
	return taxfiling.Setup{LegalEntities: tenancy.Directory{}, Keys: apitest.FieldKeys()}
}

// TestFixedSettings: what a registration's periods and returns follow from
// never changes under its code: a proposal changing it is refused at each
// path when checked, and activation refuses it with a problem (409
// tax.registration_fixed); the number is compared as it is normalized.
func TestFixedSettings(t *testing.T) {
	e := env(t)
	changed := func(f func(r *config.TaxRegistration)) []config.TaxRegistration {
		r := vat()
		f(&r)
		return []config.TaxRegistration{gst(), r}
	}
	const p = "companies[1000].tax_registrations[VAT]"
	for name, c := range map[string]struct {
		rs   []config.TaxRegistration
		want string
	}{
		"nothing fixed":       {changed(func(r *config.TaxRegistration) { r.Number, r.LastPeriodEnd = "1002003 00045678", "2026-12-14" }), ""},
		"authority":           {changed(func(r *config.TaxRegistration) { r.Authority = "FTA2" }), p + ".authority"},
		"number":              {changed(func(r *config.TaxRegistration) { r.Number = "100200300045679" }), p + ".number"},
		"return type":         {changed(func(r *config.TaxRegistration) { r.ReturnType = "VAT201A" }), p + ".return_type"},
		"frequency":           {changed(func(r *config.TaxRegistration) { r.Frequency = config.TaxQuarterly }), p + ".frequency"},
		"first period":        {changed(func(r *config.TaxRegistration) { r.FirstPeriodStart = "2026-02-15" }), p + ".first_period_start"},
		"group":               {changed(func(r *config.TaxRegistration) { r.GroupMembers = nil }), p + ".group_members"},
		"moved to 2000":       {nil, "companies[2000].tax_registrations[VAT] companies[2000].tax_registrations[VAT].group_members"},
		"frequency and group": {changed(func(r *config.TaxRegistration) { r.Frequency, r.GroupMembers = config.TaxQuarterly, nil }), p + ".frequency " + p + ".group_members"},
	} {
		t.Run(name, func(t *testing.T) {
			doc := fixture.Document(t)
			registered(c.rs...)(doc)
			if c.rs == nil {
				moved := vat()
				moved.GroupMembers = []string{"1000"}
				registered(gst())(doc)
				doc.Companies[1].TaxRegistrations = []config.TaxRegistration{moved}
			}
			var findings []config.Finding
			if err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				findings, err = setup().Check(ctx, nil, doc)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, f := range findings {
				paths = append(paths, f.Path)
			}
			want(t, "findings", strings.Join(paths, " "), c.want)
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return setup().Apply(ctx, doc) })
			if c.want == "" {
				want(t, "applied", err, nil)
				return
			}
			want(t, "activation refused", problem.From(err).Code, "tax.registration_fixed")
		})
	}
}

// TestUnknownCompany: a registration of a company without a legal entity
// is the ledger's finding to report, and cannot be applied.
func TestUnknownCompany(t *testing.T) {
	e := env(t)
	doc := fixture.Document(t)
	member := vat()
	member.GroupMembers = []string{"9999"}
	registered(member)(doc)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		findings, err := setup().Check(ctx, nil, doc)
		want(t, "findings", len(findings), 0)
		return err
	})
	want(t, "check", err, nil)
	err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return setup().Apply(ctx, doc) })
	if err == nil || !strings.Contains(err.Error(), "no legal entity") {
		t.Errorf("apply: got %v, want no legal entity", err)
	}
}

// TestRegistrationFixedProblem: the trigger backing up the check, which a
// writer other than Setup meets, is the check's problem.
func TestRegistrationFixedProblem(t *testing.T) {
	e := env(t)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		tx, _, err := reqctx.Request(ctx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE tax.registration SET frequency = 'annual', version = version + 1 WHERE code = 'GST'`)
		return err
	})
	want(t, "problem", problem.From(err).Code, "tax.registration_fixed")
	want(t, "status", problem.From(err).Status, 409)
}

// TestGroupOfThree: a tax group's members are a set: listed in another
// order, they are the same group.
func TestGroupOfThree(t *testing.T) {
	group := vat()
	group.GroupMembers = []string{"2000", "3000"}
	third := func(doc *config.Config) {
		co := doc.Companies[1]
		co.Code, co.Name = "3000", "Acme GmbH Zwei"
		doc.Companies = append(doc.Companies, co)
	}
	e := fixture.NewWith(t, []fixture.Company{{Code: "3000", Name: "Acme GmbH Zwei", Country: "DE", Currency: "EUR"}},
		func(doc *config.Config) { third(doc); registered(group)(doc) }, perm)
	doc := fixture.Document(t)
	third(doc)
	group.GroupMembers = []string{"3000", "2000"}
	registered(group)(doc)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		findings, err := setup().Check(ctx, nil, doc)
		want(t, "findings", len(findings), 0)
		return err
	})
	want(t, "check", err, nil)
	page, _ := e.Must(200, "GET", "/v1/tax-registrations?legal_entity_id="+e.DE.String(), "", "")
	members := page["items"].([]any)[0].(map[string]any)["group_members"].([]any)
	want(t, "members", len(members), 2)
}

// TestUnparsedDates: a date that does not parse is the document's
// validation finding, so the check passes over its registration; applying
// such a document, which validation refuses first, fails.
func TestUnparsedDates(t *testing.T) {
	e := env(t)
	for name, f := range map[string]func(r *config.TaxRegistration){
		"first period start": func(r *config.TaxRegistration) { r.FirstPeriodStart = "January" },
		"last period end":    func(r *config.TaxRegistration) { r.LastPeriodEnd = "December" },
	} {
		t.Run(name, func(t *testing.T) {
			r := gst()
			f(&r)
			doc := fixture.Document(t)
			registered(r)(doc)
			var findings []config.Finding
			err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
				var err error
				findings, err = setup().Check(ctx, nil, doc)
				return err
			})
			want(t, "check", err, nil)
			want(t, "findings", len(findings), 0)
			err = apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error { return setup().Apply(ctx, doc) })
			if err == nil {
				t.Error("apply: got no error, want the date refused")
			}
		})
	}
}

// TestRegistrationOfNoLegalEntity: a stored registration naming a legal
// entity the directory does not know fails the listing rather than naming
// no company.
func TestRegistrationOfNoLegalEntity(t *testing.T) {
	e := env(t)
	pgtest.Exec(t, `INSERT INTO tax.registration (tenant_id, id, code, legal_entity_id, authority, number_cipher, number_fingerprint, number_last4,
			return_type, frequency, first_period_start, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'LOST', gen_random_uuid(), 'CRA', '\x01', repeat('a', 64), '0001', 'GST34', 'annual', '2026-01-01', $2, $2)`,
		e.Tenant.ID, e.Tenant.Human.ID)
	err := apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := endpoints(t, time.Now()).ListTaxRegistrations(ctx, gen.ListTaxRegistrationsParams{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "no legal entity") {
		t.Errorf("list: got %v, want no legal entity", err)
	}
}
