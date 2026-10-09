// Package workflow is the approval engine shared by requisitions, supplier
// master changes, payment releases and configuration changes. Requests move
// through sequential steps; each step needs a number of distinct approvers
// holding a role. Only people approve, never the requester, and nobody
// approves twice.
package workflow

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/money"
)

type Status string

const (
	Pending   Status = "pending"
	Approved  Status = "approved"
	Rejected  Status = "rejected"
	Cancelled Status = "cancelled"
)

// Actor is whoever acts on a request.
type Actor struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"` // human | agent | rule
	Roles []string `json:"roles"`
}

// Step requires Required distinct approvals from holders of Role.
type Step struct {
	Role       string   `json:"role"`
	Required   int      `json:"required"`
	ApprovedBy []string `json:"approvedBy,omitempty"`
}

type Event struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Comment string    `json:"comment,omitempty"`
}

type Request struct {
	ID        string      `json:"id"`
	Kind      string      `json:"kind"`    // e.g. requisition, vendor_change, payment_run
	Subject   string      `json:"subject"` // id of the object being approved
	Title     string      `json:"title"`
	Requester string      `json:"requester"`
	Amount    money.Minor `json:"amount,omitempty"`
	Steps     []Step      `json:"steps"`
	Current   int         `json:"current"`
	Status    Status      `json:"status"`
	History   []Event     `json:"history"`
}

var (
	ErrNotPending   = errors.New("request is not pending")
	ErrSelfApproval = errors.New("requesters cannot approve their own request")
	ErrNotHuman     = errors.New("only people can approve or reject")
	ErrWrongRole    = errors.New("approver does not hold the role this step requires")
	ErrAlreadyActed = errors.New("approver has already approved this request")
)

// New creates a pending request. A request with no steps is approved at once.
func New(id, kind, subject, title, requester string, amount money.Minor, steps []Step, now time.Time) (*Request, error) {
	for i, s := range steps {
		if s.Role == "" || s.Required < 1 {
			return nil, fmt.Errorf("step %d needs a role and at least one approval", i+1)
		}
	}
	r := &Request{
		ID: id, Kind: kind, Subject: subject, Title: title, Requester: requester, Amount: amount,
		Steps: slices.Clone(steps), Status: Pending,
		History: []Event{{At: now, Actor: requester, Action: "submitted"}},
	}
	if len(steps) == 0 {
		r.Status = Approved
		r.History = append(r.History, Event{At: now, Actor: "rule:workflow", Action: "approved", Comment: "no approval required"})
	}
	return r, nil
}

// Approve records an approval for the current step and advances the request.
func (r *Request) Approve(a Actor, comment string, now time.Time) error {
	if err := r.check(a); err != nil {
		return err
	}
	step := &r.Steps[r.Current]
	if !slices.Contains(a.Roles, step.Role) {
		return fmt.Errorf("%w (%s)", ErrWrongRole, step.Role)
	}
	step.ApprovedBy = append(step.ApprovedBy, a.ID)
	r.History = append(r.History, Event{At: now, Actor: a.ID, Action: "approved", Comment: comment})
	if len(step.ApprovedBy) >= step.Required {
		r.Current++
		if r.Current == len(r.Steps) {
			r.Status = Approved
		}
	}
	return nil
}

// Reject ends the request. Any holder of the current step's role may reject.
func (r *Request) Reject(a Actor, reason string, now time.Time) error {
	if err := r.check(a); err != nil && !errors.Is(err, ErrAlreadyActed) {
		return err
	}
	if !slices.Contains(a.Roles, r.Steps[r.Current].Role) {
		return fmt.Errorf("%w (%s)", ErrWrongRole, r.Steps[r.Current].Role)
	}
	if reason == "" {
		return errors.New("a rejection needs a reason")
	}
	r.Status = Rejected
	r.History = append(r.History, Event{At: now, Actor: a.ID, Action: "rejected", Comment: reason})
	return nil
}

// Cancel lets the requester withdraw a pending request.
func (r *Request) Cancel(actorID, reason string, now time.Time) error {
	if r.Status != Pending {
		return ErrNotPending
	}
	if actorID != r.Requester {
		return errors.New("only the requester can cancel a request")
	}
	r.Status = Cancelled
	r.History = append(r.History, Event{At: now, Actor: actorID, Action: "cancelled", Comment: reason})
	return nil
}

func (r *Request) check(a Actor) error {
	if r.Status != Pending {
		return ErrNotPending
	}
	if a.Kind != "human" {
		return ErrNotHuman
	}
	if a.ID == r.Requester {
		return ErrSelfApproval
	}
	for _, s := range r.Steps {
		if slices.Contains(s.ApprovedBy, a.ID) {
			return ErrAlreadyActed
		}
	}
	return nil
}

// Waiting describes what the request needs next, for queues and messages.
func (r *Request) Waiting() string {
	if r.Status != Pending {
		return string(r.Status)
	}
	s := r.Steps[r.Current]
	return fmt.Sprintf("waiting for %d more approval(s) from %s", s.Required-len(s.ApprovedBy), s.Role)
}
