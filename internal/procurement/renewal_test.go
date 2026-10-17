package procurement_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Contract renewal (T47g): a draft successor continues a contract from the
// day after it ends, once at a time.

const renewal = `{"valid_to": "2027-12-31", "target_value": {"amount": "2000.00", "currency": "EUR"}}`

// activeContract is a released contract of 2026 and its path.
func (e env) activeContract(t *testing.T) (map[string]any, string) {
	t.Helper()
	c := e.contract(t, e.Supplier.String(), "")
	path := "/v1/contracts/" + id(c)
	return e.act(t, path, "release", ""), path
}

func TestRenewContract(t *testing.T) {
	e := setup(t)
	c, path := e.activeContract(t)
	r, _ := e.Must(201, "POST", path+":renew", renewal, "*")
	want(t, "renewal", r, map[string]any{"status": "draft", "valid_from": "2027-01-01", "valid_to": "2027-12-31", "renewal_of_id": c["id"],
		"supplier_id": c["supplier_id"], "currency": "EUR"})
	want(t, "renewal target", r["target_value"].(map[string]any), map[string]any{"amount": "2000.00"})
	if r["document_number"] == c["document_number"] {
		t.Errorf("renewal number: got %v, want a number of its own", r["document_number"])
	}
	if got, want := len(r["items"].([]any)), len(c["items"].([]any)); got != want {
		t.Errorf("renewal items: got %d, want %d", got, want)
	}
	got, _ := e.Must(200, "GET", path, "", "")
	want(t, "renewed contract", got, map[string]any{"renewed_by_id": r["id"], "renewal_of_id": nil})
	e.Problem(409, "procurement.contract_renewed", "POST", path+":renew", renewal, "*")
	rpath := "/v1/contracts/" + id(r)
	e.Problem(422, "procurement.invalid_renewal", "PATCH", rpath, `{"valid_from": "2027-02-01"}`, "*")
	// A cancelled renewal does not count.
	e.act(t, rpath, "cancel", "")
	got, _ = e.Must(200, "GET", path, "", "")
	want(t, "contract of a cancelled renewal", got, map[string]any{"renewed_by_id": nil})
	r, _ = e.Must(201, "POST", path+":renew", renewal, "*")
	before := len(events(t, e, "contract_activated"))
	e.act(t, "/v1/contracts/"+id(r), "release", "")
	activated := events(t, e, "contract_activated")
	if len(activated) != before+1 {
		t.Fatalf("ContractActivated events: got %d, want %d", len(activated), before+1)
	}
	want(t, "activated renewal", activated[len(activated)-1], map[string]any{"contract_id": r["id"], "renewal_of_id": c["id"]})
	want(t, "activated contract", activated[0], map[string]any{"renewal_of_id": nil})
}

// TestRenewalChecked: what can be renewed, and how.
func TestRenewalChecked(t *testing.T) {
	e := setup(t)
	_, path := e.activeContract(t)
	for name, c := range map[string]struct {
		body string
		code string
	}{
		"ending before it starts": {`{"valid_to": "2026-12-31", "target_value": {"amount": "1.00", "currency": "EUR"}}`, "procurement.invalid_validity"},
		"in another currency":     {`{"valid_to": "2027-12-31", "target_value": {"amount": "1.00", "currency": "USD"}}`, "procurement.currency_mismatch"},
		"without a target":        {`{"valid_to": "2027-12-31", "target_value": {"amount": "0.00", "currency": "EUR"}}`, "procurement.invalid_amount"},
	} {
		t.Run(name, func(t *testing.T) {
			e := e.in(t)
			e.Problem(422, c.code, "POST", path+":renew", c.body, "*")
		})
	}
	draft := e.contract(t, e.Supplier.String(), "")
	e.Problem(409, "procurement.invalid_status", "POST", "/v1/contracts/"+id(draft)+":renew", renewal, "*")
	// An expired contract is renewed too.
	expired, epath := e.activeContract(t)
	pgtest.Exec(t, `UPDATE procurement.contract SET status = 'expired', version = version + 1 WHERE id = $1`, uuidOf(expired))
	if r, _ := e.Must(201, "POST", epath+":renew", renewal, "*"); r["renewal_of_id"] != expired["id"] {
		t.Errorf("renewal of an expired contract: got %v, want %v", r["renewal_of_id"], expired["id"])
	}
	// A blocked supplier renews nothing.
	pgtest.Exec(t, `UPDATE partner.supplier_company SET is_posting_blocked = true, version = version + 1 WHERE partner_id = $1`, e.Supplier)
	e.Problem(422, "procurement.supplier_not_usable", "POST", path+":renew", renewal, "*")
}

// TestContractsExpiringBefore: the list finds the contracts to renew.
func TestContractsExpiringBefore(t *testing.T) {
	e := setup(t)
	c, path := e.activeContract(t)
	r, _ := e.Must(201, "POST", path+":renew", renewal, "*")
	page, _ := e.Must(200, "GET", "/v1/contracts?expiring_before=2027-01-01", "", "")
	var ids []any
	for _, it := range page["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"])
	}
	if len(ids) != 1 || ids[0] != c["id"] {
		t.Errorf("contracts expiring before 2027: got %v, want only %v (not the renewal %v)", ids, c["id"], r["id"])
	}
	// As the alert pass (T47g) records it.
	pgtest.Exec(t, `UPDATE procurement.contract SET expiry_alerted_on = '2026-12-01', version = version + 1 WHERE id = $1`, uuidOf(c))
	got, _ := e.Must(200, "GET", path, "", "")
	want(t, "alerted contract", got, map[string]any{"expiry_alerted_on": "2026-12-01"})
}

func TestRenewFailures(t *testing.T) {
	e := setup(t)
	h := endpoints()
	c, _ := e.activeContract(t)
	call := func(ctx context.Context) error {
		_, err := h.RenewContract(ctx, &gen.ContractRenew{ValidTo: dateOf("2027-12-31"), TargetValue: gen.Money{Amount: "10.00", Currency: "EUR"}},
			gen.RenewContractParams{ID: uuidOf(c), IfMatch: "*"})
		return err
	}
	if err := call(context.Background()); err == nil {
		t.Error("outside a unit of work: got nil error, want an error")
	}
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "renew", call)
	pgtest.CheckErr(t, apitest.InRequest(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.RenewContract(ctx, &gen.ContractRenew{ValidTo: dateOf("2027-12-31"), TargetValue: gen.Money{Amount: "10.00", Currency: "EUR"}},
			gen.RenewContractParams{ID: uuid.New(), IfMatch: "*"})
		return err
	}), "request.not_found")
}
