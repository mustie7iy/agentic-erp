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
