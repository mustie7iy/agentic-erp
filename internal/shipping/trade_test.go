package shipping_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
)

// screenedAtP1 connects a trade compliance service for plant P1 of
// company 2000: an active gts connector, as integration's API would write
// it (S2s).
func (e env) screenedAtP1(t *testing.T) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, status, created_by, updated_by)
		VALUES ($1, gen_random_uuid(), 'gts', 'Trade compliance', '{"adapter": "standin", "base_url": "https://gts.example", "plants": {"2000": ["P1"]}}',
		'active', $2, $2)`, e.Tenant.ID, e.Tenant.Human.ID)
}

// screened records the service's answer on the delivery, committed, as
// the exchange with trade compliance services does.
func (e env) screened(t *testing.T, h *shipping.Endpoints, id, result, reason string) error {
	t.Helper()
	return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
		_, err := h.RecordScreening(ctx, uuid.MustParse(id), shipping.ScreeningInput{Result: result, ReasonCode: reason, ScreenedAt: time.Now()})
		return err
	})
}

// trade is the delivery's trade compliance as the API gives it.
func (e env) trade(t *testing.T, id string) (map[string]any, string) {
	t.Helper()
	d, tag := e.Must(200, "GET", deliveries+"/"+id, "", "")
	tc, _ := d["trade_compliance"].(map[string]any)
	return tc, tag
}

// TestTradeComplianceBlocksTheGoodsIssue (S2s): a delivery created at a
// plant a trade compliance service screens is pending, and its goods
// issue is refused until the service answers; a block refuses it too,
// naming the service's reason; the service's later release lets it post.
// A delivery at a plant no service screens is not screened and posts as
// before. Each answer moves the version, is audited and published.
func TestTradeComplianceBlocksTheGoodsIssue(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	plain, plainTag := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	if plain["trade_compliance"] != nil {
		t.Errorf("a delivery before the service was connected: got %v, want not screened", plain["trade_compliance"])
	}
	e.screenedAtP1(t)
	d, tag := e.Must(201, "POST", deliveries, e.create(order, "1 2 BOX"), "")
	id := d["id"].(string)
	if tc, _ := d["trade_compliance"].(map[string]any); tc["status"] != "pending" || tc["screened_at"] != nil {
		t.Errorf("a delivery at a screened plant: got %v, want pending, not yet answered", tc)
	}
	if ev := e.events(t, id); ev[0]["trade_compliance"].(map[string]any)["status"] != "pending" {
		t.Errorf("its creation's event: got %v, want pending", ev[0]["trade_compliance"])
	}
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":pick", pick("1 2 BOX"), tag)
	e.Problem(409, "shipping.trade_compliance_block", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
	if err := e.screened(t, h, id, "blocked", "SPL-HIT"); err != nil {
		t.Fatalf("the block: got %v, want it recorded", err)
	}
	tc, tag := e.trade(t, id)
	if tc["status"] != "blocked" || tc["reason_code"] != "SPL-HIT" || tc["screened_at"] == nil {
		t.Errorf("the blocked delivery: got %v, want blocked by SPL-HIT", tc)
	}
	p := e.Problem(409, "shipping.trade_compliance_block", "POST", deliveries+"/"+id+":postGoodsIssue", "", tag)
	if detail, _ := p["detail"].(string); !strings.Contains(detail, "blocked") || !strings.Contains(detail, "SPL-HIT") {
		t.Errorf("the refusal's detail: got %q, want it to name the block and its reason", detail)
	}
	if err := e.screened(t, h, id, "released", ""); err != nil {
		t.Fatalf("the release: got %v, want it recorded", err)
	}
	tc, tag = e.trade(t, id)
	if tc["status"] != "released" || tc["reason_code"] != nil {
		t.Errorf("the released delivery: got %v, want released, its reason cleared", tc)
	}
	if got, _ := e.issued(t, id, tag); got["status"] != "goods_issued" {
		t.Errorf("the released delivery's goods issue: got %v, want goods_issued", got["status"])
	}
	if a := e.audits(t, id, "shipping.outbound_delivery.record_screening"); len(a) != 2 || a[0]["trade_compliance_before"] != "pending" ||
		a[1]["trade_compliance_before"] != "blocked" {
		t.Errorf("the screenings' audit records: got %v, want two, from pending then blocked", a)
	}
	events := e.events(t, id)
	if last := events[len(events)-1]["trade_compliance"].(map[string]any); last["status"] != "released" {
		t.Errorf("the last event's trade compliance: got %v, want released", last)
	}
	_, plainTag = e.Must(200, "POST", deliveries+"/"+plain["id"].(string)+":pick", pick("1 1 BOX"), plainTag)
	if got, _ := e.issued(t, plain["id"].(string), plainTag); got["status"] != "goods_issued" {
		t.Errorf("the delivery not screened: got %v, want goods_issued", got["status"])
	}
}

// TestTradeComplianceReleasedByHand (S2s): a trade compliance officer
// releases a blocked delivery at the version read, once, named with the
// time and the service's reason kept; the goods issue then posts. A
// delivery that is not screened, or released already, is refused, as is
// one whose goods issue is posted; a later block of the service replaces
// a person's release. The list finds the deliveries by their screening.
func TestTradeComplianceReleasedByHand(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	plain, plainTag := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	e.screenedAtP1(t)
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := d["id"].(string)
	if err := e.screened(t, h, id, "blocked", "EMBARGO"); err != nil {
		t.Fatal(err)
	}
	page, _ := e.Must(200, "GET", deliveries+"?trade_compliance_status=blocked&sales_order_id="+order, "", "")
	if items := rows(t, page, "items"); len(items) != 1 || items[0]["id"] != id {
		t.Errorf("the blocked deliveries: got %v, want %s alone", items, id)
	}
	release := deliveries + "/" + id + ":releaseTradeComplianceBlock"
	e.Problem(412, "request.version_mismatch", "POST", release, "", `"1"`)
	_, tag := e.trade(t, id)
	got, tag := e.Must(200, "POST", release, "", tag)
	tc, _ := got["trade_compliance"].(map[string]any)
	if tc["status"] != "released" || tc["released_by"] != e.Tenant.Human.ID.String() || tc["released_by_name"] == nil || tc["released_at"] == nil ||
		tc["reason_code"] != "EMBARGO" {
		t.Errorf("the released delivery: got %v, want released by the caller, EMBARGO kept", tc)
	}
	e.Problem(409, "shipping.not_trade_compliance_blocked", "POST", release, "", tag)
	e.Problem(409, "shipping.not_trade_compliance_blocked", "POST", deliveries+"/"+plain["id"].(string)+":releaseTradeComplianceBlock", "", plainTag)
	if a := e.audits(t, id, "shipping.outbound_delivery.release_trade_compliance_block"); len(a) != 1 || a[0]["trade_compliance_before"] != "blocked" {
		t.Errorf("the release's audit records: got %v, want one, from blocked", a)
	}
	if err := e.screened(t, h, id, "blocked", ""); err != nil {
		t.Fatal(err)
	}
	if tc, _ := e.trade(t, id); tc["status"] != "blocked" || tc["released_by"] != nil || tc["reason_code"] != nil {
		t.Errorf("a block after the release: got %v, want blocked, the release gone", tc)
	}
	_, tag = e.trade(t, id)
	_, tag = e.Must(200, "POST", release, "", tag)
	_, tag = e.Must(200, "POST", deliveries+"/"+id+":pick", pick("1 1 BOX"), tag)
	_, tag = e.issued(t, id, tag)
	e.Problem(409, "shipping.goods_issued", "POST", release, "", tag)
	err := e.screened(t, h, id, "blocked", "")
	if p := problem.From(err); p.Code != "shipping.goods_issued" {
		t.Errorf("an answer after the goods issue: got %v, want shipping.goods_issued", err)
	}
}

// TestRecordScreeningRefusals (S2s): an answer neither released nor
// blocked, and a reason code above 40 characters, are refused naming the
// field; a delivery that is not there is not found; outside a unit of
// work and on every database failure, nothing is recorded.
func TestRecordScreeningRefusals(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	e.screenedAtP1(t)
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := d["id"].(string)
	for name, c := range map[string]struct {
		id         string
		in         shipping.ScreeningInput
		code, path string
	}{
		"another result":       {id, shipping.ScreeningInput{Result: "review"}, "shipping.invalid_screening", "result"},
		"a reason code of 41":  {id, shipping.ScreeningInput{Result: "blocked", ReasonCode: strings.Repeat("R", 41)}, "shipping.invalid_screening", "reason_code"},
		"no delivery":          {uuid.NewString(), shipping.ScreeningInput{Result: "blocked"}, "", ""},
		"a reason code of 40":  {id, shipping.ScreeningInput{Result: "blocked", ReasonCode: strings.Repeat("R", 40)}, "ok", ""},
		"released with reason": {id, shipping.ScreeningInput{Result: "released", ReasonCode: "OK-AFTER-REVIEW"}, "ok", ""},
		"no screened_at":       {id, shipping.ScreeningInput{Result: "blocked"}, "shipping.invalid_screening", "screened_at"},
	} {
		t.Run(name, func(t *testing.T) {
			if c.path != "screened_at" {
				c.in.ScreenedAt = time.Now() // each case later than the one before it
			}
			err := e.inRequest(t, func(ctx context.Context) error {
				_, err := h.RecordScreening(ctx, uuid.MustParse(c.id), c.in)
				return err
			})
			p := problem.From(err)
			switch c.code {
			case "ok":
				if err != nil {
					t.Errorf("got %v, want it recorded", err)
				}
			case "":
				if p.Status != 404 {
					t.Errorf("got %v, want 404", err)
				}
			default:
				if p.Code != c.code || len(p.Errors) != 1 || p.Errors[0].Field != c.path {
					t.Errorf("got %v naming %v, want %s naming %s", err, p.Errors, c.code, c.path)
				}
			}
		})
	}
	t.Run("outside a unit of work", func(t *testing.T) {
		if _, err := h.RecordScreening(context.Background(), uuid.MustParse(id), shipping.ScreeningInput{Result: "blocked", ScreenedAt: time.Now()}); err == nil {
			t.Error("got nil error, want an error")
		}
		if _, err := h.ReleaseTradeComplianceBlock(context.Background(), uuid.MustParse(id), func(int32) error { return nil }); err == nil {
			t.Error("the release: got nil error, want an error")
		}
	})
	t.Run("database failures", func(t *testing.T) {
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "record a screening", func(ctx context.Context) error {
			_, err := h.RecordScreening(ctx, uuid.MustParse(id), shipping.ScreeningInput{Result: "blocked", ReasonCode: "SPL", ScreenedAt: time.Now()})
			return err
		})
		apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "release a block", func(ctx context.Context) error {
			_, err := h.ReleaseTradeComplianceBlock(ctx, uuid.MustParse(id), func(int32) error { return nil })
			return err
		})
	})
}

// failingScreening is a Screening whose read fails.
type failingScreening struct{}

func (failingScreening) Screens(context.Context, string, string) (bool, error) {
	return false, errors.New("the connectors cannot be read")
}

// TestCreateScreeningFails (S2s): a delivery whose plant's screening
// cannot be read is not created.
func TestCreateScreeningFails(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	h.Screening = failingScreening{}
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	err := e.inRequest(t, func(ctx context.Context) error {
		_, err := h.Create(ctx, shipping.Input{OrderID: uuid.MustParse(order), PlantID: e.p1, StorageLocationID: e.s1})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "trade compliance of plant P1") {
		t.Errorf("got %v, want the screening's failure", err)
	}
}

// TestScreeningAndPickInEitherOrder (S2s; ADR-0008): a screening recorded
// and a person's pick both take the delivery's row lock alone, so either
// waits for the other and both succeed, whichever comes first; no
// deadlock.
func TestScreeningAndPickInEitherOrder(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	e.screenedAtP1(t)
	for name, screeningFirst := range map[string]bool{"screening first": true, "pick first": false} {
		t.Run(name, func(t *testing.T) {
			raceScreeningAndPick(t, e, h, order, screeningFirst)
		})
	}
}

// raceScreeningAndPick races a screening and a pick of a new delivery of
// the order, the screening first or second.
func raceScreeningAndPick(t *testing.T, e env, h *shipping.Endpoints, order string, screeningFirst bool) {
	t.Helper()
	anyVersion := func(int32) error { return nil }
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := uuid.MustParse(d["id"].(string))
	screen := func(ctx context.Context) error {
		_, err := h.RecordScreening(ctx, id, shipping.ScreeningInput{Result: "released", ScreenedAt: time.Now()})
		return err
	}
	pickIt := func(ctx context.Context) error {
		_, err := h.Pick(ctx, id, anyVersion, []shipping.PickInput{{ItemNo: 1, Quantity: "1", Unit: "BOX"}})
		return err
	}
	first, second := pickIt, screen
	if screeningFirst {
		first, second = screen, pickIt
	}
	if a, b := apitest.Race(t, e.Tenant, e.Tenant.Human, e.Tenant.Human, first, second); a != nil || b != nil {
		t.Errorf("screening first %v: got %v and %v, want both done", screeningFirst, a, b)
	}
	if tc, _ := e.trade(t, id.String()); tc["status"] != "released" {
		t.Errorf("screening first %v: got %v, want released", screeningFirst, tc)
	}
}

// TestScreeningAnswersInTheServicesOrder (S2s; ADR-0008): the service's
// answers count in the order it screened, not the order they are read: a
// block read before an earlier release stays, the earlier answer refused
// (409 shipping.screening_superseded), as is one of the same time; a later
// release applies, its time the service's.
func TestScreeningAnswersInTheServicesOrder(t *testing.T) {
	e := setup(t)
	h := endpoints(t)
	order := e.order(t, false, e.ofMaterial(e.bracket, "5 BOX"))
	e.screenedAtP1(t)
	d, _ := e.Must(201, "POST", deliveries, e.create(order, "1 1 BOX"), "")
	id := uuid.MustParse(d["id"].(string))
	at := time.Date(2026, 11, 23, 10, 0, 0, 123456789, time.UTC)
	record := func(result string, when time.Time) error {
		return apitest.Committed(t, e.Tenant, e.Tenant.Human, func(ctx context.Context) error {
			_, err := h.RecordScreening(ctx, id, shipping.ScreeningInput{Result: result, ScreenedAt: when})
			return err
		})
	}
	if err := record("blocked", at); err != nil {
		t.Fatalf("the block: got %v, want it recorded", err)
	}
	for name, when := range map[string]time.Time{"an earlier release": at.Add(-time.Hour), "a release of the same time": at} {
		t.Run(name, func(t *testing.T) {
			if p := problem.From(record("released", when)); p.Code != "shipping.screening_superseded" || p.Status != 409 {
				t.Errorf("got %v, want 409 shipping.screening_superseded", p)
			}
			if tc, _ := e.trade(t, id.String()); tc["status"] != "blocked" {
				t.Errorf("the delivery: got %v, want blocked still", tc)
			}
		})
	}
	if err := record("released", at.Add(time.Minute)); err != nil {
		t.Fatalf("a later release: got %v, want it recorded", err)
	}
	tc, _ := e.trade(t, id.String())
	if want := at.Add(time.Minute).Truncate(time.Microsecond).Format(time.RFC3339Nano); tc["status"] != "released" || tc["screened_at"] != want {
		t.Errorf("the delivery: got %v, want released, screened at %s", tc, want)
	}
}
