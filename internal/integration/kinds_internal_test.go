package integration

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// TestDocumentKind: RBKP-XRECH and RSEG-TBTKZ give a parked document's
// kind, and anything else is refused rather than guessed (T72b).
func TestDocumentKind(t *testing.T) {
	item := func(tbtkz string) sap.RSEG { return sap.RSEG{BUZEI: "000001", TBTKZ: tbtkz} }
	cases := []struct {
		name     string
		xrech    *string
		items    []sap.RSEG
		kind     string
		severity string
	}{
		{"invoice", sap.Indicator("X"), []sap.RSEG{item("")}, "invoice", ""},
		{"invoice without items", sap.Indicator("X"), nil, "invoice", ""},
		{"credit memo", sap.Indicator(""), []sap.RSEG{item(""), item("")}, "credit_memo", ""},
		{"subsequent debit", sap.Indicator("X"), []sap.RSEG{item("X"), item("X")}, "subsequent_debit", ""},
		{"subsequent credit", sap.Indicator(""), []sap.RSEG{item("X")}, "subsequent_credit", ""},
		{"mixed items", sap.Indicator("X"), []sap.RSEG{item("X"), item("")}, "", SevWarning},
		{"no XRECH", nil, []sap.RSEG{item("")}, "", SevError},
		{"unknown XRECH", sap.Indicator("."), []sap.RSEG{item("")}, "", SevError},
		{"unknown TBTKZ", sap.Indicator("X"), []sap.RSEG{item("Y")}, "", SevError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, issue := documentKind(sap.RBKP{BELNR: "5100000001", GJAHR: "2026", XRECH: c.xrech}, c.items)
			if kind != c.kind {
				t.Errorf("kind: got %q, want %q", kind, c.kind)
			}
			severity := ""
			if issue != nil {
				severity = issue.Severity
				if issue.Table != "RBKP" || issue.Key != "5100000001/2026" {
					t.Errorf("issue row: got %s %s, want RBKP 5100000001/2026", issue.Table, issue.Key)
				}
			}
			if severity != c.severity {
				t.Errorf("issue severity: got %q, want %q", severity, c.severity)
			}
		})
	}
}
