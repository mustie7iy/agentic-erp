package payments

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/partner"
)

// TestRouteByTheCompanysOrder: a supplier is paid by the first method of
// the company's run order that it may be paid by, whatever the order its
// own list has them in (SAP specialist 2026-11-09, A4(c): the supplier's
// methods only restrict the run's); one no method of the order pays is
// not routed, and a run for one house bank routes everyone through it.
func TestRouteByTheCompanysOrder(t *testing.T) {
	main, second := uuid.New(), uuid.New()
	byMethod := router{byMethod: true, methods: []string{"T", "C"}, banks: map[string]uuid.UUID{"T": main, "C": second}}
	for _, c := range []struct {
		name    string
		rt      router
		allowed []string
		method  string
		bank    uuid.UUID
		routed  bool
	}{
		{"the supplier's own order differs", byMethod, []string{"C", "T"}, "T", main, true},
		{"only the second allowed", byMethod, []string{"C"}, "C", second, true},
		{"a method the run does not pay by comes first", byMethod, []string{"X", "C"}, "C", second, true},
		{"none allowed", byMethod, nil, "", uuid.Nil, false},
		{"none the run pays by", byMethod, []string{"X"}, "", uuid.Nil, false},
		{"a run for one house bank", router{}, []string{"C"}, "", uuid.Nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, routed := c.rt.route(partner.Payee{PaymentMethods: c.allowed})
			if routed != c.routed {
				t.Fatalf("routed: got %v, want %v", routed, c.routed)
			}
			if got := methodOf(v.method); got != c.method {
				t.Errorf("method: got %q, want %q", got, c.method)
			}
			if got := bankOf(Run{}, Payment{HouseBankID: v.bank}); (got == nil) != (c.bank == uuid.Nil) || (got != nil && *got != c.bank) {
				t.Errorf("bank: got %v, want %v", got, c.bank)
			}
		})
	}
}

// TestPayingBankOfNeither: a payment that names no house bank in a run
// without one is broken data (a run proposed without a bank names one on
// each payment), an error rather than a guess; the database is not asked.
func TestPayingBankOfNeither(t *testing.T) {
	_, err := payingBank(context.Background(), nil, Run{ID: uuid.New()}, Payment{ID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "names no house bank") {
		t.Errorf("got %v, want the payment refused", err)
	}
}

// TestMessageIDsOfLongCodes: with the longest codes a run's files still
// have message ids of their own within pain.001's 35 characters, ending in
// their bank and method, and two runs whose ids share a long prefix differ.
func TestMessageIDsOfLongCodes(t *testing.T) {
	transfer, cheque := "T", "S"
	bank := houseBank{Code: "HSBC1"}
	ids := map[string]bool{}
	for _, number := range []string{"PAYRUN-26-1234567", "PAYRUN-26-1234568"} {
		for _, m := range []*string{&transfer, &cheque} {
			id := fileGroup{bank: bank, v: via{method: m}}.messageID("ACMEGMBH01", Run{DocumentNumber: number})
			if len(id) > maxMessageID {
				t.Errorf("%s: got %d characters, want at most %d", id, len(id), maxMessageID)
			}
			if !strings.HasSuffix(id, "-HSBC1-"+*m) {
				t.Errorf("%s: got no bank and method at its end, want -HSBC1-%s", id, *m)
			}
			ids[id] = true
		}
	}
	if len(ids) != 4 {
		t.Errorf("message ids: got %d distinct, want 4: %v", len(ids), ids)
	}
	if got := (fileGroup{bank: bank, v: via{method: &transfer}}).messageID("2000", Run{DocumentNumber: "PR1"}); got != "2000-PR1-HSBC1-T" {
		t.Errorf("short id: got %s, want 2000-PR1-HSBC1-T", got)
	}
}

// TestRouteItemsFixingAMethod: a supplier's items that fix a payment
// method go by that method alone, through the bank ranked for it, or are
// left out with the reason, never routed by another method (ADR-0038;
// product owner 2026-11-10), whether or not the supplier's own methods
// allow it: those restrict the run order alone (SAP specialist, A4(c)).
// A run for one house bank pays by no method and cannot honour one.
// Items that fix none are routed exactly as the
// company's run order routes the supplier (route), unrouted with
// NoHouseBank.
func TestRouteItemsFixingAMethod(t *testing.T) {
	main, second := uuid.New(), uuid.New()
	byMethod := router{byMethod: true, methods: []string{"T", "C"}, banks: map[string]uuid.UUID{"T": main, "C": second},
		active: map[string]bool{"T": true, "C": true, "U": true}}
	for _, c := range []struct {
		name    string
		rt      router
		allowed []string
		fixes   string
		method  string
		bank    uuid.UUID
		reason  string
	}{
		{"the method fixed, though the run order has another first", byMethod, []string{"T", "C"}, "C", "C", second, ""},
		{"a method the supplier's own methods lack", byMethod, []string{"T"}, "C", "C", second, ""},
		{"a method fixed for a supplier without methods", byMethod, nil, "T", "T", main, ""},
		{"a method no bank is ranked for", byMethod, []string{"T", "U"}, "U", "", uuid.Nil, NoHouseBank},
		{"a method no bank is ranked for, nor the supplier's", byMethod, []string{"T"}, "U", "", uuid.Nil, NoHouseBank},
		{"a method the company no longer has", byMethod, []string{"T", "X"}, "X", "", uuid.Nil, MethodNotActive},
		{"a method neither the company nor the supplier has", byMethod, []string{"T"}, "X", "", uuid.Nil, MethodNotActive},
		{"a run for one house bank", router{}, []string{"C"}, "C", "", uuid.Nil, FixedMethodInBankRun},
		{"no method fixed", byMethod, []string{"C", "T"}, "", "T", main, ""},
		{"no method fixed, none the run pays by", byMethod, []string{"X"}, "", "", uuid.Nil, NoHouseBank},
		{"no method fixed, a run for one house bank", router{}, nil, "", "", uuid.Nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			payee := partner.Payee{PaymentMethods: c.allowed}
			v, reason := c.rt.items(payee, c.fixes)
			if reason != c.reason {
				t.Fatalf("reason: got %q, want %q", reason, c.reason)
			}
			if got := methodOf(v.method); got != c.method {
				t.Errorf("method: got %q, want %q", got, c.method)
			}
			if got := bankOf(Run{}, Payment{HouseBankID: v.bank}); (got == nil) != (c.bank == uuid.Nil) || (got != nil && *got != c.bank) {
				t.Errorf("bank: got %v, want %v", got, c.bank)
			}
			if want := c.fixes != "" && c.reason == ""; v.fixed != want {
				t.Errorf("fixed: got %v, want %v", v.fixed, want)
			}
			if c.fixes == "" { // as the run order alone routes the supplier
				byOrder, routed := c.rt.route(payee)
				if methodOf(byOrder.method) != methodOf(v.method) || routed != (reason == "") {
					t.Errorf("by the run order: got method %q routed %v, want %q routed %v", methodOf(v.method), reason == "", methodOf(byOrder.method), routed)
				}
			}
		})
	}
}

