package consolidation

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestFinalizeUntranslated: a computed run with a company whose period it
// never read, which the schema does not rule out, is refused as not
// locked and counts as moved, never dereferenced.
func TestFinalizeUntranslated(t *testing.T) {
	r := Run{ID: uuid.New(), FiscalYear: 2026, Period: 11}
	ms := []Member{{LegalEntityID: uuid.New(), CompanyCode: "1000"}}
	err := locked(r, ms, map[periodKey]Period{})
	if p := problem.From(err); p.Code != "consolidation.period_not_locked" || !strings.Contains(p.Detail, "company 1000 (none when the run read its figures, none now)") {
		t.Errorf("locked: got %v, want company 1000 not locked, none then and now", err)
	}
	if got := len(moved(r, ms, map[periodKey]Period{})); got != 1 {
		t.Errorf("moved: got %d, want 1", got)
	}
}
