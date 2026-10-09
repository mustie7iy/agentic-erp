package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/governance"
	"github.com/mustie7iy/agentic-erp/internal/workflow"
)

func fixtureYAML(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixture(t *testing.T) *Config {
	t.Helper()
	c, err := Parse(fixtureYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func yamlOf(t *testing.T, c *Config) []byte {
	t.Helper()
	b, err := c.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFixtureIsValid(t *testing.T) {
	if fs := Validate(fixture(t)); HasErrors(fs) {
		t.Fatalf("fixture invalid: %v", fs)
	}
}

func TestParseIsStrict(t *testing.T) {
	if _, err := Parse([]byte("version: 1\ncompnies: []\n")); err == nil {
		t.Fatal("misspelled field must be rejected")
	}
	if _, err := Parse([]byte("version: [")); err == nil {
		t.Fatal("broken YAML must be rejected")
	}
	if _, err := Load("testdata/missing.yaml"); err == nil {
		t.Fatal("missing file must error")
	}
	if c, err := Load("testdata/valid.yaml"); err != nil || c.Company("1000") == nil {
		t.Fatalf("load: %v", err)
	}
}

// Each mutation must produce an error finding at the expected path.
func TestValidationRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
	}{
		{"version", func(c *Config) { c.Version = 2 }, "version"},
		{"no companies", func(c *Config) { c.Companies = nil }, "companies"},
		{"duplicate company", func(c *Config) { c.Companies = append(c.Companies, c.Companies[0]) }, "companies[1000]"},
		{"empty code", func(c *Config) { c.Companies[0].Code = "" }, "companies[0].code"},
		{"name", func(c *Config) { c.Companies[0].Name = " " }, "companies[1000].name"},
		{"currency", func(c *Config) { c.Companies[0].Currency = "usd" }, "companies[1000].currency"},
		{"country", func(c *Config) { c.Companies[0].Country = "USA" }, "companies[1000].country"},
		{"fiscal", func(c *Config) { c.Companies[0].FiscalYearStartMonth = 13 }, "companies[1000].fiscal_year_start_month"},
		{"account number", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Number = "12-34" }, "companies[1000].chart_of_accounts[12-34]"},
		{"duplicate account", func(c *Config) {
			co := &c.Companies[0]
			co.ChartOfAccounts = append(co.ChartOfAccounts, co.ChartOfAccounts[0])
		}, "companies[1000].chart_of_accounts[100000]"},
		{"account type", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Type = "cash" }, "companies[1000].chart_of_accounts[100000].type"},
		{"recon must be liability", func(c *Config) { c.Companies[0].ChartOfAccounts[0].VendorReconciliation = true }, "companies[1000].chart_of_accounts[100000]"},
		{"empty chart", func(c *Config) { c.Companies[0].ChartOfAccounts = nil }, "companies[1000].chart_of_accounts"},
		{"no recon", func(c *Config) {
			for i := range c.Companies[0].ChartOfAccounts {
				c.Companies[0].ChartOfAccounts[i].VendorReconciliation = false
			}
		}, "companies[1000].chart_of_accounts"},
		{"ap missing", func(c *Config) { c.Companies[0].AccountDetermination.APReconciliation = "" }, "companies[1000].account_determination.ap_reconciliation"},
		{"ap unknown", func(c *Config) { c.Companies[0].AccountDetermination.APReconciliation = "999999" }, "companies[1000].account_determination.ap_reconciliation"},
		{"input tax wrong type", func(c *Config) { c.Companies[0].AccountDetermination.InputTax = "400000" }, "companies[1000].account_determination.input_tax"},
		{"ap not recon", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "212000", Name: "Other", Type: core.Liability})
			c.Companies[0].AccountDetermination.APReconciliation = "212000"
		}, "companies[1000].account_determination.ap_reconciliation"},
		{"tax duplicate", func(c *Config) {
			co := &c.Companies[0]
			co.TaxCodes = append(co.TaxCodes, co.TaxCodes[0])
		}, "companies[1000].tax_codes[V1]"},
		{"tax rate", func(c *Config) { c.Companies[0].TaxCodes[0].RatePercent = "101" }, "companies[1000].tax_codes[V1].rate_percent"},
		{"tax rate parse", func(c *Config) { c.Companies[0].TaxCodes[0].RatePercent = "eight" }, "companies[1000].tax_codes[V1].rate_percent"},
		{"tax account", func(c *Config) { c.Companies[0].TaxCodes[0].Account = "1" }, "companies[1000].tax_codes[V1].account"},
		{"tolerance pct", func(c *Config) { c.Companies[0].InvoiceTolerance.PricePercent = -1 }, "companies[1000].invoice_tolerance.price_percent"},
		{"tolerance abs", func(c *Config) { c.Companies[0].InvoiceTolerance.PriceAbsolute = "-5" }, "companies[1000].invoice_tolerance.price_absolute"},
		{"limit", func(c *Config) { c.Companies[0].AutoPostLimit = "lots" }, "companies[1000].auto_post_limit"},
		{"over delivery", func(c *Config) { c.Companies[0].OverDeliveryPercent = 101 }, "companies[1000].gr_over_delivery_percent"},
		{"iban", func(c *Config) { c.Companies[0].HouseBank.IBAN = "GB33BUKB20201555555556" }, "companies[1000].house_bank.iban"},
		{"bic", func(c *Config) { c.Companies[0].HouseBank.BIC = "BUK" }, "companies[1000].house_bank.bic"},
		{"terms key", func(c *Config) { c.PaymentTerms[0].Key = "net thirty days!" }, "payment_terms[net thirty days!]"},
		{"terms days", func(c *Config) { c.PaymentTerms[0].NetDays = 400 }, "payment_terms[NT14].net_days"},
		{"no terms", func(c *Config) { c.PaymentTerms = nil }, "payment_terms"},
		{"role dup", func(c *Config) { c.Roles = append(c.Roles, c.Roles[0]) }, "roles[" + "Z_AP_CLERK" + "]"},
		{"role perm", func(c *Config) { c.Roles[0].Permissions = append(c.Roles[0].Permissions, "everything") }, "roles[Z_AP_CLERK].permissions"},
		{"user dup", func(c *Config) { c.Users = append(c.Users, c.Users[0]) }, "users[apclerk1]"},
		{"user role", func(c *Config) { c.Users[0].Roles = []string{"ghost"} }, "users[apclerk1].roles"},
		{"user company", func(c *Config) { c.Users[0].Companies = []string{"9999"} }, "users[apclerk1].companies"},
		{"agent id", func(c *Config) { c.Agents[0].ID = "bot" }, "agents[bot]"},
		{"agent autonomy", func(c *Config) { c.Agents[0].Autonomy = 4 }, "agents[agent:ap-exceptions].autonomy"},
		{"agent unknown perm", func(c *Config) { c.Agents[0].Permissions = []string{"x"} }, "agents[agent:ap-exceptions].permissions"},
		{"agent approval perm", func(c *Config) { c.Agents[0].Permissions = []string{governance.ActConfigApprove} }, "agents[agent:ap-exceptions].permissions"},
		{"tiers empty", func(c *Config) { c.Purchasing.RequisitionApproval = nil }, "purchasing.requisition_approval"},
		{"last tier capped", func(c *Config) { c.Purchasing.RequisitionApproval[1].UpTo = "1000000.00" }, "purchasing.requisition_approval[1].up_to"},
		{"middle tier open", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "" }, "purchasing.requisition_approval[0].up_to"},
		{"tier order", func(c *Config) {
			r := c.Purchasing.RequisitionApproval
			c.Purchasing.RequisitionApproval = []ApprovalTier{{UpTo: "100.00", Steps: r[0].Steps}, {UpTo: "50.00", Steps: r[0].Steps}, r[1]}
		}, "purchasing.requisition_approval[1].up_to"},
		{"tier amount", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "x" }, "purchasing.requisition_approval[0].up_to"},
		{"tier role", func(c *Config) { c.Purchasing.RequisitionApproval[0].Steps[0].Role = "ghost" }, "purchasing.requisition_approval[0].steps[0].role"},
		{"tier role perm", func(c *Config) { c.Purchasing.RequisitionApproval[0].Steps[0].Role = "controller" }, "purchasing.requisition_approval[0].steps[0].role"},
		{"tier approvals", func(c *Config) { c.Purchasing.RequisitionApproval[0].Steps[0].Approvals = 0 }, "purchasing.requisition_approval[0].steps[0].approvals"},
		{"release role", func(c *Config) { c.Payments.ReleaseRole = "Z_BUYER" }, "payments.release_role"},
		{"release role unknown", func(c *Config) { c.Payments.ReleaseRole = "ghost" }, "payments.release_role"},
		{"release approvals", func(c *Config) { c.Payments.ReleaseApprovals = 0 }, "payments.release_approvals"},
		{"hold days", func(c *Config) { c.Payments.BankChangeHoldDays = -1 }, "payments.bank_change_hold_days"},
		{"sod perm", func(c *Config) { c.SoDConflicts = append(c.SoDConflicts, [2]string{"x", "y"}) }, "sod_conflicts[7]"},
		{"sod role", func(c *Config) {
			c.Roles[0].Permissions = append(c.Roles[0].Permissions, governance.ActPaymentRun)
		}, "roles[Z_AP_CLERK]"},
		{"sod user", func(c *Config) { c.User("super1").Roles = append(c.User("super1").Roles, "Z_TREASURY_MGR") }, "users[super1]"},
		{"sod agent", func(c *Config) {
			c.Agents[0].Permissions = []string{governance.ActInvoicePost, governance.ActPaymentPropose, governance.ActRequisition, governance.ActVendorChange}
			c.SoDConflicts = append(c.SoDConflicts, [2]string{governance.ActInvoicePost, governance.ActPaymentPropose})
		}, "agents[agent:ap-exceptions]"},
		{"output", func(c *Config) { c.Output.PurchaseOrderChannel = "fax" }, "output.purchase_order_channel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.mutate(c)
			fs := Validate(c)
			if !slices.ContainsFunc(fs, func(f Finding) bool { return f.Severity == SevError && f.Path == tc.path }) {
				t.Fatalf("no error at %s; got %v", tc.path, fs)
			}
		})
	}
}

