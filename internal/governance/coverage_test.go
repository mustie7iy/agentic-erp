package governance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditFileErrors(t *testing.T) {
	dir := t.TempDir()
	// Unreadable log file.
	locked := filepath.Join(dir, "locked.jsonl")
	os.WriteFile(locked, []byte("{}\n"), 0o000)
	if _, err := NewAuditLog(locked); err == nil {
		t.Fatal("unreadable audit log must error")
	}
	// A line that is not JSON.
	bad := filepath.Join(dir, "bad.jsonl")
	os.WriteFile(bad, []byte("not json\n"), 0o644)
	if _, err := NewAuditLog(bad); err == nil || !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("bad line: %v", err)
	}
	// A directory instead of a file: reading fails.
	asDir := filepath.Join(dir, "dir.jsonl")
	os.Mkdir(asDir, 0o755)
	if _, err := NewAuditLog(asDir); err == nil {
		t.Fatal("directory as audit log must error")
	}
}

func TestAppendErrors(t *testing.T) {
	l, _ := NewAuditLog("")
	if _, err := l.Append("a", "b", "c", "d", map[string]any{"x": make(chan int)}); err == nil {
		t.Fatal("unserializable detail must error")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, "file")
	os.WriteFile(parent, nil, 0o644)
	if _, err := NewAuditLog(filepath.Join(parent, "audit.jsonl")); err == nil {
		t.Fatal("a log path under a file must be refused")
	}
	l2 := &AuditLog{path: filepath.Join(parent, "audit.jsonl"), now: l.now}
	if _, err := l2.Append("a", "b", "c", "d", nil); err == nil {
		t.Fatal("directory creation failure must error")
	}
	asDir := filepath.Join(dir, "a.jsonl")
	os.Mkdir(asDir, 0o755)
	l3 := &AuditLog{path: asDir, now: l.now}
	if _, err := l3.Append("a", "b", "c", "d", nil); err == nil {
		t.Fatal("open failure must error")
	}
	if len(l3.Events()) != 0 {
		t.Fatal("a failed append must not be recorded")
	}
}

func TestVerifySequenceGap(t *testing.T) {
	l, _ := NewAuditLog("")
	l.Append("a", "b", "c", "d", nil)
	l.Append("a", "b", "c", "d", nil)
	l.events[1].Seq = 3
	l.events[1].Hash = hashEvent(l.events[1])
	if err := l.Verify(); err == nil || !strings.Contains(err.Error(), "sequence gap") {
		t.Fatalf("gap: %v", err)
	}
}

func TestPolicyApprovalLevel(t *testing.T) {
	p := DefaultPolicy()
	l1 := Identity{ID: "agent", Permissions: []string{ActInvoicePost}, Autonomy: ActWithApproval}
	if d := p.Decide(l1, ActInvoicePost, 1); d.Outcome != Escalate || !strings.Contains(d.Reason, "L1") {
		t.Fatalf("L1: %+v", d)
	}
	full := Identity{ID: "x", Permissions: []string{ActInvoicePost}, Autonomy: FullyAutonomous}
	if d := p.Decide(full, ActInvoicePost, p.AutoLimit*10); d.Outcome != Allow {
		t.Fatal("L3 ignores the limit")
	}
}
