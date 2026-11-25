package problem_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestLed: a refusal, wrapped or not, comes back as a copy with its
// detail led and no field errors, its code and status kept and the
// original unchanged; an error without a problem comes back as it is.
func TestLed(t *testing.T) {
	refused := problem.Invalid("receivables.customer_not_usable", "customer_id", "a customer that can be posted to")
	refused.Detail = "The customer is blocked."
	for _, err := range []error{refused, fmt.Errorf("receivables: post: %w", refused)} {
		t.Run(err.Error(), func(t *testing.T) {
			var got *problem.Problem
			if !errors.As(problem.Led(err, "Notice of BP1: "), &got) {
				t.Fatalf("got %v, want a problem", err)
			}
			if got.Code != refused.Code || got.Status != refused.Status {
				t.Errorf("code: got %d %s, want %d %s", got.Status, got.Code, refused.Status, refused.Code)
			}
			if got.Detail != "Notice of BP1: The customer is blocked." {
				t.Errorf("detail: got %q, want it led", got.Detail)
			}
			if len(got.Errors) != 0 {
				t.Errorf("field errors: got %v, want none", got.Errors)
			}
			if refused.Detail != "The customer is blocked." || len(refused.Errors) != 1 {
				t.Errorf("the original: got %q with %d field errors, want it unchanged", refused.Detail, len(refused.Errors))
			}
		})
	}
	plain := errors.New("connection lost")
	if got := problem.Led(plain, "Notice of BP1: "); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("an error without a problem: got %v, want it as it is", got)
	}
}

// TestRenamed: a refusal, wrapped or not, comes back as a copy with each
// field error's field renamed and those rename does not keep left out,
// its code, status, detail and the kept field errors' codes and messages
// as they were and the original unchanged; an error without a problem
// comes back as it is.
func TestRenamed(t *testing.T) {
	refused := problem.Unprocessable("inventory.batch_invalid", "Material M1 has no batch B9.",
		problem.FieldError{Field: "items[1].batch_number", Code: "inventory.batch_invalid", Message: "No batch B9."},
		problem.FieldError{Field: "items[0].material_id", Code: "inventory.batch_invalid", Message: "Not named."})
	rename := func(field string) (string, bool) {
		rest, ok := strings.CutPrefix(field, "items[1]")
		return "items[4]" + rest, ok
	}
	for _, err := range []error{refused, fmt.Errorf("shipping: goods issue: %w", refused)} {
		t.Run(err.Error(), func(t *testing.T) {
			var got *problem.Problem
			if !errors.As(problem.Renamed(err, rename), &got) {
				t.Fatalf("got %v, want a problem", err)
			}
			if got.Code != refused.Code || got.Status != refused.Status || got.Detail != refused.Detail {
				t.Errorf("problem: got %d %s %q, want %d %s %q", got.Status, got.Code, got.Detail, refused.Status, refused.Code, refused.Detail)
			}
			want := problem.FieldError{Field: "items[4].batch_number", Code: "inventory.batch_invalid", Message: "No batch B9."}
			if len(got.Errors) != 1 || got.Errors[0] != want {
				t.Errorf("field errors: got %v, want only %v", got.Errors, want)
			}
			if len(refused.Errors) != 2 || refused.Errors[0].Field != "items[1].batch_number" {
				t.Errorf("the original's field errors: got %v, want them unchanged", refused.Errors)
			}
		})
	}
	plain := errors.New("connection lost")
	if got := problem.Renamed(plain, rename); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("an error without a problem: got %v, want it as it is", got)
	}
}
