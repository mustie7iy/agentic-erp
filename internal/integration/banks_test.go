package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// canadianTiny is tiny with its vendor's bank account in Canada: the bank
// key the routing as 0 + institution + transit, the account number
// written with a dash.
func canadianTiny(bankKey, number string) *sap.Extract {
	x := tiny()
	x.LFBK = []sap.LFBK{{LIFNR: "0000000001", BANKS: "CA", BANKL: bankKey, BANKN: number}}
	return x
}

// TestCanadianBankAccounts: a vendor's Canadian bank account (LFBK BANKS
// CA) loads as scheme ca with the institution and transit numbers of its
// bank key (T72d); the quality gate refuses a bank key or account number
// partner would, and the reconciliation compares each vendor's account.
func TestCanadianBankAccounts(t *testing.T) {
	e := setup(t)

	t.Run("malformed values fail the quality gate", func(t *testing.T) {
		write(t, e.dir, "ca-bad.json", canadianTiny("00312345", "98-76"))
		_, got := e.act(t, e.run(t, e.connector(t, "ca-bad.json"), "documents", false, e.DE.String()), "start")
		if got["status"] != "failed" {
			t.Fatalf("run status: got %v, want failed: %v", got["status"], got["report"])
		}
		quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
		for _, want := range []string{`key:0000000001 message:bank key "00312345" of the Canadian bank account is not a routing number`,
			"key:0000000001 message:the Canadian bank account number is not 7-12 digits severity:error table:LFBK"} {
			if !strings.Contains(quality, want) {
				t.Errorf("quality gate: got %s, want it to report %q", quality, want)
			}
		}
		if strings.Contains(quality, "98") {
			t.Errorf("quality gate: got %s, want the account number left out", quality)
		}
	})

	write(t, e.dir, "ca.json", canadianTiny("0003-00002", "123-4567"))
	c := e.connector(t, "ca.json")
	run := e.run(t, c, "documents", false, e.DE.String())
	t.Run("the account loads as scheme ca", func(t *testing.T) {
		if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
			t.Fatalf("run status: got %v, want loaded: %v", got["status"], got["report"])
		}
		var scheme, country, institution, transit, last4 string
		if err := pgtest.Admin(t).QueryRow(context.Background(), `SELECT account_scheme, bank_country_code, institution_number, transit_number, account_last4
			FROM partner.bank_account WHERE tenant_id = $1`, e.Tenant.ID).Scan(&scheme, &country, &institution, &transit, &last4); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join([]string{scheme, country, institution, transit, last4}, " "), "ca CA 003 00002 4567"; got != want {
			t.Errorf("bank account: got %s, want %s", got, want)
		}
		if _, got := e.act(t, run, "reconcile"); got["status"] != "reconciled" {
			t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], checks(got["report"].(map[string]any)))
		} else if cs := checks(got["report"].(map[string]any)); !strings.Contains(cs, "2000 Vendor bank accounts: vendors with differences true") {
			t.Errorf("reconcile checks: got %s, want the bank accounts passed", cs)
		}
	})

	t.Run("an account that differs from SAP's fails the reconciliation", func(t *testing.T) {
		write(t, e.dir, "ca.json", canadianTiny("000300003", "7654321"))
		again := e.run(t, c, "documents", false, e.DE.String())
		e.act(t, again, "start")
		_, got := e.act(t, again, "reconcile")
		report := got["report"].(map[string]any)
		if report["passed"] != false {
			t.Fatalf("reconcile passed: got %v, want false: %s", report["passed"], checks(report))
		}
		if cs := checks(report); !strings.Contains(cs, "2000 Vendor bank accounts: vendors with differences false") {
			t.Errorf("reconcile checks: got %s, want the bank accounts failed", cs)
		}
		want := "map[company_code:2000 key:bank account of vendor 1 platform:ca CA 003-00002 ending 4567 sap:ca CA 003-00003 ending 4321]"
		if diffs := fmt.Sprint(report["diffs"]); !strings.Contains(diffs, want) {
			t.Errorf("diffs: got %s, want %s", diffs, want)
		}
	})
}
