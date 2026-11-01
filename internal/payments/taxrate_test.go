package payments_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
)

// rerateV1 changes company 2000's tax code V1 from 19% to 16% on
// 2026-04-01, a rate written ahead (T61a).
func rerateV1(t *testing.T, e env) {
	t.Helper()
	fixture.TaxCodes(t, e.Tenant, downPaymentDoc(t), config.TaxCode{Code: "V1", Description: "Input tax, standard rate", Components: []config.TaxComponent{
		{Key: config.LegacyComponent, AccountKey: config.KeyInput, Account: "140000",
			Rates: []config.TaxRate{{RatePercent: "19", ValidFrom: "2026-01-01", ValidTo: "2026-03-31"}, {RatePercent: "16", ValidFrom: "2026-04-01"}}}}})
}

// requestExclusion is the exclusion of a request in a run, as listed.
func (e env) requestExclusion(t *testing.T, run, request map[string]any) map[string]any {
	t.Helper()
	page, _ := e.Must(200, "GET", "/v1/payment-runs/"+id(run)+"/exclusions", "", "")
	for _, x := range page["items"].([]any) {
		if m := x.(map[string]any); m["down_payment_request_id"] == id(request) {
			return m
		}
	}
	t.Fatalf("exclusions of run %s: got %v, want one of request %s", id(run), page["items"], id(request))
	return nil
}

// TestDownPaymentTaxRate: a down payment request's stated tax is checked
// at the run date's rate (ADR-0049 §4). A request whose tax no longer
// matches is left out with tax_code_invalid, naming the request, its code
// and both rates; one whose tax still matches, or of a code ended but with
// no tax, is paid; a proposed run whose run date's rate changed since is
// refused at release with both rates; one whose code ended names it not
// valid.
func TestDownPaymentTaxRate(t *testing.T) {
	t.Run("rate changed before the proposal", func(t *testing.T) {
		e := downPaymentSetup(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		rerateV1(t, e)
		taxed := e.request(t, e.Supplier, "119.00", "2026-03-15", `, "tax_code": "V1"`) // at 19%: 19.00
		later := e.request(t, e.Supplier, "116.00", "2026-04-02", `, "tax_code": "V1"`) // at 16%: 16.00
		run := e.run(t, "2026-04-15", "2026-04-15")
		x := e.requestExclusion(t, run, taxed)
		got := fmt.Sprintf("%v %v %v %v %v", x["reason_code"], x["down_payment_request_number"], x["tax_code"], x["request_tax_rate_percent"], x["run_date_tax_rate_percent"])
		if want := fmt.Sprintf("tax_code_invalid %v V1 19 16", taxed["document_number"]); got != want {
			t.Errorf("exclusion: got %s, want %s", got, want)
		}
		pays := byKind(e.payments(t, run))
		if p, ok := pays[e.Supplier.String()+" down_payment"]; len(pays) != 1 || !ok || p["down_payment_request_id"] != id(later) {
			t.Errorf("payments: got %v, want the request still at its rate, %s", pays, id(later))
		}
	})
	t.Run("rate changed after the proposal", func(t *testing.T) {
		e := downPaymentSetup(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		e.request(t, e.Supplier, "119.00", "2026-03-15", `, "tax_code": "V1"`)
		run := e.run(t, "2026-04-15", "2026-04-15")
		rerateV1(t, e)
		_, tag := e.Must(200, "GET", "/v1/payment-runs/"+id(run), "", "")
		p := e.As(e.Approver).Problem(409, "payments.proposal_outdated", "POST", "/v1/payment-runs/"+id(run)+":release", "", tag)
		if want := "has tax code V1 (19% when requested, 16% on the run date)"; !strings.Contains(fmt.Sprint(p["detail"]), want) {
			t.Errorf("detail: got %v, want it to name %q", p["detail"], want)
		}
	})
	t.Run("code ended", func(t *testing.T) {
		e := downPaymentSetup(t)
		e.bankAccount(t, e.Supplier, "DE89370400440532013000", "Supplier", "2026-01-01", "", "create")
		taxed := e.request(t, e.Supplier, "119.00", "2026-03-15", `, "tax_code": "V1"`)
		untaxed := e.request(t, e.Supplier, "0.01", "2026-03-15", `, "tax_code": "V1"`) // 19% of 0.01 included: 0.00
		endV1(t, e)
		run := e.run(t, "2026-04-15", "2026-04-15")
		x := e.requestExclusion(t, run, taxed)
		if got := fmt.Sprintf("%v %v %v", x["tax_code"], x["request_tax_rate_percent"], x["run_date_tax_rate_percent"]); got != "V1 19 <nil>" {
			t.Errorf("exclusion: got %s, want V1 19 and no run date rate", got)
		}
		if _, ok := byKind(e.payments(t, run))[e.Supplier.String()+" down_payment"]; !ok {
			t.Errorf("payments: got none of request %s, which states no tax, want it paid", id(untaxed))
		}
	})
}
