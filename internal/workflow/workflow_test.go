package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func human(id string, roles ...string) Actor { return Actor{ID: id, Kind: "human", Roles: roles} }

func twoStep(t *testing.T) *Request {
	t.Helper()
	r, err := New("WF-1", "requisition", "PR-1", "Laptops", "alice", 1_200_000,
		[]Step{{Role: "team_lead", Required: 1}, {Role: "controller", Required: 2}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSequentialApproval(t *testing.T) {
	r := twoStep(t)
	if err := r.Approve(human("carol", "controller"), "", t0); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("step 2 role must not approve step 1: %v", err)
	}
	if err := r.Approve(human("bob", "team_lead"), "ok", t0); err != nil {
		t.Fatal(err)
	}
	if r.Current != 1 || r.Status != Pending || !strings.Contains(r.Waiting(), "2 more approval(s) from controller") {
		t.Fatalf("unexpected state %+v / %s", r, r.Waiting())
	}
	if err := r.Approve(human("carol", "controller"), "", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(human("carol", "controller"), "", t0); !errors.Is(err, ErrAlreadyActed) {
		t.Fatalf("double approval: %v", err)
	}
	if err := r.Approve(human("dave", "controller"), "", t0); err != nil {
		t.Fatal(err)
	}
	if r.Status != Approved || r.Waiting() != "approved" {
		t.Fatalf("want approved, got %s", r.Status)
	}
	if err := r.Approve(human("erin", "controller"), "", t0); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approval after completion: %v", err)
	}
	if len(r.History) != 4 { // submitted + three approvals; failed attempts are not recorded
		t.Fatalf("history should record every action: %d", len(r.History))
	}
}

func TestControls(t *testing.T) {
	r := twoStep(t)
	if err := r.Approve(human("alice", "team_lead"), "", t0); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self approval: %v", err)
	}
	if err := r.Approve(Actor{ID: "agent:x", Kind: "agent", Roles: []string{"team_lead"}}, "", t0); !errors.Is(err, ErrNotHuman) {
		t.Fatalf("agent approval: %v", err)
	}
	// Same person may not approve two different steps.
	r.Approve(human("bob", "team_lead", "controller"), "", t0)
	if err := r.Approve(human("bob", "team_lead", "controller"), "", t0); !errors.Is(err, ErrAlreadyActed) {
		t.Fatalf("one person approving two steps: %v", err)
	}
}

func TestRejectAndCancel(t *testing.T) {
	r := twoStep(t)
	if err := r.Reject(human("bob", "team_lead"), "", t0); err == nil {
		t.Fatal("rejection without reason accepted")
	}
	if err := r.Reject(human("zed", "controller"), "no", t0); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("reject by wrong role: %v", err)
	}
	if err := r.Reject(human("bob", "team_lead"), "over budget", t0); err != nil || r.Status != Rejected {
		t.Fatalf("reject: %v %s", err, r.Status)
	}
	if err := r.Cancel("alice", "", t0); !errors.Is(err, ErrNotPending) {
		t.Fatalf("cancel after reject: %v", err)
	}

	c := twoStep(t)
	if err := c.Cancel("bob", "", t0); err == nil {
		t.Fatal("only the requester may cancel")
	}
	if err := c.Cancel("alice", "not needed", t0); err != nil || c.Status != Cancelled {
		t.Fatalf("cancel: %v", err)
	}
	if err := c.Reject(human("bob", "team_lead"), "x", t0); !errors.Is(err, ErrNotPending) {
		t.Fatalf("reject after cancel: %v", err)
	}
}

func TestNoStepsAutoApproves(t *testing.T) {
	r, err := New("WF-2", "requisition", "PR-2", "Pens", "alice", 500, nil, t0)
	if err != nil || r.Status != Approved {
		t.Fatalf("no-step request: %v %s", err, r.Status)
	}
	if _, err := New("WF-3", "x", "y", "z", "a", 0, []Step{{Role: "", Required: 1}}, t0); err == nil {
		t.Fatal("step without role accepted")
	}
	if _, err := New("WF-4", "x", "y", "z", "a", 0, []Step{{Role: "r", Required: 0}}, t0); err == nil {
		t.Fatal("step with zero approvals accepted")
	}
}
