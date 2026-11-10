package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestNotedItemsAreNotMigrated: SAP's noted items (ACDOCA BSTAT S, such as
// a down payment request, special G/L F) post nothing to the G/L, so a run
// loads no journal entry for one and counts none as an open item
// (review:batch63; the SAP specialist, po:2026-11-09, A14, A15). The mock
// extract with an open and a cleared request added loads and reconciles
// exactly as without them, in both modes, and its report's quality part
// says what was left out: the count per company and each open one. Before,
// a one-sided request was refused as unbalanced and as a special G/L item
// that is not migrated, and a noted document that balanced was posted as a
// journal entry with an open payable.
func TestNotedItemsAreNotMigrated(t *testing.T) {
	for _, mode := range []string{"documents", "balances"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t)
			x := sap.GenerateMock(42, "2026")
			vendor, name := x.LFA1[0].LIFNR, x.LFA1[0].NAME1 // the mock's first vendor is set up in company 1000
			request := func(belnr, augbl string) sap.ACDOCA {
				return sap.ACDOCA{RLDNR: "0L", RBUKRS: "1000", GJAHR: "2026", BELNR: belnr, DOCLN: "000001", RACCT: "0000211000", HSL: "-500.00", RWCUR: "USD",
					WSL: "-500.00", BUDAT: "20260315", BLART: "KA", LIFNR: vendor, UMSKZ: "F", BSTAT: "S", AUGBL: augbl}
			}
			x.ACDOCA = append(x.ACDOCA, request("1900000002", "1500000099"), request("1900000001", ""))
			// A noted document that balances, from an extract without the
			// special G/L indicator: nothing else would stop it posting.
			balanced := request("1900000003", "")
			balanced.UMSKZ, balanced.BLART = "", "KR"
			offset := balanced
			offset.DOCLN, offset.RACCT, offset.HSL, offset.WSL, offset.LIFNR = "000002", "0000610000", "500.00", "500.00", ""
			x.ACDOCA = append(x.ACDOCA, balanced, offset)
			write(t, e.dir, "noted.json", x)
			_, without := e.act(t, e.run(t, e.connector(t, "extract.json"), mode, true, e.US.String()), "start")
			_, with := e.act(t, e.run(t, e.connector(t, "noted.json"), mode, true, e.US.String()), "start")
			if with["status"] != "reconciled" {
				t.Fatalf("status with noted items: got %v, want reconciled: %v", with["status"], with["report"])
			}
			want, got := without["report"].(map[string]any), with["report"].(map[string]any)
			for _, part := range []string{"extract", "load", "loaded", "checks", "diffs"} {
				if fmt.Sprint(got[part]) != fmt.Sprint(want[part]) {
					t.Errorf("report %s with noted items: got %v, want %v, as without them", part, got[part], want[part])
				}
			}
			findings := func(report map[string]any) []string {
				var out []string
				for _, i := range report["quality"].([]any) {
					m := i.(map[string]any)
					out = append(out, fmt.Sprintf("%s %s %s: %s", m["severity"], m["table"], m["key"], m["message"]))
				}
				return out
			}
			var added []string
			for _, f := range findings(got) {
				if !slices.Contains(findings(want), f) {
					added = append(added, f)
				}
			}
			if n, base := len(findings(got)), len(findings(want)); n != base+len(added) {
				t.Errorf("quality findings: got %d, want the %d without noted items and those added", n, base)
			}
			item := "open noted item (BSTAT S) of vendor " + vendor + " " + fmt.Sprintf("%q", name)
			const advice = " is not migrated; if it is still wanted, enter it on the platform (a down payment request is a payables document, not a journal entry)"
			wantAdded := []string{
				"warning ACDOCA 1000: noted items (BSTAT S, such as down payment requests) of company 1000 are not migrated: lines 4, documents 3, " +
					"open in SAP (AUGBL blank; each listed) 3. SAP posts nothing to the G/L for a noted item, so it loads as no journal entry and counts as " +
					"no open item",
				"warning ACDOCA 1000/2026/1900000001/000001: " + item + ` with special G/L indicator "F" posted on 20260315 over -500.00 USD` + advice,
				"warning ACDOCA 1000/2026/1900000003/000001: " + item + " posted on 20260315 over -500.00 USD" + advice,
				"warning ACDOCA 1000/2026/1900000003/000002: open noted item (BSTAT S) posted on 20260315 over 500.00 USD" + advice,
			}
			if strings.Join(added, "\n") != strings.Join(wantAdded, "\n") {
				t.Errorf("quality findings added by the noted items:\ngot  %s\nwant %s", strings.Join(added, "\n"), strings.Join(wantAdded, "\n"))
			}
		})
	}
}
