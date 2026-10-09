package sapconfig

import (
	"slices"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

func notesText(ns []Note) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = n.String()
	}
	return strings.Join(parts, "\n")
}

func TestGenerateFromMockSAP(t *testing.T) {
	c, notes := Generate(sap.GenerateMock(42, "2026"), DefaultOptions())
	us := c.Company("1000")
	if us == nil || us.Currency != "USD" || us.Country != "US" || len(us.ChartOfAccounts) != 10 {
		t.Fatalf("company 1000: %+v", us)
	}
	if ad := us.AccountDetermination; ad.APReconciliation != "211000" || ad.InputTax != "140000" || ad.Bank != "100000" {
		t.Fatalf("account determination from T030/LFB1: %+v", ad)
	}
	if a := us.Account("211000"); a == nil || !a.VendorReconciliation || a.Type != core.Liability {
		t.Fatal("reconciliation account from SKB1-MITKZ")
	}
	if len(us.TaxCodes) != 2 || us.TaxCodes[0].RatePercent != "8" || c.Company("2000").TaxCodes[0].RatePercent != "19" {
		t.Fatalf("tax codes from T007A: %+v", us.TaxCodes)
	}
	if us.InvoiceTolerance.PricePercent != 2 || us.InvoiceTolerance.PriceAbsolute != "5.00" || us.HouseBank.IBAN != "GB33BUKB20201555555555" {
		t.Fatal("tolerance from T169G and house bank from T012K")
	}
	if len(c.PaymentTerms) != 4 || c.PaymentTerm("NT45").NetDays != 45 {
		t.Fatal("payment terms from T052")
	}
	clerk := c.Role("Z_AP_CLERK")
	if clerk == nil || !slices.Contains(clerk.Permissions, governance.ActInvoicePost) || !slices.Contains(clerk.Permissions, governance.ActInvoiceCapture) {
		t.Fatalf("tcodes mapped to permissions: %+v", clerk)
	}
	if r := c.Role("controller"); r == nil || !slices.Contains(r.Permissions, governance.ActConfigApprove) {
		t.Fatal("governance role mapping")
	}
	if u := c.User("super1"); u == nil || len(u.Roles) != 2 {
		t.Fatal("users from AGR_USERS")
	}
	if c.Payments.ReleaseRole != "Z_TREASURY_MGR" || c.Payments.VendorApproverRole != "vendor_master_approver" {
		t.Fatalf("payments roles: %+v", c.Payments)
	}
	if len(c.Agents) == 0 || c.Purchasing.RequisitionApproval[0].Steps[0].Role != "Z_PURCH_MGR" {
		t.Fatal("agents and requisition tiers")
	}
	all := notesText(notes)
	for _, want := range []string{"auto_post_limit", "fiscal year variant", "release strategies", "bank_change_hold_days"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note about %s", want)
		}
	}
	// The SAP role design carries one segregation-of-duties conflict.
	fs := config.Validate(c)
	var errs []config.Finding
	for _, f := range fs {
		if f.Severity == config.SevError {
			errs = append(errs, f)
		}
	}
	if len(errs) != 1 || errs[0].Path != "users[super1]" || !strings.Contains(errs[0].Message, "invoice.post and payment.release") {
		t.Fatalf("want exactly the super1 SoD conflict, got %v", errs)
	}
}

func TestGenerateGapsAndDefaults(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.T052, e.T169G, e.T030 = nil, nil, nil
	e.SKB1 = append(e.SKB1, sap.SKB1{BUKRS: "1000", SAKNR: "ABC", TXT50: "Odd account"})
	e.LFB1 = append(e.LFB1, sap.LFB1{LIFNR: "0000100001", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"}, sap.LFB1{LIFNR: "0000100002", BUKRS: "1000", AKONT: "0000212000", ZTERM: "NT30"})
	e.AGR_TCODES = append(e.AGR_TCODES, sap.AGRTcode{AGR_NAME: "Z_BUYER", TCODE: "ZCUSTOM1"})
	e.AGR_USERS = slices.DeleteFunc(e.AGR_USERS, func(u sap.AGRUser) bool { return u.AGR_NAME == "Z_CONTROLLER" })
	e.AGR_TCODES = slices.DeleteFunc(e.AGR_TCODES, func(t sap.AGRTcode) bool { return t.TCODE == "BNK_APP" })
	c, notes := Generate(e, DefaultOptions())
	all := notesText(notes)
	for _, want := range []string{
		"T052 not in extract", "T169G key PP not found", "not numeric", "reconciliation accounts",
		"ZCUSTOM1 has no platform equivalent", "no user holds governance role controller", "no role holds payment.release",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing note %q in:\n%s", want, all)
		}
	}
	if c.PaymentTerm("NT30") == nil || c.Company("1000").InvoiceTolerance.PricePercent != 2 {
		t.Fatal("placeholders for missing tables")
	}
	if c.Company("1000").AccountDetermination.Bank != "100000" {
		t.Fatal("bank account falls back to the house bank G/L account")
	}
	if c.Company("1000").AccountDetermination.InputTax != "" {
		t.Fatal("no T030 means no input tax account; validation will flag it")
	}
}

func TestRequisitionApproverFallback(t *testing.T) {
	e := sap.GenerateMock(42, "2026")
	e.AGR_TCODES = slices.DeleteFunc(e.AGR_TCODES, func(t sap.AGRTcode) bool { return t.TCODE == "ME54N" })
	c, _ := Generate(e, DefaultOptions())
	if c.Purchasing.RequisitionApproval[0].Steps[0].Role != "process_owner" {
		t.Fatalf("without a purchasing approver the process owner approves: %+v", c.Purchasing)
	}
}
