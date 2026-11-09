package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// formTiny is tiny with an open invoice of vendor 0000000001 in each of
// JPY, KWD, CAD and AED, company 2000 being in EUR: JPY 100,000 (EUR
// 600.00), KWD 1.234 (EUR 3.70), CAD 100.00 (EUR 67.50) and AED 100.00
// (EUR 25.00). In SAP's internal form, with TCURX listing JPY (0
// decimals) and KWD (3), the yen are written "1000.00" and the dinars
// "12.34"; in external form as they are.
func formTiny(internal bool) *sap.Extract {
	x := tiny()
	docs := []struct{ belnr, currency, hsl, internal, external string }{
		{"0100000040", "JPY", "600.00", "1000.00", "100000"},
		{"0100000041", "KWD", "3.70", "12.34", "1.234"},
		{"0100000042", "CAD", "67.50", "100.00", "100.00"},
		{"0100000043", "AED", "25.00", "100.00", "100.00"},
	}
	for _, d := range docs {
		wsl := d.external
		if internal {
			wsl = d.internal
		}
		expense := fxLine(d.belnr, "000001", "0000520000", d.hsl, wsl, "20260302", "RE")
		vendor := vendorLine(d.belnr, "000002", "-"+d.hsl, "-"+wsl, "20260302", "RE", "")
		expense.RWCUR, vendor.RWCUR = d.currency, d.currency
		x.ACDOCA = append(x.ACDOCA, expense, vendor)
	}
	if internal {
		x.TCURX = []sap.TCURX{{CURRKEY: "JPY", CURRDEC: "0"}, {CURRKEY: "KWD", CURRDEC: "3"}, {CURRKEY: "BHD", CURRDEC: "3"}}
	}
	return x
}

// formConnector is a sap_file connector of the extract with the given
// amount_format, none when empty.
func (e env) formConnector(t *testing.T, path, format string) string {
	t.Helper()
	setting := ""
	if format != "" {
		setting = fmt.Sprintf(`, "amount_format": %q`, format)
	}
	c, _ := e.Must(201, "POST", "/v1/connectors", fmt.Sprintf(`{"kind": "sap_file", "name": %q, "settings": {"path": %q%s}}`, "SAP "+path+format, path, setting), "")
	return c["id"].(string)
}

// TestAmountForms (po:2026-11-09, D10): the extract's amounts load as the
// amounts SAP holds whether the export wrote SAP's internal form, which
// the run converts with the extract's TCURX, or the external form the
// connector names: the AP clerk sees the open items as JPY 100,000, KWD
// 1.234, CAD 100.00 and AED 100.00, and the reconciliation agrees with
// SAP in each currency.
func TestAmountForms(t *testing.T) {
	for _, c := range []struct {
		name, format string
		internal     bool
	}{{"internal by default", "", true}, {"internal named", "internal", true}, {"external", "external", false}} {
		t.Run(c.name, func(t *testing.T) {
			e := dpSetup(t)
			write(t, e.dir, "form.json", formTiny(c.internal))
			run := e.run(t, e.formConnector(t, "form.json", c.format), "documents", false, e.DE.String())
			if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
				t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
			}
			_, got := e.act(t, run, "reconcile")
			if got["status"] != "reconciled" {
				t.Fatalf("reconcile: got %v, want reconciled: %v", got["status"], got["report"])
			}
			report := got["report"].(map[string]any)
			for _, want := range []string{"2000 Open items in JPY total -100000 JPY -100000 JPY true", "2000 Open items in KWD total -1.234 KWD -1.234 KWD true",
				"2000 Open items in CAD total -100.00 CAD -100.00 CAD true", "2000 Open items in AED total -100.00 AED -100.00 AED true",
				"2000 Open payables total"} {
				if figures := checkFigures(report); !strings.Contains(figures, want) {
					t.Errorf("checks: got %s, want %s among them", figures, want)
				}
			}
			items, _ := e.Must(200, "GET", "/v1/open-items?partner_id="+supplierOf(t, e, "0000000001").String(), "", "")
			var open []string
			for _, it := range items["items"].([]any) {
				m := it.(map[string]any)
				a := m["amount"].(map[string]any)
				open = append(open, fmt.Sprint(a["amount"], " ", a["currency"], " ", m["amount_lc"].(map[string]any)["amount"]))
			}
			slices.Sort(open)
			if got, want := strings.Join(open, ", "), "-1.234 KWD -3.70, -100.00 AED -25.00, -100.00 CAD -67.50, -100000 JPY -600.00"; got != want {
				t.Errorf("the supplier's open items (document, company currency): got %s, want %s", got, want)
			}
		})
	}
}

