package assets_test

import (
	"fmt"
	"testing"
)

// heldForSale is a PATCH body classifying the asset as held for sale in
// the ledger from 2026's period.
func heldForSale(ledger any, period int) string {
	return fmt.Sprintf(`{"terms": [{"ledger_id": "%v", "held_for_sale_from": {"fiscal_year": 2026, "period": %d}}]}`, ledger, period)
}

// heldFrom is the asset's classification in its first ledger, or "none".
func heldFrom(a map[string]any) string {
	h, ok := a["terms"].([]any)[0].(map[string]any)["held_for_sale"].(map[string]any)
	if !ok {
		return "none"
	}
	return fmt.Sprint(h["fiscal_year"], "/", h["period"], " ", h["from"])
}

// TestHeldForSale (T57k): the mill, booked through June in 0L, is
// classified as held for sale from August, then moved to September while
// neither is booked; July and August depreciate, September and later do
// not, the useful life still elapsing; a booked period, a classification
// whose period is booked, a special period and a ledger without terms are
// refused, and the same period again changes nothing.
func TestHeldForSale(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	path := "/v1/assets/" + mill
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	_, tag := e.Must(200, "GET", path, "", "")
	for name, c := range map[string]struct {
		status     int
		code, body string
	}{
		"a booked period":   {422, "assets.change_not_prospective", heldForSale(e.l0, 6)},
		"a special period":  {422, "assets.special_period", heldForSale(e.l0, 13)},
		"a ledger not kept": {422, "ledger.ledger_not_kept", heldForSale(e.l3, 8)},
	} {
		t.Run(name, func(t *testing.T) {
			e.In(t).Problem(c.status, c.code, "PATCH", path, c.body, tag)
		})
	}
	a, tag := e.Must(200, "PATCH", path, heldForSale(e.l0, 8), tag)
	want(t, "held for sale from August", heldFrom(a), "2026/8 2026-08-01")
	same, sameTag := e.Must(200, "PATCH", path, heldForSale(e.l0, 8), tag)
	want(t, "the same period again", fmt.Sprint(sameTag, " ", heldFrom(same)), tag+" 2026/8 2026-08-01")
	_, run = e.start(t, e.l0, 7)
	want(t, "July's run", runOf(e.carry(t, run)), "posted 1 1 0 10.00; 1 posted 1 1 0 10.00")
	_, tag = e.Must(200, "GET", path, "", "")
	a, _ = e.Must(200, "PATCH", path, heldForSale(e.l0, 9), tag)
	want(t, "moved to September", heldFrom(a), "2026/9 2026-09-01")
	s, _ := e.Must(200, "GET", path+"/schedule?fiscal_year=2026", "", "")
	want(t, "the schedule", schedule(s, 6), "6 10.00 10.00 1190.00; 7 10.00 10.00 1180.00; 8 10.00 0.00 1170.00; 9 0.00 0.00 1170.00; "+
		"10 0.00 0.00 1170.00; 11 0.00 0.00 1170.00; 12 0.00 0.00 1170.00 | 30.00 20.00")
	for _, c := range []struct {
		period int
		run    string
	}{{8, "posted 1 1 0 10.00; 1 posted 1 1 0 10.00"}, {9, "posted 1 0 0 0.00; 1 posted 1 0 0 0.00"}} {
		_, run = e.start(t, e.l0, c.period)
		want(t, fmt.Sprint("period ", c.period, "'s run"), runOf(e.carry(t, run)), c.run)
	}
	_, tag = e.Must(200, "GET", path, "", "")
	e.Problem(422, "assets.change_not_prospective", "PATCH", path, heldForSale(e.l0, 10), tag)
	a, _ = e.Must(200, "PATCH", path, `{"description": "Mill, for sale"}`, tag)
	want(t, "a later change keeps the classification", fmt.Sprint(a["description"], " ", heldFrom(a)), "Mill, for sale 2026/9 2026-09-01")
}

// TestTransferReceiverDepreciatesItsMonth (T57k; po:2026-11-09, B11):
// half the mill, whose June is booked, moves to another machine valued
// in June; the half that stays depreciates June on the mill and the half
// moved on the receiver, so no month is lost: a repeat run of June posts
// both halves, 10.00 in all.
func TestTransferReceiverDepreciatesItsMonth(t *testing.T) {
	e := newRuns(t)
	mill, _ := e.depreciable(t, "Mill")
	_, run := e.start(t, e.l0, 6)
	e.carry(t, run)
	press, _ := e.create(t, fmt.Sprintf(`{"legal_entity_id": %q, "class_code": "MACHINES", "description": "Press", "cost_center_id": %q}`, e.DE, e.c.Admin))
	e.transfer(t, mill, fmt.Sprintf(`"kind": "asset", "to_asset_id": %q, "share": {"percent": "50"}, "value_date": "2026-06-12"`, press["id"]))
	for name, id := range map[string]any{"the mill": mill, "the press": press["id"]} {
		s, _ := e.Must(200, "GET", fmt.Sprint("/v1/assets/", id, "/schedule?fiscal_year=2026"), "", "")
		want(t, name+"'s June", schedule(s, 6)[:15], "6 5.00 0.00 595")
	}
	_, run = e.start(t, e.l0, 6)
	want(t, "June's repeat run", runOf(e.carry(t, run)), "posted 2 2 0 10.00; 1 posted 2 2 0 10.00")
}
