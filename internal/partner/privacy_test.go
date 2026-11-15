package partner_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// blockPersonal blocks the partner's personal data at its version.
func (e env) blockPersonal(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	_, tag := e.Must(200, "GET", "/v1/business-partners/"+id(p), "", "")
	got, _ := e.Must(200, "POST", "/v1/business-partners/"+id(p)+":blockPersonalData", `{"reason": "Data subject's request"}`, tag)
	return got
}

// privacyEnv is setup with the privacy officer's permission to block.
func privacyEnv(t *testing.T) env {
	t.Helper()
	e := setup(t)
	e.ten.Grant(t, "partner.privacy.block", e.ten.Human)
	return e
}

// TestBlockPersonalData (T100b, ADR-0079 §11): blocking a partner's
// personal data blocks the partner, records an applied block change,
// audits and publishes it; the partner context's reads then answer its
// names, addresses, tax identifiers, bank accounts and changes masked to
// a caller without partner.privacy.read_blocked, in full to one with it,
// and a search by name finds it only for the latter. Its data is not
// changed, nor is the partner unblocked, while the block lasts; lifting
// it reads in full again and leaves the partner blocked.
func TestBlockPersonalData(t *testing.T) {
	e := privacyEnv(t)
	a := e.active(t, "Ada Lovelace", 0)
	z := e.active(t, "zara Moss", 0) // after Ada by name and after [blocked], in C and in a language's collation
	got := e.blockPersonal(t, a)
	if got["status"] != "blocked" || got["personal_data_blocked_at"] == nil {
		t.Errorf("blocked: got status %v, blocked at %v; want blocked, a time", got["status"], got["personal_data_blocked_at"])
	}
	t.Run("events and audit", func(t *testing.T) {
		updated := e.events(t, "business_partner_updated")
		if len(updated) != 1 || fmt.Sprint(updated[0]["changed_fields"]) != "[personal_data_blocked_at]" || updated[0]["personal_data_blocked_at"] == nil {
			t.Errorf("BusinessPartnerUpdated: got %v, want one naming personal_data_blocked_at", updated)
		}
		if blocked := e.events(t, "partner_blocked"); len(blocked) != 1 {
			t.Errorf("PartnerBlocked: got %d, want 1", len(blocked))
		}
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'partner.business_partner.block_personal_data'`,
			[]any{e.ten.ID}, &n)
		if n != 1 {
			t.Errorf("audit records: got %d, want 1", n)
		}
	})
	t.Run("masked without the permission", func(t *testing.T) { checkMasked(t, e.in(t), a, true) })
	t.Run("ordered by name, masked", func(t *testing.T) { checkMaskedOrder(t, e.in(t), a, z) })
	t.Run("in full with it", func(t *testing.T) {
		e.ten.Grant(t, partner.ReadBlockedPermission, e.ten.Human)
		defer e.ten.Revoke(t, partner.ReadBlockedPermission, e.ten.Human)
		checkMasked(t, e.in(t), a, false)
	})
	t.Run("refused while blocked", func(t *testing.T) {
		e := e.in(t)
		_, tag := e.Must(200, "GET", "/v1/business-partners/"+id(a), "", "")
		e.Problem(409, "partner.personal_data_blocked", "POST", "/v1/business-partners/"+id(a)+":blockPersonalData", `{"reason": "r"}`, tag)
		e.Problem(409, "partner.personal_data_blocked", "POST", "/v1/business-partners/"+id(a)+":unblock", `{"reason": "r"}`, tag)
		e.Problem(409, "partner.personal_data_blocked", "POST", "/v1/partner-changes", e.update(id(a), `{"name": "Ada King"}`), "")
		e.Problem(409, "partner.personal_data_blocked", "POST", "/v1/business-partners/"+id(a)+"/bank-accounts", fmt.Sprintf(`{"account_number": "12345678",
			"account_scheme": "domestic", "bank_country_code": "DE", "holder_name": "A", "valid_from": %q, "reason": "r"}`, e.date(1)), "")
		page, _ := e.Must(200, "GET", "/v1/business-partners/"+id(a)+"/bank-accounts", "", "")
		bank := page["items"].([]any)[0].(map[string]any)
		e.Problem(409, "partner.personal_data_blocked", "POST", "/v1/business-partners/"+id(a)+"/bank-accounts/"+id(bank)+":end",
			fmt.Sprintf(`{"valid_to": %q, "reason": "r"}`, e.date(10)), "*")
		e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+"/bank-accounts", fmt.Sprintf(`{"account_number": "12345678",
			"account_scheme": "domestic", "bank_country_code": "DE", "holder_name": "A", "valid_from": %q, "reason": "r"}`, e.date(1)), "")
	})
	t.Run("unblocked", func(t *testing.T) {
		e := e.in(t)
		_, tag := e.Must(200, "GET", "/v1/business-partners/"+id(a), "", "")
		got, _ := e.Must(200, "POST", "/v1/business-partners/"+id(a)+":unblockPersonalData", `{"reason": "Block lifted"}`, tag)
		if got["status"] != "blocked" || got["personal_data_blocked_at"] != nil || got["name"] != "Ada Lovelace" {
			t.Errorf("unblocked: got status %v, blocked at %v, name %v; want blocked, none, Ada Lovelace", got["status"], got["personal_data_blocked_at"], got["name"])
		}
		if updated := e.events(t, "business_partner_updated"); len(updated) != 2 || updated[1]["personal_data_blocked_at"] != nil {
			t.Errorf("BusinessPartnerUpdated: got %v, want a second with personal_data_blocked_at null", updated)
		}
		_, tag = e.Must(200, "GET", "/v1/business-partners/"+id(a), "", "")
		e.Problem(409, "partner.personal_data_not_blocked", "POST", "/v1/business-partners/"+id(a)+":unblockPersonalData", `{"reason": "r"}`, tag)
		// Blocked already, it is blocked again without a second PartnerBlocked.
		e.blockPersonal(t, a)
		if blocked := e.events(t, "partner_blocked"); len(blocked) != 1 {
			t.Errorf("PartnerBlocked after a second block: got %d, want 1", len(blocked))
		}
	})
}

// checkMasked reads the partner a, blocked, through every read of the
// partner context and checks its personal data masked or in full.
func checkMasked(t *testing.T, e env, a map[string]any, mask bool) {
	t.Helper()
	want := func(full string) string {
		if mask {
			return partner.MaskedText
		}
		return full
	}
	last4 := func(full string) string {
		if mask {
			return partner.MaskedLast4
		}
		return full
	}
	got, _ := e.Must(200, "GET", "/v1/business-partners/"+id(a), "", "")
	addr := got["addresses"].([]any)[0].(map[string]any)
	tax := got["tax_identifiers"].([]any)[0].(map[string]any)
	if got["name"] != want("Ada Lovelace") || addr["line1"] != want("Hauptstrasse 1") || addr["city"] != want("Berlin") || tax["value_last4"] != last4("6789") {
		t.Errorf("the partner: got name %v, address %v, tax %v; masked %v", got["name"], addr, tax, mask)
	}
	page, _ := e.Must(200, "GET", "/v1/business-partners?ids="+id(a), "", "")
	if n := page["items"].([]any)[0].(map[string]any)["name"]; n != want("Ada Lovelace") {
		t.Errorf("the partner listed: got name %v, masked %v", n, mask)
	}
	found, _ := e.Must(200, "GET", "/v1/business-partners?name_prefix=ada", "", "")
	if n := len(found["items"].([]any)); n != map[bool]int{true: 0, false: 1}[mask] {
		t.Errorf("found by name: got %d, masked %v", n, mask)
	}
	banks, _ := e.Must(200, "GET", "/v1/business-partners/"+id(a)+"/bank-accounts", "", "")
	bank := banks["items"].([]any)[0].(map[string]any)
	one, tag := e.Must(200, "GET", "/v1/business-partners/"+id(a)+"/bank-accounts/"+id(bank), "", "")
	for _, b := range []map[string]any{bank, one} {
		if b["holder_name"] != want("Ada Lovelace") || b["account_last4"] != last4("3000") || (b["bic"] == nil) != mask {
			t.Errorf("the bank account: got %v, masked %v", b, mask)
		}
	}
	if tag == "" {
		t.Error("the bank account's ETag: got none")
	}
	changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+id(a)+"&kind=create", "", "")
	c := changes["items"].([]any)[0].(map[string]any)
	if n := c["payload"].(map[string]any)["name"]; n != want("Ada Lovelace") {
		t.Errorf("the create change's payload: got name %v, masked %v", n, mask)
	}
	one, _ = e.Must(200, "GET", "/v1/partner-changes/"+id(c), "", "")
	if n := one["payload"].(map[string]any)["name"]; n != want("Ada Lovelace") {
		t.Errorf("the create change read: got name %v, masked %v", n, mask)
	}
	ports(t, e, a, want("Ada Lovelace"), last4("3000"))
}

// ports checks the names and bank accounts other contexts take from the
// partner.
func ports(t *testing.T, e env, a map[string]any, name, last4 string) {
	t.Helper()
	pid := uuid.MustParse(id(a))
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		names, err := partner.Payees{}.Names(ctx, []uuid.UUID{pid})
		if err != nil {
			return err
		}
		if names[pid].Name != name || names[pid].Number != a["number"] {
			t.Errorf("Names: got %+v, want %s %s", names[pid], a["number"], name)
		}
		var bank uuid.UUID
		pgtest.Scan(t, `SELECT id FROM partner.bank_account WHERE partner_id = $1`, []any{pid}, &bank)
		banks, err := partner.Payees{}.BankAccounts(ctx, []uuid.UUID{bank})
		if err != nil {
			return err
		}
		if banks[bank].AccountLast4 != last4 {
			t.Errorf("BankAccounts: got %+v, want last four %s", banks[bank], last4)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPossibleDuplicatesMasked (T100b; the product owner's default of
// 2026-11-15): a partner whose personal data is blocked is listed as a
// possible duplicate by its number with its name masked, both on a change
// requested after the block and on one that listed it before, to a
// caller who may not read it.
func TestPossibleDuplicatesMasked(t *testing.T) {
	e := privacyEnv(t)
	a := e.active(t, "Ada Lovelace", 0)
	_, before := e.create(t, e.supplier("Before", 0))
	e.blockPersonal(t, a)
	_, after := e.create(t, e.supplier("After", 0))
	for name, c := range map[string]map[string]any{"listed before": before, "listed after": after} {
		t.Run(name, func(t *testing.T) {
			got, _ := e.in(t).Must(200, "GET", "/v1/partner-changes/"+id(c), "", "")
			for _, d := range got["possible_duplicates"].([]any) {
				d := d.(map[string]any)
				if d["partner_id"] == id(a) && d["name"] != partner.MaskedText {
					t.Errorf("the blocked partner listed: got %v, want its name %s", d, partner.MaskedText)
				}
			}
		})
	}
	e.ten.Grant(t, partner.ReadBlockedPermission, e.ten.Human)
	got, _ := e.Must(200, "GET", "/v1/partner-changes/"+id(before), "", "")
	if d := got["possible_duplicates"].([]any)[0].(map[string]any); d["name"] != "Ada Lovelace" {
		t.Errorf("listed before, read with the permission: got %v, want its name", d)
	}
}

// openItems is a ledger with the open items it is given.
type openItems []uuid.UUID

func (o openItems) OpenCompanies(context.Context, uuid.UUID) ([]uuid.UUID, error) { return o, nil }

// failingItems is a ledger that does not answer.
type failingItems struct{}

func (failingItems) OpenCompanies(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, errors.New("ledger down")
}

// TestBlockPersonalDataRefused (T100b): a partner with an open item is
// refused naming its company (by id when tenancy does not know it), as is
// one with a pending change or one neither active nor blocked; a ledger
// that does not answer refuses nothing silently.
func TestBlockPersonalDataRefused(t *testing.T) {
	e := privacyEnv(t)
	a := e.active(t, "Ada Lovelace", 0)
	stranger := uuid.New()
	block := func(items partner.OpenItems, p map[string]any) error {
		return apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := partner.BlockPersonalData(ctx, items, tenancy.Directory{}, uuid.MustParse(id(p)), func(int32) error { return nil }, "r")
			return err
		})
	}
	err := block(openItems{e.le, stranger}, a)
	if p := problem.From(err); p.Status != 409 || p.Code != "partner.open_items" || !strings.Contains(p.Detail, "company 1000, "+stranger.String()) {
		t.Errorf("open items: got %v, want 409 partner.open_items naming 1000 and %s", err, stranger)
	}
	if err := block(failingItems{}, a); err == nil || !strings.Contains(err.Error(), "ledger down") {
		t.Errorf("a ledger that does not answer: got %v, want its error", err)
	}
	pending, _ := e.create(t, e.supplier("Pending", 0))
	e.Problem(409, "partner.invalid_status", "POST", "/v1/business-partners/"+id(pending)+":blockPersonalData", `{"reason": "r"}`, `"1"`)
	c := e.active(t, "Changing", 0)
	e.Must(201, "POST", "/v1/partner-changes", e.update(id(c), `{"name": "Changed"}`), "")
	_, tag := e.Must(200, "GET", "/v1/business-partners/"+id(c), "", "")
	e.Problem(409, "partner.change_pending", "POST", "/v1/business-partners/"+id(c)+":blockPersonalData", `{"reason": "r"}`, tag)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/business-partners/"+id(a)+":blockPersonalData", `{"reason": "r"}`, `"99"`)
	e.Problem(412, "request.version_mismatch", "POST", "/v1/business-partners/"+id(a)+":unblockPersonalData", `{"reason": "r"}`, `"99"`)
	e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+":blockPersonalData", `{"reason": "r"}`, "*")
	e.Problem(404, "request.not_found", "POST", "/v1/business-partners/"+uuid.NewString()+":unblockPersonalData", `{"reason": "r"}`, "*")
	agent := e
	agent.As = e.ten.Agent
	agent.Problem(403, "identity.agent_not_allowed", "POST", "/v1/business-partners/"+id(a)+":blockPersonalData", `{"reason": "r"}`, "*")
}

// failingEntities is a tenancy context that does not answer.
type failingEntities struct{}

func (failingEntities) Entity(context.Context, uuid.UUID) (tenancy.Entity, bool, error) {
	return tenancy.Entity{}, false, errors.New("tenancy down")
}

// TestMaskedInFull (T100b): a partner's legal name and every line of its
// address are masked with its name; an open item's company that tenancy
// does not answer for refuses with tenancy's error.
func TestMaskedInFull(t *testing.T) {
	e := privacyEnv(t)
	body := strings.Replace(e.supplier("Grace Hopper", 0), `"roles"`, `"legal_name": "Grace B. Hopper", "roles"`, 1)
	body = strings.Replace(body, `"city": "Berlin"`, `"line2": "Hof", "postal_code": "10115", "region": "BE", "city": "Berlin"`, 1)
	p, c := e.create(t, body)
	e.mustApply(t, id(c))
	got := e.blockPersonal(t, p)
	addr := got["addresses"].([]any)[0].(map[string]any)
	for _, v := range []any{got["legal_name"], addr["line2"], addr["postal_code"], addr["region"]} {
		if v != partner.MaskedText {
			t.Errorf("masked: got %v, want %s (partner %v)", v, partner.MaskedText, got)
		}
	}
	open := uuid.MustParse(id(e.active(t, "Open", 0)))
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.BlockPersonalData(ctx, openItems{e.le}, failingEntities{}, open, func(int32) error { return nil }, "r")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "tenancy down") {
		t.Errorf("tenancy not answering: got %v, want its error", err)
	}
}

// TestBlockWaitsForChange (T100b, ADR-0008): a change requested while a
// block is asked for holds the partner, so the block waits for it and
// then finds it pending (409).
func TestBlockWaitsForChange(t *testing.T) {
	e := privacyEnv(t)
	a := e.active(t, "Ada Lovelace", 0)
	pid := uuid.MustParse(id(a))
	h := endpoints()
	held, release := make(chan struct{}), make(chan struct{})
	requested := make(chan error, 1)
	letGo := sync.OnceFunc(func() { close(release) })
	defer letGo()
	hold := sync.OnceFunc(func() { close(held) })
	go func() {
		err := errors.New("transaction setup failed")
		defer func() { hold(); requested <- err }() // the main goroutine never waits on a body that did not run
		err = apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := h.CreatePartnerChange(ctx, &gen.PartnerChangeCreate{PartnerID: pid, Kind: "update", Reason: "r",
				Payload: gen.BusinessPartnerChanges{Name: gen.NewOptString("Ada King")}}, gen.CreatePartnerChangeParams{})
			hold()
			<-release
			return err
		})
	}()
	<-held
	blocked := make(chan error, 1)
	go func() {
		blocked <- apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) error {
			_, err := partner.BlockPersonalData(ctx, openItems{}, tenancy.Directory{}, pid, func(int32) error { return nil }, "r")
			return err
		})
	}()
	admin := pgtest.Admin(t)
	// The partner's read is matched by its start: pg_stat_activity keeps
	// the first kilobyte of a statement, before its FOR UPDATE.
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE NOT l.granted AND a.query LIKE 'SELECT p.id, p.number, p.kind%'`).Scan(&n); err != nil {
			t.Fatalf("read the lock waits: %v", err)
		}
		return n > 0
	}, "the block did not wait for the change request")
	letGo()
	if err := <-requested; err != nil {
		t.Fatal(err)
	}
	if p := problem.From(<-blocked); p.Status != 409 || p.Code != "partner.change_pending" {
		t.Errorf("the block: got %v, want 409 partner.change_pending", p)
	}
}

// checkMaskedOrder lists the blocked partner a and z by name one a page,
// as a caller who reads a masked: the order and the page token are of the
// masked name, so the token, written after a on the first page, carries
// no name of a, and the pages hold both.
func checkMaskedOrder(t *testing.T, e env, a, z map[string]any) {
	t.Helper()
	path := "/v1/business-partners?order_by=name&page_size=1&ids=" + id(a) + "," + id(z)
	first, _ := e.Must(200, "GET", path, "", "")
	token, _ := first["next_page_token"].(string)
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil || strings.Contains(string(payload), "Ada") {
		t.Errorf("the page token: got %q (%v), want one without the blocked name", payload, err)
	}
	second, _ := e.Must(200, "GET", path+"&page_token="+token, "", "")
	got := first["items"].([]any)[0].(map[string]any)["name"].(string) + ", " + second["items"].([]any)[0].(map[string]any)["name"].(string)
	if want := partner.MaskedText + ", zara Moss"; got != want {
		t.Errorf("by name: got %s, want %s", got, want)
	}
}
