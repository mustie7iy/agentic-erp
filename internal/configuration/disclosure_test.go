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
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// disclosure shows every simulation's detail as {"shown": true} and its
// summary as "Shown.", or fails.
type disclosure struct{ err error }

func (d disclosure) Disclose(_ context.Context, e configuration.Evidence, _ authz.Access) (configuration.Evidence, error) {
	if d.err != nil {
		return configuration.Evidence{}, d.err
	}
	if e.Kind == "simulation" {
		e.Summary, e.Detail = "Shown.", json.RawMessage(`{"shown": true}`)
	}
	return e, nil
}

// TestDisclosure: a change request's reader is shown each evidence's
// summary and detail as the disclosures leave them, on every read that returns the
// request; a disclosure that fails, company access that cannot be read or
// regression evidence whose cases do not read fails the read rather than
// show the detail whole.
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
		shown := res.(*gen.GetChangeRequestOKHeaders).Response.Evidence[0]
		if got := string(shown.Detail["shown"]); got != "true" {
			t.Errorf("detail shown: got %q, want the disclosure's", got)
		}
		if shown.Summary != "Shown." {
			t.Errorf("summary shown: got %q, want the disclosure's", shown.Summary)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The reader's company access is read once per request; every statement
	// of a read, that one too, can fail.
	for name, call := range map[string]func(ctx context.Context) error{
		"get": func(ctx context.Context) error {
			_, err := h.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListChangeRequests(ctx, gen.ListChangeRequestsParams{})
			return err
		},
	} {
		t.Run("every statement of "+name+" failing", func(t *testing.T) {
			apitest.EveryFailure(t, e.ten, e.ten.Human, name, call)
		})
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
	// Regression evidence whose cases do not read fails the read.
	h.Disclosures = []configuration.Disclosure{disclosure{}}
	pgtest.Exec(t, `INSERT INTO config.evidence (tenant_id, change_request_id, kind, passed, summary, detail, recorded_by)
		VALUES ($1, $2, 'regression', true, 'Suite RS-0001 replayed under the proposal.', '{"cases_by_company": 1}', $3)`, e.ten.ID, id, e.ten.Human.ID)
	pgtest.CheckErr(t, apitest.InRequest(t, e.ten, e.ten.Human, func(ctx context.Context) error {
		_, err := h.GetChangeRequest(ctx, gen.GetChangeRequestParams{ID: id})
		return err
	}), "configuration: regression cases")
}
