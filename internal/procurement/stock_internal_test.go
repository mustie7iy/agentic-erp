package procurement

import (
	"errors"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestAsReceiptItems (batch 98S integration): inventory's refusal of the
// stock items it numbers 0, 1, … is named by the goods movement's items
// they are, its batch_number the request's batch; a field of no item
// keeps its name; the problem is copied, not changed, and an error that
// holds none is returned as it is.
func TestAsReceiptItems(t *testing.T) {
	refused := &problem.Problem{Code: "inventory.batch_invalid", Errors: []problem.FieldError{{Field: "items[0].batch_number"}, {Field: "items"}}}
	var got *problem.Problem
	if !errors.As(asReceiptItems(refused, []int{2}), &got) {
		t.Fatal("the renamed refusal: got no problem, want one")
	}
	if len(got.Errors) != 2 {
		t.Fatalf("the fields: got %d, want 2", len(got.Errors))
	}
	if a, b := got.Errors[0].Field, got.Errors[1].Field; a != "items[2].batch" || b != "items" {
		t.Errorf("the fields: got %s and %s, want items[2].batch and items", a, b)
	}
	if f := refused.Errors[0].Field; f != "items[0].batch_number" {
		t.Errorf("inventory's problem after the renaming: got %s, want it unchanged", f)
	}
	plain := errors.New("inventory: down")
	if err := asReceiptItems(plain, []int{2}); !errors.Is(err, plain) {
		t.Errorf("an error without a problem: got %v, want it as it is", err)
	}
}