func TestValidationWarnings(t *testing.T) {
	c := fixture(t)
	c.Companies[0].TaxCodes = nil
	c.SoDConflicts = nil
	fs := Validate(c)
	if HasErrors(fs) {
		t.Fatalf("warnings only expected: %v", fs)
	}
	if len(fs) != 2 || fs[0].String() == "" {
		t.Fatalf("want 2 warnings, got %v", fs)
	}
}

func TestValidIBAN(t *testing.T) {
	for iban, want := range map[string]bool{
		"GB33BUKB20201555555555": true, "DE89 3704 0044 0532 0130 00": true,
		"GB33BUKB20201555555556": false, "XX": false, "1234567890123456": false, "GB33BUKB2020155555555!": false,
	} {
		if ValidIBAN(iban) != want {
			t.Errorf("ValidIBAN(%q) != %v", iban, want)
		}
	}
}

func TestDiffTiers(t *testing.T) {
	base := fixture(t)
	cases := []struct {
		name   string
		mutate func(c *Config)
		path   string
		tier   Tier
	}{
		{"rename company", func(c *Config) { c.Companies[0].Name = "Acme Inc" }, "companies[1000].name", TierCosmetic},
		{"account name", func(c *Config) { c.Companies[0].ChartOfAccounts[0].Name = "Bank" }, "companies[1000].chart_of_accounts[100000].name", TierCosmetic},
		{"tolerance", func(c *Config) { c.Companies[0].InvoiceTolerance.PricePercent = 3 }, "companies[1000].invoice_tolerance.price_percent", TierOperational},
		{"terms", func(c *Config) { c.PaymentTerms[0].NetDays = 21 }, "payment_terms[NT14].net_days", TierOperational},
		{"terms text", func(c *Config) { c.PaymentTerms[0].Description = "Two weeks" }, "payment_terms[NT14].description", TierCosmetic},
		{"routing", func(c *Config) { c.Purchasing.RequisitionApproval[0].UpTo = "5000.00" }, "purchasing.requisition_approval[0].up_to", TierOperational},
		{"hold days", func(c *Config) { c.Payments.BankChangeHoldDays = 10 }, "payments.bank_change_hold_days", TierOperational},
		{"output", func(c *Config) { c.Output.PurchaseOrderChannel = "edi" }, "output.purchase_order_channel", TierOperational},
		{"account determination", func(c *Config) { c.Companies[0].AccountDetermination.InputTax = "100000" }, "companies[1000].account_determination.input_tax", TierPostingRelated},
		{"auto limit", func(c *Config) { c.Companies[0].AutoPostLimit = "75000.00" }, "companies[1000].auto_post_limit", TierPostingRelated},
		{"release role", func(c *Config) { c.Payments.ReleaseRole = "controller" }, "payments.release_role", TierPostingRelated},
		{"user role", func(c *Config) { c.User("buyer1").Roles = append(c.User("buyer1").Roles, "Z_WAREHOUSE") }, "users[buyer1].roles[Z_WAREHOUSE]", TierPostingRelated},
		{"currency", func(c *Config) { c.Companies[0].Currency = "CAD" }, "companies[1000].currency", TierStructural},
		{"new account", func(c *Config) {
			c.Companies[0].ChartOfAccounts = append(c.Companies[0].ChartOfAccounts, Account{Number: "620000", Name: "Rent", Type: core.Expense})
		}, "companies[1000].chart_of_accounts[620000]", TierStructural},
		{"new user", func(c *Config) {
			c.Users = append(c.Users, User{ID: "new1", Name: "New", Roles: []string{"Z_BUYER"}})
		}, "users[new1]", TierPostingRelated},
		{"new company", func(c *Config) {
			n := c.Companies[0]
			n.Code = "3000"
			c.Companies = append(c.Companies, n)
		}, "companies[3000]", TierStructural},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fixture(t)
			tc.mutate(c)
			changes, err := Diff(base, c)
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) != 1 || changes[0].Path != tc.path || changes[0].Tier != tc.tier {
				t.Fatalf("want one change %s at %v, got %v", tc.path, tc.tier, changes)
			}
			if changes[0].String() == "" || MaxTier(changes) != tc.tier {
				t.Fatal("String/MaxTier")
			}
		})
	}
	// Removal is reported at the item.
	c := fixture(t)
	c.Users = c.Users[1:]
	changes, _ := Diff(base, c)
	if len(changes) != 1 || changes[0].Path != "users[apclerk1]" || changes[0].Old != "(removed)" || !strings.HasPrefix(changes[0].String(), "-") {
		t.Fatalf("removal: %v", changes)
	}
	if changes, _ := Diff(base, fixture(t)); len(changes) != 0 || MaxTier(changes) != 0 {
		t.Fatal("identical configs must not differ")
	}
	if Tier(0).String() != "no change" || TierStructural.String() == "" {
		t.Fatal("tier names")
	}
	if classify("unknown.path") != TierPostingRelated {
		t.Fatal("unmatched paths default to posting-relevant")
	}
}

