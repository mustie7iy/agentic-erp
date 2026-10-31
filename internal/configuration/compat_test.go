package configuration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Upgrade compatibility (06 §6.4): during a rolling upgrade a newer
// replica may activate a document with keys this binary does not know.
// Reading the active document tolerates them, ignored and logged once;
// submission and validation stay strict.

// activateDocument makes the tenant's active version one whose document is
// the current one changed by the jsonb expression set (over document).
func activateDocument(t *testing.T, tenant uuid.UUID, set string) {
	t.Helper()
	pgtest.Exec(t, `INSERT INTO config.config_version (tenant_id, id, number, document, hash, source, restored_from, activated_by)
		SELECT v.tenant_id, gen_random_uuid(), v.number + 1, `+set+`, v.hash, 'rollback', v.number, v.activated_by
		FROM config.config_version v JOIN config.active_version a ON a.tenant_id = v.tenant_id AND a.number = v.number WHERE v.tenant_id = $1`, tenant)
	pgtest.Exec(t, `UPDATE config.active_version SET number = number + 1, version = version + 1 WHERE tenant_id = $1`, tenant)
}

func TestActiveDocumentWithUnknownKeys(t *testing.T) {
	f := fixture.New(t, perms...)
	key := "future_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") // logged once per process: unique per run
	activateDocument(t, f.Tenant.ID, `jsonb_set(document, '{purchasing,`+key+`}', '7')`)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) }) // the process's logger, for the tests after this one
	for range 2 {
		err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			doc, _, err := configuration.Settings{}.Document(ctx)
			if err != nil {
				return err
			}
			if doc == nil {
				t.Fatal("active document with an unknown key: got nil, want the document without the key")
			}
			if len(doc.Purchasing.RequisitionApproval) == 0 {
				t.Errorf("active document with an unknown key: got no requisition tiers, want the document's")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the active document: %v", err)
		}
	}
	if n := strings.Count(logs.String(), key); n != 1 {
		t.Errorf("warnings naming %s: got %d, want 1: %s", key, n, logs.String())
	}
}

// TestUnreadableActiveDocument: a document that does not read even
// tolerantly fails the read, never a nil document.
func TestUnreadableActiveDocument(t *testing.T) {
	f := fixture.New(t, perms...)
	activateDocument(t, f.Tenant.ID, `jsonb_set(document, '{payment_terms}', '"none"')`)
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		doc, _, err := configuration.Settings{}.Document(ctx)
		if doc != nil {
			t.Errorf("unreadable document: got %v, want none", doc)
		}
		return err
	})
	pgtest.WantErr(t, err, "read the active configuration")
}

// newer marks a change request's proposal as one a newer replica
// submitted, with a key this binary does not know, until the test ends.
func newer(t *testing.T, cr string) {
	t.Helper()
	pgtest.Exec(t, `UPDATE config.change_request SET proposed = jsonb_set(proposed, '{purchasing,future_key}', '1'), version = version + 1 WHERE id = $1`,
		uuid.MustParse(cr))
	// Restored, so that the package's other tests (the simulation worker's
	// rounds take every tenant's jobs) meet only proposals they can read.
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE config.change_request SET proposed = proposed #- '{purchasing,future_key}', version = version + 1 WHERE id = $1`, uuid.MustParse(cr))
	})
}

// TestNewerProposalRefused: a proposal with a key this binary does not
// know is not activated here, which would drop the key (503
// config.unknown_keys: retry, an upgraded replica takes it); nor
// simulated: the operation fails, to be asked again on an upgraded
// replica, rather than wait behind a retry limit.
func TestNewerProposalRefused(t *testing.T) {
	s := sim{fixture.New(t, append(perms, "platform.operation.read")...)}
	doc := fixture.Document(t)
	doc.NumberRanges = append(doc.NumberRanges, config.NumberRange{Company: "1000", DocumentType: "payment", FiscalYear: 2026, Prefix: "PY-", First: 1, Last: 999})
	cr, _ := s.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Range", "rationale": "Payments", "base_version": 1, "proposed": %s}`,
		jsonOf(t, doc)), "")
	id := apitest.String(t, cr, "id")
	if got, _ := s.Must(200, "POST", "/v1/change-requests/"+id+":submit", "", `"1"`); got["status"] != "approved" {
		t.Fatalf("submitted: got status %v, want approved", got["status"])
	}
	newer(t, id)
	s.Problem(503, "config.unknown_keys", "POST", "/v1/change-requests/"+id+":activate", "", "*")

	s.suite(t)
	tier3, op := s.tier3(t, "11")
	newer(t, apitest.String(t, tier3, "id"))
	w := worker(t, &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}})
	if _, err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("simulation round: %v", err)
	}
	failed := s.status(t, "/v1/operations/"+apitest.String(t, op, "id"))
	if failed["status"] != "failed" {
		t.Fatalf("simulation of a newer proposal: got %v, want failed", failed["status"])
	}
	if got, ok := failed["error"].(map[string]any); !ok || got["code"] != "config.unknown_keys" {
		t.Errorf("simulation of a newer proposal: got error %v, want config.unknown_keys", failed["error"])
	}
}

