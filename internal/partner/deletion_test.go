package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// blockedAt is when the partner's personal data was blocked.
func blockedAt(t *testing.T, p map[string]any) time.Time {
	t.Helper()
	var at time.Time
	pgtest.Scan(t, `SELECT personal_data_blocked_at FROM partner.business_partner WHERE id = $1`, []any{id(p)}, &at)
	return at
}

// blank blanks the partner as a deletion run does, committed, and
// reports whether it did.
func (e env) blank(t *testing.T, p map[string]any, at time.Time) bool {
	t.Helper()
	var did bool
	if err := apitest.Committed(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		did, err = partner.Privacy{Keys: apitest.FieldKeys(), OpenItems: openItems{}, Settings: configuration.Settings{}}.Blank(ctx, uuid.MustParse(id(p)), at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return did
}

// TestBlank (T100c, ADR-0079 §12): blanking replaces the partner's names,
// addresses, bank accounts (ended), tax identifiers and its changes'
// payloads by the markers, and its name where another change lists it as
// a possible duplicate; it reads so to every caller, its block can no
// longer be lifted, and blanking again, or a block other than the one
// the run saw, changes nothing.
func TestBlank(t *testing.T) {
	e := privacyEnv(t)
	body := strings.Replace(e.supplier("Zelda Fitzgerald", 0), `"roles"`, `"legal_name": "Zelda Sayre", "roles"`, 1)
	body = strings.Replace(body, `"city": "Berlin"`, `"line2": "Hof", "postal_code": "10115", "region": "BE", "city": "Berlin"`, 1)
	z, c := e.create(t, body)
	e.mustApply(t, id(c))
	_, other := e.create(t, e.supplier("Other", 0)) // lists Zelda as a possible duplicate
	e.blockPersonal(t, z)
	at := blockedAt(t, z)
	if e.blank(t, z, at.Add(time.Second)) {
		t.Error("a block other than the run's: got blanked, want left")
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		did, err := partner.Privacy{Keys: apitest.FieldKeys(), OpenItems: openItems{e.le}, Settings: configuration.Settings{}}.Blank(ctx, uuid.MustParse(id(z)), at)
		if did {
			t.Error("with an open item: got blanked, want left")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := partner.Privacy{Keys: apitest.FieldKeys(), OpenItems: failingItems{}, Settings: configuration.Settings{}}.Blank(ctx, uuid.MustParse(id(z)), at)
		return err
	}); err == nil || !strings.Contains(err.Error(), "ledger down") {
		t.Errorf("a ledger that does not answer: got %v, want its error", err)
	}
	if !e.blank(t, z, at) {
		t.Fatal("blanked: got false")
	}
	if e.blank(t, z, at) {
		t.Error("blanked again: got true, want nothing done")
	}
	got, tag := e.Must(200, "GET", "/v1/business-partners/"+id(z), "", "")
	addr := got["addresses"].([]any)[0].(map[string]any)
	tax := got["tax_identifiers"].([]any)[0].(map[string]any)
	if fmt.Sprint(got["name"], got["legal_name"], addr["line1"], addr["line2"], addr["city"], addr["postal_code"], addr["region"], tax["value_last4"]) !=
		strings.Repeat(partner.DeletedText, 7)+partner.DeletedLast4 || got["personal_data_deleted_at"] == nil {
		t.Errorf("the partner, blanked: got %v", got)
	}
	banks, _ := e.Must(200, "GET", "/v1/business-partners/"+id(z)+"/bank-accounts", "", "")
	bank := banks["items"].([]any)[0].(map[string]any)
	if bank["holder_name"] != partner.DeletedText || bank["account_last4"] != partner.DeletedLast4 || bank["bic"] != nil || bank["valid_to"] == nil {
		t.Errorf("the bank account: got %v, want blanked and ended", bank)
	}
	changes, _ := e.Must(200, "GET", "/v1/partner-changes?partner_id="+id(z)+"&kind=create", "", "")
	payload := fmt.Sprint(changes["items"].([]any)[0].(map[string]any)["payload"])
	if strings.Contains(payload, "Zelda") || strings.Contains(payload, "Hauptstrasse") || !strings.Contains(payload, partner.DeletedText) {
		t.Errorf("the create change's payload: got %s, want blanked", payload)
	}
	var sealed int
	pgtest.Scan(t, `SELECT count(*) FROM partner.partner_change WHERE partner_id = $1 AND payload::text LIKE '%cipher%'`, []any{id(z)}, &sealed)
	if sealed != 0 {
		t.Errorf("sealed values left in the payloads: %d changes", sealed)
	}
	dups, _ := e.Must(200, "GET", "/v1/partner-changes/"+id(other), "", "")
	for _, d := range dups["possible_duplicates"].([]any) {
		if d := d.(map[string]any); d["partner_id"] == id(z) && d["name"] != partner.DeletedText {
			t.Errorf("listed as a possible duplicate: got %v, want its name %s", d, partner.DeletedText)
		}
	}
	e.Problem(409, "partner.personal_data_deleted", "POST", "/v1/business-partners/"+id(z)+":unblockPersonalData", `{"reason": "r"}`, tag)
}

// TestBlankCanadian (T100c): a Canadian account's institution and transit
// numbers, which its check requires, read zeros; an account that ended
// already keeps its end.
func TestBlankCanadian(t *testing.T) {
	e := privacyEnv(t)
	body := strings.Replace(e.supplier("Maple Person", 0), `"account_number": "de89 3704 0044 0532 0130 00", "account_scheme": "iban", "bic": "COBADEFFXXX",
			"bank_country_code": "DE"`, `"account_number": "7000123", "account_scheme": "ca", "institution_number": "003", "transit_number": "00002",
			"bank_country_code": "CA"`, 1)
	p, c := e.create(t, body)
	e.mustApply(t, id(c))
	e.blockPersonal(t, p)
	pgtest.Unguarded(t, `UPDATE partner.bank_account SET valid_to = valid_from + 1, ended_by = $2 WHERE partner_id = $1`, id(p), e.ten.Human.ID)
	var before time.Time
	pgtest.Scan(t, `SELECT valid_to FROM partner.bank_account WHERE partner_id = $1`, []any{id(p)}, &before)
	if !e.blank(t, p, blockedAt(t, p)) {
		t.Fatal("blanked: got false")
	}
	var institution, transit string
	var after time.Time
	pgtest.Scan(t, `SELECT institution_number, transit_number, valid_to FROM partner.bank_account WHERE partner_id = $1`, []any{id(p)}, &institution, &transit, &after)
	if institution != "000" || transit != "00000" || !after.Equal(before) {
		t.Errorf("the Canadian account: got %s %s ending %s, want 000 00000 ending %s", institution, transit, after, before)
	}
}

// TestBlocked (T100c): the partners whose personal data is blocked and
// not deleted, by number, with the companies they have data in.
func TestBlocked(t *testing.T) {
	e := privacyEnv(t)
	a, b := e.active(t, "A", 0), e.active(t, "B", 0)
	e.blockPersonal(t, a)
	e.blockPersonal(t, b)
	e.blank(t, b, blockedAt(t, b))
	var got []partner.BlockedSubject
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) (err error) {
		got, err = partner.Privacy{}.Blocked(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID.String() != id(a) || got[0].Number != a["number"] || len(got[0].Companies) != 1 || got[0].Companies[0] != e.le {
		t.Errorf("got %+v, want A in company 1000", got)
	}
}

// TestBlankFailures: blanking and the blocked partners' read with each
// statement failing in turn fail, as does a partner that does not exist
// and a call outside a unit of work.
func TestBlankFailures(t *testing.T) {
	e := privacyEnv(t)
	a := e.active(t, "A", 0)
	_, _ = e.create(t, e.supplier("Dup", 0))
	e.blockPersonal(t, a)
	at, pid := blockedAt(t, a), uuid.MustParse(id(a))
	y := partner.Privacy{Keys: apitest.FieldKeys(), OpenItems: openItems{}, Settings: configuration.Settings{}}
	calls := map[string]func(ctx context.Context) error{
		"blank": func(ctx context.Context) error { _, err := y.Blank(ctx, pid, at); return err },
		"blocked": func(ctx context.Context) error {
			_, err := y.Blocked(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
	}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error { _, err := y.Blank(ctx, uuid.New(), at); return err }); err == nil {
		t.Error("an unknown partner: got no error, want one")
	}
}
