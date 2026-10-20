package configuration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// disclosure shows every simulation's detail as {"shown": true}, or fails.
type disclosure struct{ err error }

func (d disclosure) Disclose(_ context.Context, kind string, detail json.RawMessage) (json.RawMessage, error) {
	if d.err != nil {
		return nil, d.err
	}
	if kind != "simulation" {
		return detail, nil
	}
	return json.RawMessage(`{"shown": true}`), nil
}

// TestDisclosure: a change request's reader is shown each evidence's
// detail as the disclosures leave it, on every read that returns the
// request; a disclosure that fails fails the read rather than show the
// detail whole.
func TestDisclosure(t *testing.T) {
	e := setup(t, false)
	tax := document(t)
	tax.Companies[0].TaxCodes[0].RatePercent = "9"
	cr := e.propose(t, 1, tax)
	e.transition(t, cr, "submit")
	id := uuid.MustParse(cr["id"].(string))
	pgtest.Exec(t, `INSERT INTO config.evidence (tenant_id, change_request_id, kind, passed, summary, detail, recorded_by)
		VALUES ($1, $2, 'simulation', true, 'Simulated.', '{"values": [1, 2]}', $3)`, e.ten.ID, id, e.ten.Human.ID)
	h := endpoints()
	h.Disclosures = []configuration.Disclosure{disclosure{}}
	if err := apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		res, err := h.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
		if err != nil {
			return err
		}
		if got := string(res.(*gen.GetChangeRequestOKHeaders).Response.Evidence[0].Detail["shown"]); got != "true" {
			t.Errorf("detail shown: got %q, want the disclosure's", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h.Disclosures = []configuration.Disclosure{disclosure{err: errors.New("directory down")}}
	calls := map[string]func(ctx context.Context) error{
		"get": func(ctx context.Context) error {
			_, err := h.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListChangeRequests(ctx, gen.ListChangeRequestsParams{})
			return err
		},
		"withdraw": func(ctx context.Context) error {
			_, err := h.WithdrawChangeRequest(ctx, &gen.ReasonInput{Reason: "r"}, gen.WithdrawChangeRequestParams{ID: id, IfMatch: "*"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			pgtest.CheckErr(t, apitest.InRequest(t, e.ten, e.ten.Human, call), "directory down")
		})
	}
}
