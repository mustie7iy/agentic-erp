package assets_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/assets"
)

// classTotals reads the class totals the consolidation context reads of
// company 2000 in the ledger and year (ConsolidationReads) as the history
// sheet's rows by class: the years before (period 0) as the opening
// values, the year's periods summed into its columns, in s.expected's
// form. Every period read is one of the year's (1-16), in order.
func (s sheetScene) classTotals(t *testing.T, ledger uuid.UUID, year int16) string {
	t.Helper()
	var totals []assets.ClassTotal
	if err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		var err error
		totals, err = assets.ConsolidationReads{}.ClassTotals(ctx, s.DE, ledger, year)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	type sums struct{ openCost, acq, ret, tr, openAcc, dep, retAcc, trAcc int64 }
	byClass, last := map[string]*sums{}, map[string]int16{}
	var classes []string
	for _, c := range totals {
		v := byClass[c.Class]
		if v == nil {
			v, classes = &sums{}, append(classes, c.Class)
			byClass[c.Class], last[c.Class] = v, -1
		}
		if c.Period <= last[c.Class] || c.Period > 16 {
			t.Errorf("class %s: period %d after %d, want the periods of the year in order", c.Class, c.Period, last[c.Class])
		}
		last[c.Class] = c.Period
		if c.Period == 0 {
			v.openCost += int64(c.Acquisitions + c.Retirements + c.Transfers)
			v.openAcc += int64(c.Depreciation + c.ImpairmentLosses + c.ImpairmentReversals + c.RetirementsAccumulated + c.TransfersAccumulated)
			continue
		}
		v.acq, v.ret, v.tr = v.acq+int64(c.Acquisitions), v.ret+int64(c.Retirements), v.tr+int64(c.Transfers)
		v.dep, v.retAcc, v.trAcc = v.dep+int64(c.Depreciation+c.ImpairmentLosses+c.ImpairmentReversals), v.retAcc+int64(c.RetirementsAccumulated),
			v.trAcc+int64(c.TransfersAccumulated)
	}
	out := make([]string, 0, len(classes))
	for _, class := range classes {
		v := byClass[class]
		closeCost, closeAcc := v.openCost+v.acq+v.ret+v.tr, v.openAcc+v.dep+v.retAcc+v.trAcc
		cols := make([]string, 0, 12)
		for _, c := range []int64{v.openCost, v.acq, v.ret, v.tr, closeCost, v.openAcc, v.dep, v.retAcc, v.trAcc, closeAcc, v.openCost + v.openAcc,
			closeCost + closeAcc} {
			cols = append(cols, decimal(c))
		}
		out = append(out, class+": "+strings.Join(cols, " "))
	}
	return strings.Join(out, "; ")
}

// TestConsolidationClassTotals: the class totals the consolidation
// context reads (ADR-0057 As built, the consolidated sheet) are the
// history sheet's by class for each ledger and year, each period of the
// year in its own row: read from the class period totals every posting
// keeps, which equal the movements' sums by period (totalsDiffer).
func TestConsolidationClassTotals(t *testing.T) {
	s := newSheetScene(t)
	for _, c := range []struct {
		ledger uuid.UUID
		year   int16
	}{{s.l0, 2026}, {s.l0, 2027}, {s.l2, 2026}, {s.l2, 2027}} {
		t.Run(fmt.Sprint(c.ledger, " ", c.year), func(t *testing.T) {
			want(t, "the class totals", s.classTotals(t, c.ledger, c.year), "LAND: "+s.expected(t, c.ledger, int(c.year), s.land...)+"; MACHINES: "+
				s.expected(t, c.ledger, int(c.year), s.machines...))
		})
	}
	want(t, "class period totals that differ from their movements' sums", totalsDiffer(t, s.Tenant.ID), 0)
	read := func(ctx context.Context) error {
		_, err := assets.ConsolidationReads{}.ClassTotals(ctx, s.DE, s.l0, 2026)
		return err
	}
	if err := read(context.Background()); err == nil || !strings.Contains(err.Error(), "outside a unit of work") {
		t.Errorf("outside a unit of work: got %v, want it refused", err)
	}
	apitest.EveryFailure(t, s.Tenant, s.Tenant.Human, "class totals", read)
}
