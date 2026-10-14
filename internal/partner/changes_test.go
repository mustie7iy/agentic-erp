package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/httpapi/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func (e env) update(pid, payload string) string {
	return fmt.Sprintf(`{"partner_id": %q, "kind": "update", "payload": %s, "reason": "Update"}`, pid, payload)
}

func TestUpdateChange(t *testing.T) {
	e := setup(t)
	p := e.active(t, "Muster", 0)
	pid := id(p)
	payload := fmt.Sprintf(`{"name": "Muster AG", "legal_name": "Muster Aktiengesellschaft", "language": null, "roles": ["supplier", "customer"],
		"supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": "NT60"}],
		"addresses": [{"purpose": "legal", "line1": "Neue Strasse 2", "city": "Hamburg", "country_code": "DE", "valid_from": %q},
			{"purpose": "remit_to", "line1": "Postfach 1", "city": "Hamburg", "country_code": "DE", "valid_from": %q, "valid_to": %q}],
		"tax_identifiers": [{"scheme": "EU_VAT", "value": "DE999999999"}, {"scheme": "DE_TAX_NO", "value": "12/345/67890"}],
		"extensions": {"cost_object_hint": null, "segment": "B2B", "routing": {"desk": "north"}}}`, e.le, e.recon, e.date(0), e.date(3), e.date(30))
	c, tag := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, payload), "")
	if c["kind"] != "update" {
		t.Errorf("change kind: got %v, want update", c["kind"])
	}
	if c["status"] != "pending" {
		t.Errorf("change status: got %v, want pending", c["status"])
	}
	if tag != `"2"` {
		t.Errorf("change tag: got %s, want \"2\"", tag)
	}
	if c["workflow_request_id"] == nil {
		t.Error("change workflow_request_id: got nil, want a request")
	}
	pl := c["payload"].(map[string]any)
	if pl["language"] != nil {
		t.Errorf("payload language: got %v, want null", pl["language"])
	}
	if pl["legal_name"] != "Muster Aktiengesellschaft" {
		t.Errorf("payload legal_name: got %v, want Muster Aktiengesellschaft", pl["legal_name"])
	}
	if strings.Contains(fmt.Sprint(pl), "DE999999999") {
		t.Errorf("payload: got the plain tax identifier in %v, want it hidden", pl)
	}
	if _, ok := pl["language"]; !ok {
		t.Fatalf("cleared language missing from payload: %v", pl)
	}
	e.Problem(409, "partner.change_pending", "POST", "/v1/partner-changes", e.update(pid, `{"name": "Again"}`), "")
	e.mustApply(t, id(c))
	got, _ := e.Must(200, "GET", "/v1/business-partners/"+pid, "", "")
	for field, want := range map[string]any{"name": "Muster AG", "legal_name": "Muster Aktiengesellschaft", "language": nil} {
		if got[field] != want {
			t.Errorf("partner %s: got %v, want %v", field, got[field], want)
		}
	}
	if roles := got["roles"].([]any); fmt.Sprint(roles) != "[customer supplier]" {
		t.Errorf("partner roles: got %v, want [customer supplier]", roles)
	}
	if term := got["supplier_companies"].([]any)[0].(map[string]any)["payment_term_code"]; term != "NT60" {
		t.Errorf("payment term: got %v, want NT60", term)
	}
	ext := got["extensions"].(map[string]any)
	if ext["segment"] != "B2B" {
		t.Errorf("extension segment: got %v, want B2B", ext["segment"])
	}
	if ext["cost_object_hint"] != nil {
		t.Errorf("extension cost_object_hint: got %v, want none", ext["cost_object_hint"])
	}
	if desk := ext["routing"].(map[string]any)["desk"]; desk != "north" {
		t.Errorf("extension routing.desk: got %v, want north", desk)
	}
	addrs := got["addresses"].([]any)
	if len(addrs) != 3 {
		t.Fatalf("addresses: %v", addrs)
	}
	old, current, remit := addrs[0].(map[string]any), addrs[1].(map[string]any), addrs[2].(map[string]any)
	if old["valid_to"] != e.date(0) {
		t.Errorf("old address valid_to: got %v, want %s", old["valid_to"], e.date(0))
	}
	if current["line1"] != "Neue Strasse 2" {
		t.Errorf("current address line1: got %v, want Neue Strasse 2", current["line1"])
	}
	if current["valid_to"] != nil {
		t.Errorf("current address valid_to: got %v, want none", current["valid_to"])
	}
	if remit["valid_from"] != e.date(3) {
		t.Errorf("remit-to address valid_from: got %v, want %s", remit["valid_from"], e.date(3))
	}
	taxes := got["tax_identifiers"].([]any)
	if len(taxes) != 2 {
		t.Fatalf("tax identifiers: got %d %v, want 2", len(taxes), taxes)
	}
	if last4 := taxes[1].(map[string]any)["value_last4"]; last4 != "9999" {
		t.Errorf("tax identifier last 4: got %v, want 9999", last4)
	}
	updated := e.events(t, "business_partner_updated")
	if len(updated) != 1 {
		t.Fatalf("updated events: got %d %v, want 1", len(updated), updated)
	}
	if updated[0]["name"] != "Muster AG" {
		t.Errorf("updated event name: got %v, want Muster AG", updated[0]["name"])
	}
	if updated[0]["language"] != nil {
		t.Errorf("updated event language: got %v, want null", updated[0]["language"])
	}
	for _, want := range []string{"supplier_company." + e.le.String(), "tax_identifier.DE_TAX_NO"} {
		if fields := fmt.Sprint(updated[0]["changed_fields"]); !strings.Contains(fields, want) {
			t.Errorf("updated event changed_fields: got %s, want it to contain %s", fields, want)
		}
	}
	// A minimal change publishes only what it names.
	c2, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, `{"legal_name": null, "language": "en"}`), "")
	e.mustApply(t, id(c2))
	u := e.events(t, "business_partner_updated")[1]
	if fields := fmt.Sprint(u["changed_fields"]); fields != "[legal_name language]" {
		t.Errorf("second update changed_fields: got %s, want [legal_name language]", fields)
	}
	if u["legal_name"] != nil {
		t.Errorf("second update legal_name: got %v, want null", u["legal_name"])
	}
	// An address dated from tomorrow ends the current one tomorrow.
	c3, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, fmt.Sprintf(`{"addresses": [{"purpose": "legal", "line1": "Dritte 3",
		"city": "Bonn", "country_code": "DE", "valid_from": %q}]}`, e.date(1))), "")
	e.mustApply(t, id(c3))
	got, _ = e.Must(200, "GET", "/v1/business-partners/"+pid, "", "")
	if got["addresses"].([]any)[1].(map[string]any)["valid_to"] != e.date(1) {
		t.Fatalf("future replacement: %v", got["addresses"])
	}
	// An address that already starts after the new one cannot be replaced.
	c4, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, fmt.Sprintf(`{"addresses": [{"purpose": "legal", "line1": "Vierte 4",
		"city": "Bonn", "country_code": "DE", "valid_from": %q}]}`, e.date(0))), "")
	pgtest.WantErr(t, e.apply(t, id(c4)), "partner.address_overlap")
}

