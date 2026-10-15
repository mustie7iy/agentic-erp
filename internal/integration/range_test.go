package integration_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// A purchase order line whose net value is out of the range of an amount
// fails the quality gate: reconciliation adds SAP's line values, which
// wrapped before.
func TestQualityGateRefusesLineOutOfRange(t *testing.T) {
	e := setup(t)
	x := tiny()
	x.EKPO = append(x.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MATNR: "MAT-2", MENGE: "2", NETPR: "92233720368547758.07", PEINH: "1",
		SAKTO: "0000520000"})
	write(t, e.dir, "range.json", x)
	run := e.run(t, e.connector(t, "range.json"), "documents", false, e.DE.String())
	_, got := e.act(t, run, "start")
	if quality := fmt.Sprint(got["report"].(map[string]any)["quality"]); !strings.Contains(quality, "is out of range") {
		t.Errorf("quality gate: got %s, want it to report the line out of range", quality)
	}
	if got["status"] != "failed" {
		t.Errorf("run status: got %v, want failed", got["status"])
	}
}

// A purchase order quantity has up to six decimals, as the quality gate
// (money.PositiveQuantity) and procurement allow. A line of 0.0005 at
// 1000.00 is worth 0.50: it loads and reconciles, where reading the
// quantity with three decimals refused it.
func TestSixDecimalQuantityLoadsAndReconciles(t *testing.T) {
	e := setup(t)
	x := tiny()
	x.EKPO = append(x.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MATNR: "MAT-2", MENGE: "0.0005", NETPR: "1000.00", PEINH: "1",
		SAKTO: "0000520000"})
	write(t, e.dir, "fine.json", x)
	run := e.run(t, e.connector(t, "fine.json"), "documents", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	if _, got := e.act(t, run, "reconcile"); got["status"] != "reconciled" {
		t.Errorf("reconcile status: got %v, want reconciled: %v", got["status"], got["report"])
	}
}

// Journal lines whose sums are out of range fail the quality gate: a
// document of 92233720368547758.07 twice and 0.02 wrapped to zero and
// counted as balanced, and the opening balances summed per account
// wrapped the same way and posted with their sign reversed.
func TestQualityGateRefusesJournalOutOfRange(t *testing.T) {
	e := setup(t)
	x := tiny()
	line := func(no, account, amount string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000009", DOCLN: no, RACCT: account, HSL: amount, BUDAT: "20260301", BLART: "SA"}
	}
	x.ACDOCA = append(x.ACDOCA, line("000001", "0000100000", "92233720368547758.07"), line("000002", "0000100000", "92233720368547758.07"),
		line("000003", "0000520000", "0.02"))
	write(t, e.dir, "journal.json", x)
	run := e.run(t, e.connector(t, "journal.json"), "documents", false, e.DE.String())
	_, got := e.act(t, run, "start")
	quality := fmt.Sprint(got["report"].(map[string]any)["quality"])
	for _, want := range []string{"its lines add up to more than an amount holds", "the journal lines of company 2000 add up to more than an amount holds"} {
		if !strings.Contains(quality, want) {
			t.Errorf("quality gate: got %s, want it to report %q", quality, want)
		}
	}
}

// A reconciliation run extracts again without the quality gate, so its
// sums are bounded only if the extract is unchanged since the load. Here
// it changed: a document of 92233720368547758.07 twice and 0.02 on the
// bank account wrapped to zero in the trial balance and the debits, and
// the reconciliation passed. It now fails, reporting the sums out of
// range the way the quality gate does.
func TestReconciliationRefusesSumsOutOfRange(t *testing.T) {
	e := setup(t)
	write(t, e.dir, "changing.json", tiny())
	run := e.run(t, e.connector(t, "changing.json"), "documents", false, e.DE.String())
	if _, got := e.act(t, run, "start"); got["status"] != "loaded" {
		t.Fatalf("start status: got %v, want loaded: %v", got["status"], got["report"])
	}
	x := tiny()
	line := func(no, amount string) sap.ACDOCA {
		return sap.ACDOCA{RLDNR: "0L", RBUKRS: "2000", GJAHR: "2026", BELNR: "0100000009", DOCLN: no, RACCT: "0000100000", HSL: amount, BUDAT: "20260301", BLART: "SA"}
	}
	x.ACDOCA = append(x.ACDOCA, line("000001", "92233720368547758.07"), line("000002", "92233720368547758.07"), line("000003", "0.02"),
		line("000004", "92233720368547758.08"))
	x.EKPO = append(x.EKPO, sap.EKPO{EBELN: "4500000001", EBELP: "00020", MATNR: "MAT-2", MENGE: "2", NETPR: "92233720368547758.07", PEINH: "1",
		SAKTO: "0000520000"}, sap.EKPO{EBELN: "4500000001", EBELP: "00030", MATNR: "MAT-3", MENGE: "abc", NETPR: "1.234,00", PEINH: "x",
		SAKTO: "0000520000"}, sap.EKPO{EBELN: "4500000001", EBELP: "00040", MATNR: "MAT-4", MENGE: "1.2345678", NETPR: "1.00", PEINH: "1",
		SAKTO: "0000520000"},
		// A space-padded fixed-width MENGE: the gate refuses it, as procurement would at load.
		sap.EKPO{EBELN: "4500000001", EBELP: "00050", MATNR: "MAT-5", MENGE: " 5.000", NETPR: "1.00", PEINH: "1", SAKTO: "0000520000"})
	write(t, e.dir, "changing.json", x)
	_, got := e.act(t, run, "reconcile")
	if got["status"] != "failed" {
		t.Errorf("reconcile status: got %v, want failed", got["status"])
	}
	issues := fmt.Sprint(got["report"].(map[string]any)["issues"])
	for _, want := range []string{"the lines of account 100000 add up to more than an amount holds", "the debits of company 2000 add up to more than an amount holds",
		"2000/2026/0100000009/000004", "net value of 2 at 92233720368547758.07 per 1 is out of range",
		`quantity "abc" is not a plain decimal`, `quantity " 5.000" is not a plain decimal`, "4500000001/00050", `price unit "x" is not a positive whole number`, "4500000001/00030", `quantity "1.2345678" has more than 6 significant decimals`} {
		if !strings.Contains(issues, want) {
			t.Errorf("reconciliation issues: got %s, want %q", issues, want)
		}
	}
}