func TestRuntime(t *testing.T) {
	c := fixture(t)
	s, err := c.Settings("1000")
	if err != nil {
		t.Fatal(err)
	}
	if s.Tolerance.PercentOfLine != 2 || s.Tolerance.Absolute != 500 || s.AutoPostLimit != 5_000_000 || s.InputTax != "140000" || s.HouseBank.IBAN == "" {
		t.Fatalf("settings: %+v", s)
	}
	if _, err := c.Settings("9999"); err == nil {
		t.Fatal("unknown company")
	}
	c.Companies[0].InvoiceTolerance.PriceAbsolute = "x"
	if _, err := c.Settings("1000"); err == nil {
		t.Fatal("bad tolerance must error")
	}
	c = fixture(t)
	c.Companies[0].AutoPostLimit = "x"
	if _, err := c.Settings("1000"); err == nil {
		t.Fatal("bad limit must error")
	}
	if _, err := c.Policy("1000"); err == nil {
		t.Fatal("policy on bad settings must error")
	}
	c = fixture(t)
	p, err := c.Policy("1000")
	if err != nil || p.AutoLimit != 5_000_000 || len(p.SoDConflicts) == 0 {
		t.Fatalf("policy: %+v %v", p, err)
	}

	id, err := c.Identity("apclerk1")
	if err != nil || id.Kind != "human" || !id.Can(governance.ActInvoicePost) || id.ID != "user:apclerk1" {
		t.Fatalf("user identity: %+v %v", id, err)
	}
	ag, err := c.Identity("agent:configuration")
	if err != nil || ag.Kind != "agent" || !ag.Can(governance.ActConfigPropose) {
		t.Fatalf("agent identity: %+v %v", ag, err)
	}
	if _, err := c.Identity("nobody"); err == nil {
		t.Fatal("unknown identity")
	}
	a, err := c.Actor("owner1")
	if err != nil || a.Kind != "human" || !slices.Contains(a.Roles, "process_owner") {
		t.Fatalf("actor: %+v", a)
	}
	if a, err := c.Actor("agent:test"); err != nil || a.Kind != "agent" {
		t.Fatalf("agent actor: %+v %v", a, err)
	}
	if _, err := c.Actor("nobody"); err == nil {
		t.Fatal("unknown actor")
	}
	if c.UserPermissions("nobody") != nil {
		t.Fatal("unknown user has no permissions")
	}
	c.User("buyer1").Companies = []string{"2000"}
	if c.CanAccess("buyer1", "1000") || !c.CanAccess("buyer1", "2000") || !c.CanAccess("owner1", "1000") || !c.CanAccess("agent:test", "1000") || c.CanAccess("ghost", "1000") {
		t.Fatal("company access")
	}

	small, _ := c.RequisitionSteps(500_000)
	large, _ := c.RequisitionSteps(5_000_000)
	if len(small) != 1 || len(large) != 2 || large[1].Role != "process_owner" {
		t.Fatalf("requisition routing: %v / %v", small, large)
	}
	c.Purchasing.RequisitionApproval[0].UpTo = "bad"
	if _, err := c.RequisitionSteps(1); err == nil {
		t.Fatal("bad tier amount must error")
	}
	c.Purchasing.RequisitionApproval = []ApprovalTier{{UpTo: "1.00", Steps: nil}}
	if _, err := c.RequisitionSteps(1_000); err == nil {
		t.Fatal("uncovered amount must error")
	}
	if ApprovalSteps(TierCosmetic) != nil || len(ApprovalSteps(TierOperational)) != 1 || len(ApprovalSteps(TierPostingRelated)) != 2 || ApprovalSteps(TierStructural)[0].Role != "solution_architect" {
		t.Fatal("approval steps by tier")
	}
	if c.PaymentTerm("NT30") == nil || c.PaymentTerm("X") != nil || c.Role("x") != nil || c.Agent("x") != nil {
		t.Fatal("lookups")
	}
}