func TestUpdateRefusals(t *testing.T) {
	e := setup(t)
	p := e.active(t, "Muster", 0)
	pid := id(p)
	customer, cc := e.create(t, `{"kind": "person", "name": "C", "country_code": "DE", "roles": ["customer"], "reason": "r"}`)
	e.mustApply(t, id(cc))
	company := fmt.Sprintf(`{"supplier_companies": [{"legal_entity_id": %q, "reconciliation_account_id": %q, "payment_term_code": "NT30"}]}`, e.le, e.recon)
	cases := []struct {
		name, body string
		code       string
	}{
		{"kind", strings.Replace(e.update(pid, `{"name": "N"}`), `"update"`, `"block"`, 1), "partner.unsupported_kind"},
		{"unknown partner", e.update(uuid.NewString(), `{"name": "N"}`), "request.reference_not_found"},
		{"nothing", e.update(pid, `{}`), "partner.no_changes"},
		{"role", e.update(pid, `{"roles": ["supplier", "vendor"]}`), "partner.unknown_role"},
		{"role removed", e.update(pid, `{"roles": ["customer"]}`), "partner.role_removal"},
		{"extension", e.update(pid, `{"extensions": {"Bad": 1}}`), "request.invalid_extension"},
		{"address country", e.update(pid, fmt.Sprintf(`{"addresses": [{"purpose": "legal", "line1": "L", "city": "C", "country_code": "QQ", "valid_from": %q}]}`, e.date(0))), "partner.unknown_country"},
		{"supplier data of a customer", e.update(id(customer), company), "partner.supplier_role_required"},
		{"backdated address", e.update(pid, fmt.Sprintf(`{"addresses": [{"purpose": "legal", "line1": "L", "city": "C", "country_code": "DE", "valid_from": %q}]}`, e.date(-1))), "partner.backdated_address"},
		{"tax scheme twice", e.update(pid, `{"tax_identifiers": [{"scheme": "EU_VAT", "value": "1"}, {"scheme": "EU_VAT", "value": "2"}]}`), "partner.duplicate_tax_scheme"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, m, _ := e.in(t).Do("POST", "/v1/partner-changes", c.body, "")
			if got != 422 {
				t.Errorf("status: got %d, want 422 (%v)", got, m)
			}
			if m["code"] != c.code {
				t.Errorf("code: got %v, want %s", m["code"], c.code)
			}
		})
	}
	// A customer that becomes a supplier may get supplier data in the same change.
	e.Must(201, "POST", "/v1/partner-changes", e.update(id(customer), strings.Replace(company, `{"supplier_companies"`, `{"roles": ["customer", "supplier"], "supplier_companies"`, 1)), "")
}

