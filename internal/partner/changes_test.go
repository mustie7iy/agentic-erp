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
	if c["kind"] != "update" || c["status"] != "pending" || tag != `"2"` || c["workflow_request_id"] == nil {
		t.Fatalf("change: %v", c)
	}
	pl := c["payload"].(map[string]any)
	if pl["language"] != nil || pl["legal_name"] != "Muster Aktiengesellschaft" || strings.Contains(fmt.Sprint(pl), "DE999999999") {
		t.Fatalf("payload: %v", pl)
	}
	if _, ok := pl["language"]; !ok {
		t.Fatalf("cleared language missing from payload: %v", pl)
	}
	e.Problem(409, "partner.change_pending", "POST", "/v1/partner-changes", e.update(pid, `{"name": "Again"}`), "")
	e.mustApply(t, id(c))
	got, _ := e.Must(200, "GET", "/v1/business-partners/"+pid, "", "")
	if got["name"] != "Muster AG" || got["legal_name"] != "Muster Aktiengesellschaft" || got["language"] != nil ||
		strings.Join([]string{got["roles"].([]any)[0].(string), got["roles"].([]any)[1].(string)}, ",") != "customer,supplier" ||
		got["supplier_companies"].([]any)[0].(map[string]any)["payment_term_code"] != "NT60" {
		t.Fatalf("partner after update: %v", got)
	}
	if ext := got["extensions"].(map[string]any); ext["segment"] != "B2B" || ext["cost_object_hint"] != nil || ext["routing"].(map[string]any)["desk"] != "north" {
		t.Fatalf("extensions: %v", ext)
	}
	addrs := got["addresses"].([]any)
	if len(addrs) != 3 {
		t.Fatalf("addresses: %v", addrs)
	}
	old, current, remit := addrs[0].(map[string]any), addrs[1].(map[string]any), addrs[2].(map[string]any)
	if old["valid_to"] != e.date(0) || current["line1"] != "Neue Strasse 2" || current["valid_to"] != nil || remit["valid_from"] != e.date(3) {
		t.Fatalf("address replacement: %v", addrs)
	}
	taxes := got["tax_identifiers"].([]any)
	if len(taxes) != 2 || taxes[1].(map[string]any)["value_last4"] != "9999" {
		t.Fatalf("tax identifiers: %v", taxes)
	}
	updated := e.events(t, "business_partner_updated")
	if len(updated) != 1 || updated[0]["name"] != "Muster AG" || updated[0]["language"] != nil ||
		!strings.Contains(fmt.Sprint(updated[0]["changed_fields"]), "supplier_company."+e.le.String()) ||
		!strings.Contains(fmt.Sprint(updated[0]["changed_fields"]), "tax_identifier.DE_TAX_NO") {
		t.Fatalf("updated events: %v", updated)
	}
	// A minimal change publishes only what it names.
	c2, _ := e.Must(201, "POST", "/v1/partner-changes", e.update(pid, `{"legal_name": null, "language": "en"}`), "")
	e.mustApply(t, id(c2))
	if u := e.events(t, "business_partner_updated")[1]; fmt.Sprint(u["changed_fields"]) != "[legal_name language]" || u["legal_name"] != nil {
		t.Fatalf("second update: %v", u)
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
	if err := e.apply(t, id(c4)); err == nil || !strings.Contains(err.Error(), "partner.address_overlap") {
		t.Fatalf("overlapping address: %v", err)
	}
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
		if got, m, _ := e.Do("POST", "/v1/partner-changes", c.body, ""); got != 422 || m["code"] != c.code {
			t.Errorf("%s: %d %v, want 422 %s", c.name, got, m, c.code)
		}
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
	if c["kind"] != "bank_change" || c["payload"].(map[string]any)["action"] != "add" ||
		c["payload"].(map[string]any)["bank_account"].(map[string]any)["account_last4"] != "5678" {
		t.Fatalf("bank change: %v", c)
	}
	e.Problem(409, "partner.change_pending", "POST", path, body("87654321", "domestic", e.date(2)), "")
	e.mustApply(t, id(c))
	all, _ := e.Must(200, "GET", path, "", "")
	items := all["items"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if len(items) != 2 || first["valid_to"] != e.date(2) || first["ended_by"] != e.ten.Human.ID.String() || first["ended_at"] == nil ||
		second["valid_from"] != e.date(2) || second["account_scheme"] != "domestic" {
		t.Fatalf("accounts after the change: %v", items)
	}
	changed := e.events(t, "bank_account_changed")
	if last := changed[len(changed)-1]; last["ended_bank_account_id"] != id(first) || last["ended_valid_to"] != e.date(2) || last["bic"] != nil {
		t.Fatalf("bank account changed: %v", last)
	}
	if today, _ := e.Must(200, "GET", path+"?valid_on="+e.date(0), "", ""); len(today["items"].([]any)) != 1 || id(today["items"].([]any)[0].(map[string]any)) != id(first) {
		t.Fatalf("valid today: %v", today)
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
	if ec["payload"].(map[string]any)["bank_account_id"] != id(acct) || ec["payload"].(map[string]any)["valid_to"] != e.date(10) {
		t.Fatalf("end change: %v", ec)
	}
	e.mustApply(t, id(ec))
	if got, _ := e.Must(200, "GET", path+"/"+id(second), "", ""); got["valid_to"] != e.date(10) {
		t.Fatalf("not ended: %v", got)
	}
	if last := e.events(t, "bank_account_changed"); last[len(last)-1]["bank_account_id"] != id(second) || last[len(last)-1]["valid_to"] != e.date(10) {
		t.Fatalf("end event: %v", last[len(last)-1])
	}
	// An end that is no longer earlier than the account's end is refused on apply.
	_, tag = e.Must(200, "GET", path+"/"+id(second), "", "")
	late, _ := e.Must(200, "POST", end, endBody(e.date(8)), tag)
	pgtest.Exec(t, `UPDATE partner.bank_account SET valid_to = current_date + 5, ended_by = created_by WHERE id = $1`, uuid.MustParse(id(second)))
	if err := e.apply(t, id(late)); err == nil || !strings.Contains(err.Error(), "partner.invalid_end") {
		t.Fatalf("late end: %v", err)
	}

	// A future account is not ended by an earlier one: they would overlap.
	q := e.active(t, "Future", 20)
	fc, _ := e.Must(201, "POST", "/v1/business-partners/"+id(q)+"/bank-accounts", body(iban, "iban", e.date(0)), "")
	if err := e.apply(t, id(fc)); err == nil || !strings.Contains(err.Error(), "partner.bank_account_overlap") {
		t.Fatalf("overlapping account: %v", err)
	}
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
			if err != nil || !strings.Contains(got, want) || (want == "" && got != "") {
				t.Errorf("%s %s: %q %v, want %q", p, kind, got, err, want)
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
	if f, _ := terms.Check(context.Background(), base, proposed); len(f) != 1 || f[0].Path != "payment_terms[NT90]" {
		t.Fatalf("removed term: %v", f)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return terms.Apply(ctx, proposed) }); err != nil {
		t.Fatal(err)
	}
	var desc string
	var days, version int
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT description, net_days, version FROM partner.payment_term WHERE tenant_id = $1 AND code = 'NT30'`,
		e.ten.ID).Scan(&desc, &days, &version); err != nil || desc != "Net thirty" || days != 31 || version != 2 {
		t.Fatalf("NT30: %s %d %d %v", desc, days, version, err)
	}
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error { return terms.Apply(ctx, proposed) }); err != nil {
		t.Fatal(err)
	}
	pgtest.Admin(t).QueryRow(context.Background(), `SELECT version FROM partner.payment_term WHERE tenant_id = $1 AND code = 'NT30'`, e.ten.ID).Scan(&version)
	if version != 2 {
		t.Fatalf("an unchanged term was updated: version %d", version)
	}
	if err := terms.Apply(context.Background(), proposed); err == nil {
		t.Error("apply outside a request")
	}
	apitest.EveryFailure(t, e.ten, e.ten.Human, "payment terms", func(ctx context.Context) error { return terms.Apply(ctx, proposed) })
}
