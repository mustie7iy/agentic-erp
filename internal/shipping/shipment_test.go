package shipping_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
)

// shipment is a shipment as a transport system reports it, its freight
// cost "amount currency".
func shipment(number, carrier, freight string) shipping.ShipmentInput {
	f := strings.Fields(freight)
	return shipping.ShipmentInput{Number: number, Carrier: carrier, FreightAmount: f[0], Currency: f[1]}
}

// recorded records the shipment on the delivery, committed, as the
// exchange with transport systems does.
func (e env) recorded(t *testing.T, h *shipping.Endpoints, id string, in shipping.ShipmentInput) error {
	t.Helper()
	return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.RecordShipment(ctx, uuid.MustParse(id), in)
		return err
	})
}

// TestRecordShipment (S2r): a transport system's shipment is recorded on
// a created delivery, its freight cost in minor units of the currency it
// names, moving the version, audited and published; a second one is
// refused, and the first stays. A goods-issued delivery takes its
// shipment too, and stays goods-issued.
func TestRecordShipment(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	d, tag := e.Must(201, "POST", deliveries, e.create(order, "1 2 BOX"), "")
	id := d["id"].(string)
	if err := e.recorded(t, h, id, shipment("SH-1001", "NWTRANS", "45.50 CAD")); err != nil {
		t.Fatalf("the shipment: got %v, want it recorded", err)
	}
	got, gotTag := e.Must(200, "GET", deliveries+"/"+id, "", "")
	s, _ := got["shipment"].(map[string]any)
	if s["shipment_number"] != "SH-1001" || s["carrier_code"] != "NWTRANS" || money(s["freight_cost"]) != "45.50 CAD" || s["recorded_at"] == nil {
		t.Errorf("the delivery's shipment: got %v, want SH-1001 by NWTRANS at 45.50 CAD, recorded", s)
	}
	if gotTag == tag || got["status"] != "created" {
		t.Errorf("the delivery: got %v at %s, want created at a version after %s", got["status"], gotTag, tag)
	}
	events := e.events(t, id)
	if last, _ := events[len(events)-1]["shipment"].(map[string]any); last["shipment_number"] != "SH-1001" || money(last["freight_cost"]) != "45.50 CAD" {
		t.Errorf("the last event's shipment: got %v, want SH-1001 at 45.50 CAD", last)
	}
	if a := e.audits(t, id, "shipping.outbound_delivery.record_shipment"); len(a) != 1 {
		t.Errorf("audit records of the shipment: got %d, want 1", len(a))
	}
	err := e.recorded(t, h, id, shipment("SH-1002", "NWTRANS", "1.00 CAD"))
	if p := problem.From(err); p.Code != "shipping.shipment_recorded" || p.Status != 409 {
		t.Errorf("a second shipment: got %v, want 409 shipping.shipment_recorded", err)
	}
	if again, _ := e.Must(200, "GET", deliveries+"/"+id, "", ""); again["shipment"].(map[string]any)["shipment_number"] != "SH-1001" {
		t.Errorf("the shipment after a second: got %v, want SH-1001 kept", again["shipment"])
	}
	t.Run("after the goods issue", func(t *testing.T) {
		d, tag := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
		other := d["id"].(string)
		_, tag = e.Must(200, "POST", deliveries+"/"+other+":pick", pick("1 1 BOX"), tag)
		e.issued(t, other, tag)
		if err := e.recorded(t, h, other, shipment("SH-1003", "NWTRANS", "1200 JPY")); err != nil {
			t.Fatalf("the shipment of a goods-issued delivery: got %v, want it recorded", err)
		}
		got, _ := e.Must(200, "GET", deliveries+"/"+other, "", "")
		if got["status"] != "goods_issued" || money(got["shipment"].(map[string]any)["freight_cost"]) != "1200 JPY" {
			t.Errorf("the delivery: got %v with %v, want goods_issued with 1200 JPY", got["status"], got["shipment"])
		}
	})
}