func TestBankChanges(t *testing.T) {
	e := setup(t)
	p := e.active(t, "Muster", 0)
	pid := id(p)
	path := "/v1/business-partners/" + pid + "/bank-accounts"
	body := func(number, scheme, from string) string {
		return fmt.Sprintf(`{"account_number": %q, "account_scheme": %q, "bank_country_code": "DE", "holder_name": "Muster", "valid_from": %q,
			"reason": "Confirmed by call-back"}`, number, scheme, from)
	}
	e.Problem(422, "partner.backdated_bank_account", "POST", path, body("12345678", "domestic", e.date(-1)), "")
	e.Problem(422, "partner.unknown_country", "POST", path, strings.Replace(body("12345678", "domestic", e.date(0)), `"DE"`, `"QQ"`, 1), "")
	e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+"/bank-accounts", body("12345678", "domestic", e.date(0)), "")
	c, _ := e.Must(201, "POST", path, body("12345678", "domestic", e.date(2)), "")
	if c["kind"] != "bank_change" {
		t.Errorf("bank change kind: got %v, want bank_change", c["kind"])
	}
	cp := c["payload"].(map[string]any)
	if cp["action"] != "add" {
		t.Errorf("bank change action: got %v, want add", cp["action"])
	}
	if last4 := apitest.Object(t, cp, "bank_account")["account_last4"]; last4 != "5678" {
		t.Errorf("bank change account_last4: got %v, want 5678", last4)
	}
	e.Problem(409, "partner.change_pending", "POST", path, body("87654321", "domestic", e.date(2)), "")
	e.mustApply(t, id(c))
	all, _ := e.Must(200, "GET", path, "", "")
	items := all["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("accounts after the change: got %d %v, want 2", len(items), items)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	for field, want := range map[string]any{"valid_to": e.date(2), "ended_by": e.ten.Human.ID.String()} {
		if first[field] != want {
			t.Errorf("first account %s: got %v, want %v", field, first[field], want)
		}
	}
	if first["ended_at"] == nil {
		t.Error("first account ended_at: got nil, want a time")
	}
	for field, want := range map[string]any{"valid_from": e.date(2), "account_scheme": "domestic"} {
		if second[field] != want {
			t.Errorf("second account %s: got %v, want %v", field, second[field], want)
		}
	}
	changed := e.events(t, "bank_account_changed")
	last := changed[len(changed)-1]
	for field, want := range map[string]any{"ended_bank_account_id": id(first), "ended_valid_to": e.date(2), "bic": nil} {
		if last[field] != want {
			t.Errorf("bank account changed %s: got %v, want %v", field, last[field], want)
		}
	}
	today, _ := e.Must(200, "GET", path+"?valid_on="+e.date(0), "", "")
	if items := today["items"].([]any); len(items) != 1 {
		t.Fatalf("valid today: got %d %v, want 1", len(items), items)
	} else if got := id(items[0].(map[string]any)); got != id(first) {
		t.Errorf("valid today: got %s, want %s", got, id(first))
	}
	if paged, _ := e.Must(200, "GET", path+"?page_size=1", "", ""); paged["next_page_token"] == nil {
		t.Fatalf("paging: %v", paged)
	} else if rest, _ := e.Must(200, "GET", path+"?page_size=1&page_token="+paged["next_page_token"].(string), "", ""); id(rest["items"].([]any)[0].(map[string]any)) != id(second) {
		t.Fatalf("page 2: %v", rest)
	}
	e.Problem(404, "request.not_found", "GET", "/v1/business-partners/"+uuid.NewString()+"/bank-accounts", "", "")
	e.Problem(404, "request.not_found", "GET", path+"/"+uuid.NewString(), "", "")

	acct, tag := e.Must(200, "GET", path+"/"+id(second), "", "")
	end := path + "/" + id(second) + ":end"
	endBody := func(to string) string { return fmt.Sprintf(`{"valid_to": %q, "reason": "Closed"}`, to) }
	e.Problem(412, "request.version_mismatch", "POST", end, endBody(e.date(10)), `"stale"`)
	e.Problem(422, "partner.invalid_end", "POST", end, endBody(e.date(-1)), tag)
	e.Problem(422, "partner.invalid_end", "POST", end, endBody(e.date(2)), tag)
	e.Problem(404, "request.not_found", "POST", path+"/"+uuid.NewString()+":end", endBody(e.date(10)), tag)
	ended := path + "/" + id(first) + ":end"
	_, firstTag := e.Must(200, "GET", path+"/"+id(first), "", "")
	e.Problem(422, "partner.invalid_end", "POST", ended, endBody(e.date(2)), firstTag)
	ec, _ := e.Must(200, "POST", end, endBody(e.date(10)), tag)
	ep := ec["payload"].(map[string]any)
	if ep["bank_account_id"] != id(acct) {
		t.Errorf("end change bank_account_id: got %v, want %s", ep["bank_account_id"], id(acct))
	}
	if ep["valid_to"] != e.date(10) {
		t.Errorf("end change valid_to: got %v, want %s", ep["valid_to"], e.date(10))
	}
	e.mustApply(t, id(ec))
	if got, _ := e.Must(200, "GET", path+"/"+id(second), "", ""); got["valid_to"] != e.date(10) {
		t.Fatalf("not ended: %v", got)
	}
	changed = e.events(t, "bank_account_changed")
	last = changed[len(changed)-1]
	for field, want := range map[string]any{"bank_account_id": nil, "ended_bank_account_id": id(second), "valid_to": e.date(10)} {
		if last[field] != want {
			t.Errorf("end event %s: got %v, want %v", field, last[field], want)
		}
	}
	// An end that is no longer earlier than the account's end is refused on apply.
	_, tag = e.Must(200, "GET", path+"/"+id(second), "", "")
	late, _ := e.Must(200, "POST", end, endBody(e.date(8)), tag)
	pgtest.Exec(t, `UPDATE partner.bank_account SET valid_to = current_date + 5, ended_by = created_by WHERE id = $1`, uuid.MustParse(id(second)))
	pgtest.WantErr(t, e.apply(t, id(late)), "partner.invalid_end")

	// A future account is not ended by an earlier one: they would overlap.
	q := e.active(t, "Future", 20)
	fc, _ := e.Must(201, "POST", "/v1/business-partners/"+id(q)+"/bank-accounts", body(iban, "iban", e.date(0)), "")
	pgtest.WantErr(t, e.apply(t, id(fc)), "partner.bank_account_overlap")
}

