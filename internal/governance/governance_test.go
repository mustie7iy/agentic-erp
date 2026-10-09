package governance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditChainDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := NewAuditLog(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := l.Append("agent:x", "invoice.post", "INV", "allow", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := NewAuditLog(path)
	if err != nil {
		t.Fatalf("clean log should verify: %v", err)
	}
	if len(reloaded.Events()) != 3 {
		t.Fatalf("want 3 events, got %d", len(reloaded.Events()))
	}

	data, _ := os.ReadFile(path)
	tampered := strings.Replace(string(data), `"outcome":"allow"`, `"outcome":"deny"`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditLog(path); err == nil || !strings.Contains(err.Error(), "modified") {
		t.Fatalf("tampering not detected: %v", err)
	}
}

func TestAuditChainDetectsDeletion(t *testing.T) {
	l, _ := NewAuditLog("")
	for range 3 {
		l.Append("a", "b", "c", "d", nil)
	}
	l.events = append(l.events[:1], l.events[2:]...)
	if err := l.Verify(); err == nil {
		t.Fatal("deleted event not detected")
	}
}

func TestPolicyDecisions(t *testing.T) {
	p := DefaultPolicy()
	l2 := Identity{ID: "rule", Permissions: []string{ActInvoicePost}, Autonomy: ActWithinLimits}
	if d := p.Decide(l2, ActInvoicePost, 100_00); d.Outcome != Allow {
		t.Errorf("small amount: %v", d)
	}
	if d := p.Decide(l2, ActInvoicePost, p.AutoLimit+1); d.Outcome != Escalate {
		t.Errorf("over limit: %v", d)
	}
	if d := p.Decide(l2, ActVendorCreate, 0); d.Outcome != Deny {
		t.Errorf("missing permission: %v", d)
	}
	l0 := Identity{ID: "agent", Permissions: []string{ActInvoicePost}, Autonomy: Suggest}
	if d := p.Decide(l0, ActInvoicePost, 1); d.Outcome != Escalate {
		t.Errorf("L0 must escalate: %v", d)
	}
	full := Identity{ID: "x", Permissions: []string{ActVendorBank}, Autonomy: FullyAutonomous}
	if d := p.Decide(full, ActVendorBank, 0); d.Outcome != Escalate {
		t.Errorf("bank change must always go to a human: %v", d)
	}
}

func TestSegregationOfDuties(t *testing.T) {
	p := DefaultPolicy()
	bad := Identity{ID: "agent:greedy", Permissions: []string{ActInvoicePost, ActPaymentRun}, Autonomy: FullyAutonomous}
	if err := p.ValidateIdentity(bad); err == nil {
		t.Fatal("SoD conflict not detected")
	}
	if d := p.Decide(bad, ActInvoicePost, 1); d.Outcome != Deny {
		t.Fatalf("conflicting identity must be denied, got %v", d)
	}
}

func TestAuditReloadWithStructuredDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, _ := NewAuditLog(path)
	type nested struct {
		Zeta  []string `json:"zeta"`
		Alpha int64    `json:"alpha"`
	}
	if _, err := l.Append("user:x", "analytics.query", "q1", "ok",
		map[string]any{"query": nested{Zeta: []string{"a"}, Alpha: 9007199254740993}, "rows": 18}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuditLog(path); err != nil {
		t.Fatalf("structured detail must verify after reload: %v", err)
	}
}