// TestItemsWithoutAFixedMethodAreProposedAsBefore: a proposal none of
// whose items fixes a payment method is the proposal it was before items
// could fix one (product owner 2026-11-10: no change for them): the
// candidates pass through byMethod as they are, one per supplier in the
// same order with the same items, so each supplier is paid or excluded
// once. A supplier whose items fix methods is split, those that fix none
// first and then each method by code, without touching the others.
func TestItemsWithoutAFixedMethodAreProposedAsBefore(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	item := func(line int32, method string) Item { return Item{LineNo: line, Amount: 100, method: method} }
	plain := []candidate{{supplier: a, items: []Item{item(1, ""), item(2, "")}}, {supplier: b, items: []Item{item(3, "")}}}
	got := byMethod(plain)
	if len(got) != len(plain) {
		t.Fatalf("candidates without a fixed method: got %d, want %d, unchanged", len(got), len(plain))
	}
	for i := range plain {
		if got[i].supplier != plain[i].supplier || got[i].method != "" || !slices.Equal(lines(got[i]), lines(plain[i])) {
			t.Errorf("candidate %d: got %+v, want %+v, unchanged", i, got[i], plain[i])
		}
	}
	mixed := append(slices.Clone(plain), candidate{supplier: c, items: []Item{item(4, "S"), item(5, ""), item(6, "C"), item(7, "S"), item(8, "")}})
	var split []string
	for _, cand := range byMethod(mixed) {
		split = append(split, fmt.Sprint(cand.supplier.String()[:8], " ", cand.method, " ", lines(cand)))
	}
	want := []string{fmt.Sprint(a.String()[:8], "  [1 2]"), fmt.Sprint(b.String()[:8], "  [3]"), fmt.Sprint(c.String()[:8], "  [5 8]"),
		fmt.Sprint(c.String()[:8], " C [6]"), fmt.Sprint(c.String()[:8], " S [4 7]")}
	if !slices.Equal(split, want) {
		t.Errorf("candidates of a supplier whose items fix methods: got %q, want %q", split, want)
	}
}

// lines are the candidate's items' line numbers, in order.
func lines(c candidate) []int32 {
	out := make([]int32, len(c.items))
	for i, it := range c.items {
		out[i] = it.LineNo
	}
	return out
}