// TestRecordShipmentRefusals (S2r): a shipment number or carrier code
// empty or above 40 characters, an unknown currency, a negative cost and
// an amount without its currency's decimals are refused, naming the field;
// a delivery that is not there is not found.
func TestRecordShipmentRefusals(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := d["id"].(string)
	for name, c := range map[string]struct {
		id         string
		in         shipping.ShipmentInput
		code, path string
	}{
		"an empty number":       {id, shipment("", "NWTRANS", "1.00 CAD"), "shipping.invalid_shipment", "shipment_number"},
		"a number of 41":        {id, shipment(strings.Repeat("S", 41), "NWTRANS", "1.00 CAD"), "shipping.invalid_shipment", "shipment_number"},
		"an empty carrier":      {id, shipment("SH1", "", "1.00 CAD"), "shipping.invalid_shipment", "carrier_code"},
		"a carrier of 41":       {id, shipment("SH1", strings.Repeat("C", 41), "1.00 CAD"), "shipping.invalid_shipment", "carrier_code"},
		"an unknown currency":   {id, shipment("SH1", "NWTRANS", "1.00 XXX"), "shipping.invalid_shipment", "freight_cost.currency"},
		"a negative cost":       {id, shipment("SH1", "NWTRANS", "-1.00 CAD"), "shipping.invalid_shipment", "freight_cost.amount"},
		"three decimals of CAD": {id, shipment("SH1", "NWTRANS", "1.000 CAD"), "request.invalid_amount", "freight_cost.amount"},
		"no delivery":           {uuid.NewString(), shipment("SH1", "NWTRANS", "1.00 CAD"), "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.RecordShipment(ctx, uuid.MustParse(c.id), c.in)
				return err
			})
			p := problem.From(err)
			if c.code == "" {
				if p.Status != 404 {
					t.Errorf("got %v, want 404", err)
				}
				return
			}
			if p.Code != c.code || len(p.Errors) != 1 || p.Errors[0].Field != c.path {
				t.Errorf("got %v naming %v, want %s naming %s", err, p.Errors, c.code, c.path)
			}
		})
	}
	t.Run("outside a unit of work", func(t *testing.T) {
		if _, err := h.RecordShipment(context.Background(), uuid.MustParse(id), shipment("SH1", "NWTRANS", "1.00 CAD")); err == nil {
			t.Error("got nil error, want an error")
		}
	})
	t.Run("database failures", func(t *testing.T) {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "record a shipment", func(ctx context.Context) error {
			_, err := h.RecordShipment(ctx, uuid.MustParse(id), shipment("SH1", "NWTRANS", "1.00 CAD"))
			return err
		})
	})
}

// TestShipmentAndPickInEitherOrder (S2r; ADR-0008): a shipment recorded
// and a person's pick both take the delivery's row lock alone, so either
// waits for the other and both succeed, whichever comes first; no
// deadlock.
func TestShipmentAndPickInEitherOrder(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	anyVersion := func(int32) error { return nil }
	for i, shipmentFirst := range []bool{true, false} {
		d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
		id := uuid.MustParse(d["id"].(string))
		ship := func(ctx context.Context) error {
			_, err := h.RecordShipment(ctx, id, shipment(fmt.Sprintf("SH-%d", i), "NWTRANS", "1.00 CAD"))
			return err
		}
		pickIt := func(ctx context.Context) error {
			_, err := h.Pick(ctx, id, anyVersion, []shipping.PickInput{{ItemNo: 1, Quantity: "1", Unit: "BOX"}})
			return err
		}
		first, second := pickIt, ship
		if shipmentFirst {
			first, second = ship, pickIt
		}
		if a, b := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, first, second); a != nil || b != nil {
			t.Errorf("shipment first %v: got %v and %v, want both done", shipmentFirst, a, b)
		}
		got, _ := e.Must(200, "GET", deliveries+"/"+id.String(), "", "")
		if got["shipment"] == nil || got["picking_status"] != "picked" {
			t.Errorf("shipment first %v: got shipment %v, %v; want both kept", shipmentFirst, got["shipment"], got["picking_status"])
		}
	}
}

// TestShipmentRecordedOnceUnderARace (S2r; ADR-0008): two shipments of one
// delivery recorded at once: the second waits for the first's row lock,
// reads the delivery as the first left it and is refused; the first is
// kept.
func TestShipmentRecordedOnceUnderARace(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := uuid.MustParse(d["id"].(string))
	record := func(number string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			_, err := h.RecordShipment(ctx, id, shipment(number, "NWTRANS", "1.00 CAD"))
			return err
		}
	}
	first, second := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, record("SH-A"), record("SH-B"))
	if first != nil {
		t.Fatalf("the first shipment: got %v, want recorded", first)
	}
	if p := problem.From(second); p.Code != "shipping.shipment_recorded" {
		t.Errorf("the second shipment: got %v, want shipping.shipment_recorded", second)
	}
	got, _ := e.Must(200, "GET", deliveries+"/"+id.String(), "", "")
	if n := got["shipment"].(map[string]any)["shipment_number"]; n != "SH-A" {
		t.Errorf("the delivery's shipment: got %v, want SH-A", n)
	}
}