// TestNewerBaseRefused: a change based on a version with a key this binary
// does not know is not assessed here: the change could drop the key
// without the assessment seeing it.
func TestNewerBaseRefused(t *testing.T) {
	s := sim{fixture.New(t, perms...)}
	activateDocument(t, s.Tenant.ID, `jsonb_set(document, '{purchasing,future_key}', '1')`)
	doc := fixture.Document(t)
	cr, _ := s.Must(201, "POST", "/v1/change-requests", fmt.Sprintf(`{"title": "Older", "rationale": "From an old export", "base_version": 2, "proposed": %s}`,
		jsonOf(t, doc)), "")
	s.Problem(503, "config.unknown_keys", "POST", "/v1/change-requests/"+apitest.String(t, cr, "id")+":submit", "", `"1"`)
	// The refusal's cause names the key, for the log.
	err := apitest.InRequest(t, s.Tenant, s.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.SubmitChangeRequest(ctx, fixture.Deps(), uuid.MustParse(apitest.String(t, cr, "id")), func(int32) error { return nil })
		return err
	})
	if !strings.Contains(fmt.Sprint(problem.Cause(problem.From(err))), "future_key") {
		t.Errorf("refusal cause: got %v, want it to name future_key", problem.Cause(problem.From(err)))
	}
}

// TestUnreadableVersion: a version whose document does not read fails what
// reads it.
func TestUnreadableVersion(t *testing.T) {
	f := fixture.New(t, perms...)
	activateDocument(t, f.Tenant.ID, `jsonb_set(document, '{payment_terms}', '"none"')`)
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := configuration.CaptureRegressionSuite(ctx, fixture.Deps(), configuration.NewRegressionSuite{Name: "Baseline"})
		return err
	})
	pgtest.WantErr(t, err, "read version 2")
}

// settingsVersion: an activated version reads by its number; an unknown
// one is 422, an unreadable one fails, and so does every database failure.
func settingsVersion(t *testing.T, f fixture.Env) {
	t.Helper()
	read := func(n int32) (*config.Config, error) {
		var doc *config.Config
		err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
			var err error
			doc, err = configuration.Settings{}.Version(ctx, n)
			return err
		})
		return doc, err
	}
	if doc, err := read(1); err != nil || len(doc.Companies) != 2 {
		t.Errorf("version 1: got %v, %v; want its document", doc, err)
	}
	if _, err := read(9); problem.From(err).Code != "config.unknown_version" {
		t.Errorf("version 9: got %v, want config.unknown_version", err)
	}
	if _, err := (configuration.Settings{}).Version(context.Background(), 1); err == nil {
		t.Error("outside a unit of work: got no error, want one")
	}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "version", func(ctx context.Context) error {
		_, err := configuration.Settings{}.Version(ctx, 1)
		return err
	})
	activateDocument(t, f.Tenant.ID, `jsonb_set(document, '{payment_terms}', '"none"')`)
	if _, err := read(2); err == nil || !strings.Contains(err.Error(), "read version 2") {
		t.Errorf("unreadable version: got %v, want its error", err)
	}
}

// TestSimulators: every simulation runs; the change passes when all do,
// with the summaries joined and the details merged; an error stops them.
func TestSimulators(t *testing.T) {
	a := &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "Invoices unchanged.", Detail: map[string]any{"invoices": 3}}}
	b := &fakeSimulator{sim: configuration.Simulation{Passed: false, Summary: "Metrics not computed.", Detail: map[string]any{"metrics": []string{"revenue"}}}}
	quiet := &fakeSimulator{sim: configuration.Simulation{Passed: true}}
	got, err := configuration.Simulators{a, quiet, b}.Simulate(context.Background(), nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed {
		t.Error("passed: got true, want false (one failed)")
	}
	if want := "Invoices unchanged. Metrics not computed."; got.Summary != want {
		t.Errorf("summary: got %q, want %q", got.Summary, want)
	}
	if len(got.Detail) != 2 {
		t.Errorf("detail: got %v, want both simulations'", got.Detail)
	}
	failing := &fakeSimulator{err: errors.New("down")}
	if _, err := (configuration.Simulators{failing, a}).Simulate(context.Background(), nil, nil, 0); err == nil || a.calls != 1 {
		t.Errorf("failing: got %v after %d more calls, want its error and none", err, a.calls-1)
	}
}
