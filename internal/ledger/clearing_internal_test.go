package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestClearingLinesNamesTheFirstUnbalancedPartner: when several partners'
// items fail to net to zero, the refusal names the partner first in id
// order, the same on every run (map iteration order varies).
func TestClearingLinesNamesTheFirstUnbalancedPartner(t *testing.T) {
	legalEntity, ledgerID := uuid.New(), uuid.New()
	var items []item
	for i := 5; i >= 1; i-- {
		partner := uuid.MustParse("00000000-0000-0000-0000-00000000000" + string(rune('0'+i)))
		items = append(items, item{found: true, open: true, recon: true, legalEntity: legalEntity, ledger: ledgerID,
			account: uuid.New(), partner: &partner, tc: 100, lc: 100, gc: 100, currency: "EUR"})
	}
	want := "partner 00000000-0000-0000-0000-000000000001 "
	for run := range 200 {
		_, err := clearingLines(items, legalEntity, nil)
		var p *problem.Problem
		if !errors.As(err, &p) {
			t.Fatalf("run %d: got %v, want a problem", run, err)
		}
		if !strings.Contains(p.Detail, want) {
			t.Fatalf("run %d: got %q, want it to name %q", run, p.Detail, want)
		}
	}
}

// TestMigratedItemsNamesTheFirstUnbalancedPartner: a migrated clearing
// refuses unbalanced items the same way, naming the partner first in id
// order on every run.
func TestMigratedItemsNamesTheFirstUnbalancedPartner(t *testing.T) {
	legalEntity := uuid.New()
	var items []item
	for i := 5; i >= 1; i-- {
		partner := uuid.MustParse("00000000-0000-0000-0000-00000000000" + string(rune('0'+i)))
		items = append(items, item{found: true, open: true, recon: true, legalEntity: legalEntity, account: uuid.New(),
			partner: &partner, tc: 100, lc: 100, currency: "EUR"})
	}
	want := "partner 00000000-0000-0000-0000-000000000001 "
	for run := range 200 {
		_, err := migratedItems(items, legalEntity, map[string]int{"EUR": 2})
		var p *problem.Problem
		if !errors.As(err, &p) {
			t.Fatalf("run %d: got %v, want a problem", run, err)
		}
		if !strings.Contains(p.Detail, want) {
			t.Fatalf("run %d: got %q, want it to name %q", run, p.Detail, want)
		}
	}
}
