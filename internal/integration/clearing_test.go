package integration_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// beforePayment is tiny as SAP stood before the invoice was paid: the
// invoice's vendor line open, and no payment document.
func beforePayment() *sap.Extract {
	x := tiny()
	x.ACDOCA[1].AUGBL = ""
	x.ACDOCA = x.ACDOCA[:3]
	return x
}

// bothOpen is tiny before SAP cleared the invoice and the payment: both
// vendor lines open.
func bothOpen() *sap.Extract {
	x := tiny()
	x.ACDOCA[1].AUGBL, x.ACDOCA[3].AUGBL = "", ""
	return x
}

// clearedLater is tiny with the invoice and the payment cleared by a
// clearing document outside the extract.
func clearedLater() *sap.Extract {
	x := tiny()
	x.ACDOCA[1].AUGBL, x.ACDOCA[3].AUGBL = "0100000009", "0100000009"
	return x
}

// TestClearingAcrossRuns: a vendor line SAP cleared after an earlier run
// loaded it open is cleared on the platform by the later run, with the
// later run's document or with another earlier one (review:batch22), and
// the open payables agree with SAP's. The same extract again records
// nothing and publishes nothing.
func TestClearingAcrossRuns(t *testing.T) {
	for name, c := range map[string]struct {
		first, later *sap.Extract
		loaded       string
	}{
		"by the later run's payment":                 {beforePayment(), tiny(), "map[clearings:1 journal_entries:1]"},
		"by a clearing document outside the extract": {bothOpen(), clearedLater(), "map[clearings:1]"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			write(t, e.dir, "tiny.json", c.first)
			conn := e.connector(t, "tiny.json")
			first := e.run(t, conn, "documents", false, e.DE.String())
			if _, got := e.act(t, first, "start"); got["status"] != "loaded" {
				t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
			}
			write(t, e.dir, "tiny.json", c.later)
			delta := e.run(t, conn, "documents", false, e.DE.String())
			_, got := e.act(t, delta, "start")
			if got["status"] != "loaded" {
				t.Fatalf("delta run: got %v, want loaded: %v", got["status"], got["report"])
			}
			if loaded := fmt.Sprint(got["report"].(map[string]any)["loaded"]); loaded != c.loaded {
				t.Errorf("loaded by the delta run: got %s, want %s", loaded, c.loaded)
			}
			if n := count(t, `SELECT count(*) FROM ledger.clearing_item WHERE tenant_id = $1`, e.Tenant.ID); n != 2 {
				t.Errorf("cleared items: got %d, want the invoice's and the payment's vendor lines", n)
			}
			e.openPayablesAgree(t, delta)

			events := count(t, `SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1`, e.Tenant.ID)
			same := e.run(t, conn, "documents", false, e.DE.String())
			_, got = e.act(t, same, "start")
			if got["status"] != "loaded" {
				t.Fatalf("the same extract again: got %v, want loaded: %v", got["status"], got["report"])
			}
			if loaded := got["report"].(map[string]any)["loaded"].(map[string]any); len(loaded) != 0 {
				t.Errorf("loaded by the same extract again: got %v, want nothing", loaded)
			}
			if n := count(t, `SELECT count(*) FROM ledger.outbox WHERE tenant_id = $1`, e.Tenant.ID); n != events {
				t.Errorf("ledger events after the same extract again: got %d, want %d", n, events)
			}
			e.openPayablesAgree(t, same)
		})
	}
}

// TestClearingAcrossRunsRefused: a clearing whose item an earlier run
// loaded and the platform has cleared since (here by reversing the
// invoice) is refused, reported under the clearing, and the load rolled
// back, rather than the item cleared twice.
func TestClearingAcrossRunsRefused(t *testing.T) {
	e := setup(t)
	write(t, e.dir, "tiny.json", beforePayment())
	conn := e.connector(t, "tiny.json")
	first := e.run(t, conn, "documents", false, e.DE.String())
	if _, got := e.act(t, first, "start"); got["status"] != "loaded" {
		t.Fatalf("first run: got %v, want loaded: %v", got["status"], got["report"])
	}
	var invoice string
	pgtest.Scan(t, `SELECT entity_id::text FROM integration.external_ref WHERE tenant_id = $1 AND object = 'ACDOCA'
		AND external_key = '2000/2026/0100000001'`, []any{e.Tenant.ID}, &invoice)
	_, tag := e.Must(200, "GET", "/v1/journal-entries/"+invoice, "", "")
	e.Must(200, "POST", "/v1/journal-entries/"+invoice+":reverse", `{}`, tag)
	write(t, e.dir, "tiny.json", tiny())
	delta := e.run(t, conn, "documents", false, e.DE.String())
	_, got := e.act(t, delta, "start")
	if got["status"] != "failed" {
		t.Fatalf("delta run: got %v, want failed: %v", got["status"], got["report"])
	}
	if report := fmt.Sprint(got["report"]); !strings.Contains(report, "clearing 2000/0100000002") || !strings.Contains(report, "ledger.entry_reversed") {
		t.Errorf("delta run's refusal: got %s, want the clearing refused as reversed", report)
	}
}

// openPayablesAgree reconciles the run and requires its open payables
// checks to pass, the open items counted per vendor among them: a clearing
// left undone nets away in the sums.
func (e env) openPayablesAgree(t *testing.T, run map[string]any) {
	t.Helper()
	_, got := e.act(t, run, "reconcile")
	report := got["report"].(map[string]any)
	for _, check := range []string{"2000 Open payables total true", "2000 Open payables: vendors with differences true",
		"2000 Open payable items true", "2000 Open payable items: vendors with differences true"} {
		if !strings.Contains(checks(report), check) {
			t.Errorf("reconciliation: got %s, differences %v, want %q", checks(report), report["diffs"], check)
		}
	}
	if got["status"] != "reconciled" {
		t.Errorf("reconcile status: got %v, want reconciled: %v", got["status"], report)
	}
}