// checkFigures is checks with each check's SAP and platform figures.
func checkFigures(report map[string]any) string {
	var out []string
	for _, c := range report["checks"].([]any) {
		m := c.(map[string]any)
		out = append(out, fmt.Sprintf("%s %s %s %s %v", m["company_code"], m["name"], m["sap"], m["platform"], m["pass"]))
	}
	return strings.Join(out, "; ")
}

// TestAmountFormRefusals: the quality gate never guesses an amount's form.
// An extract in internal form with no TCURX, or one that does not list a
// currency of other than two decimals, is refused naming those
// currencies, as is each amount in them; an amount with more decimals than
// its form allows is refused; and nothing is loaded.
func TestAmountFormRefusals(t *testing.T) {
	noTCURX := formTiny(true)
	noTCURX.TCURX = nil
	noKWD := formTiny(true)
	noKWD.TCURX = noKWD.TCURX[:1]
	fineYen := formTiny(true)
	fineYen.ACDOCA[len(tiny().ACDOCA)].WSL = "1000.005"
	fineYenExternal := formTiny(false)
	fineYenExternal.ACDOCA[len(tiny().ACDOCA)].WSL = "100000.5"
	badTCURX := formTiny(true)
	badTCURX.TCURX = append(badTCURX.TCURX, sap.TCURX{CURRKEY: "KWD", CURRDEC: "2"}, sap.TCURX{CURRKEY: "OMR", CURRDEC: "x"})
	for name, c := range map[string]struct {
		x       *sap.Extract
		format  string
		message string
		keys    []string
	}{
		"no TCURX": {noTCURX, "", "it holds no TCURX to convert the amounts in JPY (0 decimals), KWD (3 decimals)",
			[]string{"", "2000/2026/0100000040/000001", "2000/2026/0100000040/000002", "2000/2026/0100000041/000001", "2000/2026/0100000041/000002"}},
		"currency not in TCURX": {noKWD, "", "its TCURX does not list the amounts in KWD (3 decimals)",
			[]string{"", "2000/2026/0100000041/000001", "2000/2026/0100000041/000002"}},
		"internal yen with decimals": {fineYen, "", `amount "1000.005" in SAP's internal form of two decimals`,
			[]string{"2000/2026/0100000040/000001"}},
		"external yen with decimals": {fineYenExternal, "external", `"100000.5" has more than 0 significant decimals`,
			[]string{"2000/2026/0100000040/000001"}},
		"unusable TCURX rows": {badTCURX, "", "currency KWD is listed with 3 and with 2 decimals",
			[]string{"", "2000/2026/0100000041/000001", "2000/2026/0100000041/000002", "KWD", "OMR"}},
	} {
		t.Run(name, func(t *testing.T) {
			e := dpSetup(t)
			write(t, e.dir, "form.json", c.x)
			_, got := e.act(t, e.run(t, e.formConnector(t, "form.json", c.format), "documents", false, e.DE.String()), "start")
			if got["status"] != "failed" {
				t.Fatalf("status: got %v, want failed: %v", got["status"], got["report"])
			}
			keys := qualityErrors(got)
			slices.Sort(keys)
			if want := slices.Sorted(slices.Values(c.keys)); !slices.Equal(keys, want) {
				t.Errorf("refused: got %v, want %v", keys, want)
			}
			if messages := fmt.Sprint(got["report"].(map[string]any)["quality"]); !strings.Contains(messages, c.message) {
				t.Errorf("quality: got %s, want %q among it", messages, c.message)
			}
			if n := count(t, `SELECT count(*) FROM integration.external_ref WHERE tenant_id = $1`, e.Tenant.ID); n != 0 {
				t.Errorf("loaded references: got %d, want 0", n)
			}
		})
	}
}

// TestAmountFormSettings: amount_format is internal or external, and only
// a sap_file connector takes it, as OData delivers external amounts.
func TestAmountFormSettings(t *testing.T) {
	e := setup(t)
	for name, body := range map[string]string{
		"unknown form":  `{"kind": "sap_file", "name": "x", "settings": {"path": "a.json", "amount_format": "shifted"}}`,
		"not a string":  `{"kind": "sap_file", "name": "x", "settings": {"path": "a.json", "amount_format": 2}}`,
		"OData's input": `{"kind": "sap_odata", "name": "x", "settings": {"base_url": "https://x", "amount_format": "external"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e.Problem(422, "integration.invalid_settings", "POST", "/v1/connectors", body, "")
		})
	}
}
