package payments

import (
	"context"
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
