package consolidation

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestFrozenChecks: a run whose member's origin, ownership, first year or
// period or investment account differs from its frozen acquisition fails
// with consolidation.acquisition_changed, one whose parent's investment
// differs with consolidation.investment_changed; the same ownership
// written otherwise ("100" and "100.0000") is not a change. A member of a
// version activated before the origin was required reads as of no origin.
func TestFrozenChecks(t *testing.T) {
	account, by := uuid.New(), uuid.New()
	year, period := int16(2026), int16(11)
	frozen := acquired{origin: config.OriginIncorporated, ownership: "100.0000", firstYear: 2026, firstP: 11, investmentAccount: account,
		investment: 110500, frozenBy: &by}
	m := Member{CompanyCode: "1000", FirstYear: &year, FirstPeriod: &period}
	gm := config.GroupMember{Company: "1000", OwnershipPercent: "100", Origin: config.OriginIncorporated}
	other, otherYear, otherPeriod := uuid.New(), int16(2025), int16(10)
	for _, c := range []struct {
		name     string
		change   func(*Member, *config.GroupMember, *uuid.UUID)
		invested int64
		want     string
	}{
		{"unchanged", func(*Member, *config.GroupMember, *uuid.UUID) {}, 110500, ""},
		{"another origin", func(_ *Member, g *config.GroupMember, _ *uuid.UUID) { g.Origin = config.OriginAcquired }, 110500, "consolidation.acquisition_changed"},
		{"another ownership", func(_ *Member, g *config.GroupMember, _ *uuid.UUID) { g.OwnershipPercent = "90" }, 110500, "consolidation.acquisition_changed"},
		{"another first year", func(m *Member, _ *config.GroupMember, _ *uuid.UUID) { m.FirstYear = &otherYear }, 110500, "consolidation.acquisition_changed"},
		{"another first period", func(m *Member, _ *config.GroupMember, _ *uuid.UUID) { m.FirstPeriod = &otherPeriod }, 110500,
			"consolidation.acquisition_changed"},
		{"another investment account", func(_ *Member, _ *config.GroupMember, a *uuid.UUID) { *a = other }, 110500, "consolidation.acquisition_changed"},
		{"another investment", func(*Member, *config.GroupMember, *uuid.UUID) {}, 100000, "consolidation.investment_changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mm, g, acc := m, gm, account
			c.change(&mm, &g, &acc)
			err := frozenChecks(frozen, mm, g, Account{ID: acc}, money.Minor(c.invested), 2, "EUR")
			got := ""
			if err != nil {
				got = problem.From(err).Code
			}
			if got != c.want {
				t.Errorf("frozenChecks: got %q (%v), want %q", got, err, c.want)
			}
		})
	}
	if got := originText(""); got != "its origin is not configured" {
		t.Errorf("no origin: got %q, want it said", got)
	}
}
