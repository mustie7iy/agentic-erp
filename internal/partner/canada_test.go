package partner_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// TestCanadianBankAccounts: a Canadian account (scheme ca, T45d) is its
// institution, transit and account numbers, checked by digits and length;
// it goes through the bank change and its approval as an IBAN does, shows
// and publishes only its last four account digits beside the bank's
// routing, and is opened with that routing for a bank file.
func TestCanadianBankAccounts(t *testing.T) {
	e := setup(t)
	pid := id(e.active(t, "Maple", 0))
	path := "/v1/business-partners/" + pid + "/bank-accounts"
	body := func(scheme, number, routing string) string {
		return fmt.Sprintf(`{"account_number": %q, "account_scheme": %q, "bank_country_code": "CA", "holder_name": "Maple Supply Ltd",
			"valid_from": %q, "reason": "Confirmed by call-back"%s}`, number, scheme, e.date(2), routing)
	}
	routing := func(institution, transit string) string {
		return fmt.Sprintf(`, "institution_number": %q, "transit_number": %q`, institution, transit)
	}
	for _, c := range []struct{ name, body, code string }{
		{"no institution", body("ca", "1234567", `, "transit_number": "12345"`), "partner.invalid_institution_number"},
		{"no transit", body("ca", "1234567", `, "institution_number": "003"`), "partner.invalid_transit_number"},
		{"short account", body("ca", "123456", routing("003", "12345")), "partner.invalid_account_number"},
		{"letters in the account", body("ca", "12345AB", routing("003", "12345")), "partner.invalid_account_number"},
		{"routing on an IBAN", body("iban", iban, routing("003", "12345")), "partner.routing_not_allowed"},
		{"routing on a domestic account", body("domestic", "12345678", `, "transit_number": "12345"`), "partner.routing_not_allowed"},
		{"bank outside Canada", strings.Replace(body("ca", "1234567", routing("003", "12345")), `"CA"`, `"DE"`, 1), "partner.bank_country_mismatch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.in(t).Problem(422, c.code, "POST", path, c.body, "")
		})
	}
	c, _ := e.Must(201, "POST", path, body("ca", "12-3456 7890", routing("003", "12345")), "")
	e.mustApply(t, id(c))
	all, _ := e.Must(200, "GET", path, "", "")
	items := all["items"].([]any)
	got := items[len(items)-1].(map[string]any)
	for field, want := range map[string]any{"account_scheme": "ca", "account_last4": "7890", "institution_number": "003", "transit_number": "12345"} {
		if got[field] != want {
			t.Errorf("account %s: got %v, want %v", field, got[field], want)
		}
	}
	if _, ok := items[0].(map[string]any)["institution_number"]; ok {
		t.Error("IBAN account institution_number: got one, want none")
	}
	changed := e.events(t, "bank_account_changed")
	last := changed[len(changed)-1]
	for field, want := range map[string]any{"account_scheme": "ca", "account_last4": "7890", "institution_number": "003", "transit_number": "12345"} {
		if last[field] != want {
			t.Errorf("event %s: got %v, want %v", field, last[field], want)
		}
	}
	if s := fmt.Sprint(last); strings.Contains(s, "1234567890") {
		t.Errorf("event: got the full account number in %v, want it masked", s)
	}
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		account := uuid.MustParse(id(got))
		details, err := partner.Payees{Keys: apitest.FieldKeys()}.BankDetails(ctx, []uuid.UUID{account})
		if err != nil {
			return err
		}
		want := partner.BankDetail{Holder: "Maple Supply Ltd", Account: "1234567890", Institution: "003", Transit: "12345"}
		if d := details[account]; d != want {
			t.Errorf("bank details: got %+v, want %+v", d, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fingerprint is of the sealed number alone, as key rotation
	// recomputes it, so the same account on another partner is warned
	// about before and after a rotation.
	var print string
	if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT account_fingerprint FROM partner.bank_account WHERE id = $1`,
		uuid.MustParse(id(got))).Scan(&print); err != nil {
		t.Fatal(err)
	}
	if want := apitest.Fingerprint(t, e.ten.ID, "1234567890"); print != want {
		t.Errorf("fingerprint: got %s, want the account number's %s", print, want)
	}
	other := id(e.active(t, "Copy", 0))
	dup, _ := e.Must(201, "POST", "/v1/business-partners/"+other+"/bank-accounts", body("ca", "1234567890", routing("003", "12345")), "")
	warnings := fmt.Sprint(dup["warnings"])
	if !strings.Contains(warnings, "partner.duplicate_bank_account") {
		t.Errorf("same account on another partner: got warnings %s, want partner.duplicate_bank_account", warnings)
	}
}

// TestPaymentAccounts: the reconciliation of a migration reads each
// partner's payments account valid today by scheme, bank and last four
// (T72d); a partner whose account starts later has none yet.
func TestPaymentAccounts(t *testing.T) {
	e := setup(t)
	now, later := uuid.MustParse(id(e.active(t, "Now", 0))), uuid.MustParse(id(e.active(t, "Later", 3)))
	err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		got, err := partner.Suppliers{}.PaymentAccounts(ctx, []uuid.UUID{now, later})
		if err != nil {
			return err
		}
		if want := (partner.PaymentAccount{Scheme: "iban", BankCountryCode: "DE", Last4: "3000"}); got[now] != want {
			t.Errorf("account valid today: got %+v, want %+v", got[now], want)
		}
		if a, ok := got[later]; ok {
			t.Errorf("account from a later day: got %+v, want none", a)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