var clock = time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)

func newRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	dir := t.TempDir()
	audit, _ := governance.NewAuditLog("")
	r, err := OpenRepo(dir, audit)
	if err != nil {
		t.Fatal(err)
	}
	r.SetClock(func() time.Time { return clock })
	if _, _, err := r.Bootstrap(fixtureYAML(t), "user:architect1"); err != nil {
		t.Fatal(err)
	}
	return r, dir
}

func TestBootstrap(t *testing.T) {
	r, _ := newRepo(t)
	if _, _, err := r.Bootstrap(fixtureYAML(t), "x"); err == nil {
		t.Fatal("second bootstrap must fail")
	}
	empty, _ := OpenRepo(t.TempDir(), nil)
	if _, _, err := empty.Active(); !errors.Is(err, ErrNoActive) {
		t.Fatalf("empty repo: %v", err)
	}
	if _, err := empty.Propose("x", "t", "", fixtureYAML(t)); !errors.Is(err, ErrNoActive) {
		t.Fatalf("propose on empty repo: %v", err)
	}
	if _, _, err := empty.Bootstrap([]byte("version: ["), "x"); err == nil {
		t.Fatal("bad yaml")
	}
	bad := fixture(t)
	bad.Version = 9
	if _, fs, err := empty.Bootstrap(yamlOf(t, bad), "x"); err == nil || !HasErrors(fs) {
		t.Fatal("invalid bootstrap must fail with findings")
	}
}