func TestPorts(t *testing.T) {
	e := setup(t)
	active := e.active(t, "Active", 0)
	pending, _ := e.create(t, e.supplier("Pending", 0))
	customer, cc := e.create(t, `{"kind": "person", "name": "C", "country_code": "DE", "roles": ["customer"], "reason": "r"}`)
	e.mustApply(t, id(cc))
	other := uuid.Must(uuid.NewV7())
	pgtest.Exec(t, `INSERT INTO tenancy.legal_entity (tenant_id, id, code, name, country_code, local_currency, fiscal_calendar_id, status, created_by, updated_by)
		VALUES ($1, $2, '2000', 'Other', 'DE', 'EUR', gen_random_uuid(), 'active', $3, $3)`, e.ten.ID, other, e.ten.Human.ID)
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		check := func(p string, le uuid.UUID, kind, want string) {
			got, err := partner.Postings{}.PostingBlock(ctx, uuid.MustParse(p), le, kind)
			if err != nil {
				t.Errorf("%s %s: %v", p, kind, err)
			}
			if want == "" && got != "" {
				t.Errorf("%s %s: got %q, want no block", p, kind, got)
			}
			if !strings.Contains(got, want) {
				t.Errorf("%s %s: got %q, want it to contain %q", p, kind, got, want)
			}
		}
		check(id(active), e.le, "supplier", "")
		check(uuid.NewString(), e.le, "supplier", "no business partner")
		check(id(pending), e.le, "supplier", "is pending")
		check(id(customer), e.le, "supplier", "does not have the supplier role")
		check(id(customer), e.le, "customer", "")
		check(id(active), other, "supplier", "not set up in this legal entity")
		pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, uuid.MustParse(id(active)))
		check(id(active), e.le, "supplier", "blocked for posting")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (partner.Postings{}).PostingBlock(context.Background(), uuid.New(), e.le, "supplier"); err == nil {
		t.Error("posting block outside a request")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "posting block", func(ctx context.Context) error {
		_, err := partner.Postings{}.PostingBlock(ctx, uuid.MustParse(id(active)), e.le, "supplier")
		return err
	})

	base := &config.Config{PaymentTerms: []config.PaymentTerm{{Key: "NT30", Description: "Net 30", NetDays: 30}, {Key: "NT90", Description: "Net 90", NetDays: 90}}}
	proposed := &config.Config{PaymentTerms: []config.PaymentTerm{{Key: "NT30", Description: "Net thirty", NetDays: 31}, {Key: "NT10", Description: "Net 10", NetDays: 10}}}
	terms := partner.PaymentTerms{}
	if f, err := terms.Check(context.Background(), nil, proposed); err != nil || f != nil {
		t.Fatalf("first version: %v %v", f, err)
	}
	if f, _ := terms.Check(context.Background(), base, proposed); len(f) != 1 {
		t.Fatalf("removed term: got %d findings %v, want 1", len(f), f)
	} else if f[0].Path != "payment_terms[NT90]" {
		t.Errorf("removed term: got %s, want payment_terms[NT90]", f[0].Path)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return terms.Apply(ctx, proposed) }); err != nil {
		t.Fatal(err)
	}
	var desc string
	var days, version int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT description, net_days, version FROM partner.payment_term WHERE tenant_id = $1 AND code = 'NT30'`,
		e.ten.ID).Scan(&desc, &days, &version); err != nil {
		t.Fatalf("read NT30: %v", err)
	}
	if desc != "Net thirty" {
		t.Errorf("NT30 description: got %s, want Net thirty", desc)
	}
	if days != 31 {
		t.Errorf("NT30 net days: got %d, want 31", days)
	}
	if version != 2 {
		t.Errorf("NT30 version: got %d, want 2", version)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return terms.Apply(ctx, proposed) }); err != nil {
		t.Fatal(err)
	}
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT version FROM partner.payment_term WHERE tenant_id = $1 AND code = 'NT30'`, e.ten.ID).Scan(&version); err != nil {
		t.Fatalf("read the term version: %v", err)
	}
	if version != 2 {
		t.Fatalf("an unchanged term was updated: version %d", version)
	}
	if err := terms.Apply(context.Background(), proposed); err == nil {
		t.Error("apply outside a request")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "payment terms", func(ctx context.Context) error { return terms.Apply(ctx, proposed) })
}