func TestTier2LifecycleAndPersistence(t *testing.T) {
	r, dir := newRepo(t)
	c := fixture(t)
	c.Companies[0].InvoiceTolerance.PricePercent = 3
	cr, err := r.Propose("agent:configuration", "Raise price tolerance to 3%", "Too many small variances", yamlOf(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if cr.Tier != TierOperational || cr.Status != CRPendingApproval || cr.Author != "agent:configuration" {
		t.Fatalf("cr: %+v", cr)
	}
	if _, err := r.Activate(cr.ID, "architect1"); err == nil {
		t.Fatal("cannot activate before approval")
	}
	if _, err := r.Approve(cr.ID, "controller1", ""); !errors.Is(err, workflow.ErrWrongRole) {
		t.Fatalf("controller is not the tier-2 approver: %v", err)
	}
	if _, err := r.Approve(cr.ID, "apclerk1", ""); !errors.Is(err, ErrPermission) {
		t.Fatalf("clerk lacks config.approve: %v", err)
	}
	if _, err := r.Approve(cr.ID, "ghost", ""); err == nil {
		t.Fatal("unknown approver")
	}
	cr, err = r.Approve(cr.ID, "owner1", "agreed")
	if err != nil || cr.Status != CRApproved {
		t.Fatalf("approve: %v %s", err, cr.Status)
	}
	if _, err := r.Approve(cr.ID, "owner1", ""); err == nil {
		t.Fatal("approving an approved CR must fail")
	}
	if _, err := r.Activate(cr.ID, "owner1"); !errors.Is(err, ErrPermission) {
		t.Fatalf("process owner cannot activate: %v", err)
	}
	if _, err := r.Activate(cr.ID, "ghost"); err == nil {
		t.Fatal("unknown activator")
	}
	n, err := r.Activate(cr.ID, "architect1")
	if err != nil || n != 2 {
		t.Fatalf("activate: %v %d", err, n)
	}
	active, v, _ := r.Active()
	if v != 2 || active.Companies[0].InvoiceTolerance.PricePercent != 3 {
		t.Fatal("active version not updated")
	}

	// Reopen from disk: same state.
	r2, err := OpenRepo(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, v, _ := r2.Active(); v != 2 || len(r2.Versions()) != 2 || len(r2.ChangeRequests()) != 1 {
		t.Fatal("state not persisted")
	}
	if got, _ := r2.Get(cr.ID); got.Status != CRActive || got.ActivatedVersion != 2 {
		t.Fatalf("persisted CR: %+v", got)
	}
	if b, err := r2.VersionYAML(1); err != nil || len(b) == 0 {
		t.Fatal("version yaml")
	}
	if _, err := r2.VersionYAML(9); err == nil {
		t.Fatal("missing version")
	}
}

func TestTier3NeedsEvidenceAndTwoApprovers(t *testing.T) {
	r, _ := newRepo(t)
	c := fixture(t)
	c.Companies[0].AutoPostLimit = "75000.00"
	cr, err := r.Propose("agent:configuration", "Raise auto-post limit", "", yamlOf(t, c))
	if err != nil || cr.Tier != TierPostingRelated || cr.Status != CRAwaitingEvidence {
		t.Fatalf("propose: %v %+v", err, cr)
	}
	if _, err := r.Approve(cr.ID, "owner1", ""); err == nil {
		t.Fatal("approval before evidence must fail")
	}
	cr, _ = r.AddEvidence(cr.ID, Evidence{Kind: "simulation", Passed: true, Summary: "2 invoices change outcome"})
	if cr.Status != CRAwaitingEvidence {
		t.Fatal("regression evidence still missing")
	}
	cr, _ = r.AddEvidence(cr.ID, Evidence{Kind: "regression", Passed: false, Summary: "1 case changed"})
	if cr.Status != CRPendingApproval || len(cr.Evidence) != 2 {
		t.Fatalf("status %s", cr.Status)
	}
	// Re-adding a kind replaces it.
	cr, _ = r.AddEvidence(cr.ID, Evidence{Kind: "regression", Passed: true, Summary: "rerun"})
	if len(cr.Evidence) != 2 {
		t.Fatal("evidence should be replaced, not duplicated")
	}
	if _, err := r.Approve(cr.ID, "owner1", ""); err != nil {
		t.Fatal(err)
	}
	if cr, _ := r.Get(cr.ID); cr.Status != CRPendingApproval {
		t.Fatal("still needs the controller")
	}
	cr, err = r.Approve(cr.ID, "controller1", "")
	if err != nil || cr.Status != CRApproved {
		t.Fatalf("second approval: %v %s", err, cr.Status)
	}
	if _, err := r.Activate(cr.ID, "architect1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddEvidence(cr.ID, Evidence{Kind: "simulation"}); err == nil {
		t.Fatal("evidence on an active CR must fail")
	}
}

func TestRejectStaleInvalidAndPermissions(t *testing.T) {
	r, _ := newRepo(t)
	c := fixture(t)
	if _, err := r.Propose("apclerk1", "x", "", yamlOf(t, c)); !errors.Is(err, ErrPermission) {
		t.Fatalf("clerk cannot propose: %v", err)
	}
	if _, err := r.Propose("ghost", "x", "", yamlOf(t, c)); err == nil {
		t.Fatal("unknown author")
	}
	if _, err := r.Propose("analyst1", "", "", yamlOf(t, c)); err == nil {
		t.Fatal("title required")
	}
	if _, err := r.Propose("analyst1", "noop", "", yamlOf(t, c)); err == nil {
		t.Fatal("no-op proposal must fail")
	}
	if _, err := r.Propose("analyst1", "bad", "", []byte("version: [")); err == nil {
		t.Fatal("unparseable proposal")
	}
	// Invalid proposals are recorded but cannot proceed.
	bad := fixture(t)
	bad.Companies[0].Currency = "dollars"
	inv, err := r.Propose("analyst1", "Break currency", "", yamlOf(t, bad))
	if err != nil || inv.Status != CRInvalid {
		t.Fatalf("invalid: %v %s", err, inv.Status)
	}
	if _, err := r.AddEvidence(inv.ID, Evidence{Kind: "simulation"}); err == nil {
		t.Fatal("evidence on invalid CR")
	}

	// Two competing tier-2 changes: the second becomes stale once the first activates.
	a, b := fixture(t), fixture(t)
	a.PaymentTerms[0].NetDays = 20
	b.PaymentTerms[1].NetDays = 25
	crA, _ := r.Propose("analyst1", "A", "", yamlOf(t, a))
	crB, _ := r.Propose("analyst1", "B", "", yamlOf(t, b))
	r.Approve(crA.ID, "owner1", "")
	r.Approve(crB.ID, "owner1", "")
	if _, err := r.Activate(crA.ID, "architect1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Activate(crB.ID, "architect1"); !errors.Is(err, ErrStale) {
		t.Fatalf("want stale, got %v", err)
	}

	// Rejection.
	d := fixture(t)
	d.PaymentTerms[0].NetDays = 21
	crD, _ := r.Propose("analyst1", "D", "", yamlOf(t, d))
	if _, err := r.Reject(crD.ID, "owner1", ""); err == nil {
		t.Fatal("rejection needs a reason")
	}
	crD, err = r.Reject(crD.ID, "owner1", "not now")
	if err != nil || crD.Status != CRRejected {
		t.Fatalf("reject: %v", err)
	}
	if _, err := r.Get("CR-9999"); err == nil {
		t.Fatal("missing CR")
	}
	if _, err := r.Approve("CR-9999", "owner1", ""); err == nil {
		t.Fatal("approve missing CR")
	}
	if _, err := r.Activate("CR-9999", "architect1"); err == nil {
		t.Fatal("activate missing CR")
	}
	if _, err := r.AddEvidence("CR-9999", Evidence{}); err == nil {
		t.Fatal("evidence on missing CR")
	}
	if list := r.ChangeRequests(); len(list) != 4 || list[0].ID < list[1].ID {
		t.Fatalf("list newest first: %d", len(list))
	}
}

func TestRollbackAndTamperDetection(t *testing.T) {
	r, dir := newRepo(t)
	c := fixture(t)
	c.PaymentTerms[0].NetDays = 20
	cr, _ := r.Propose("analyst1", "terms", "", yamlOf(t, c))
	r.Approve(cr.ID, "owner1", "")
	r.Activate(cr.ID, "architect1")

	if _, err := r.Rollback(1, "owner1", "x"); !errors.Is(err, ErrPermission) {
		t.Fatalf("owner cannot roll back: %v", err)
	}
	if _, err := r.Rollback(1, "ghost", "x"); err == nil {
		t.Fatal("unknown user")
	}
	if _, err := r.Rollback(1, "architect1", ""); err == nil {
		t.Fatal("reason required")
	}
	if _, err := r.Rollback(2, "architect1", "x"); err == nil {
		t.Fatal("cannot roll back to the active version")
	}
	if _, err := r.Rollback(7, "architect1", "x"); err == nil {
		t.Fatal("cannot roll back to a missing version")
	}
	n, err := r.Rollback(1, "architect1", "terms change broke payments")
	if err != nil || n != 3 {
		t.Fatalf("rollback: %v %d", err, n)
	}
	active, _, _ := r.Active()
	if active.PaymentTerms[0].NetDays != 14 || r.Versions()[2].Source != "rollback to v1" {
		t.Fatal("rollback content")
	}

	// Editing a version file outside the process is detected.
	path := filepath.Join(dir, "versions", "v0003.yaml")
	b, _ := os.ReadFile(path)
	os.WriteFile(path, []byte(strings.Replace(string(b), "net_days: 14", "net_days: 99", 1)), 0o644)
	if _, _, err := r.Active(); err == nil || !strings.Contains(err.Error(), "modified outside") {
		t.Fatalf("tamper not detected: %v", err)
	}
	if _, err := r.Rollback(2, "architect1", "x"); err == nil {
		t.Fatal("rollback with a tampered active version must fail")
	}
}

func TestOpenRepoErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0o644)
	if _, err := OpenRepo(dir, nil); err == nil {
		t.Fatal("corrupt state must error")
	}
	os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"active":0}`), 0o644)
	r, err := OpenRepo(dir, nil)
	if err != nil || r.state.Changes == nil {
		t.Fatalf("minimal state: %v", err)
	}
	unreadable := t.TempDir()
	os.Mkdir(filepath.Join(unreadable, "state.json"), 0o755)
	if _, err := OpenRepo(unreadable, nil); err == nil {
		t.Fatal("unreadable state must error")
	}
}
