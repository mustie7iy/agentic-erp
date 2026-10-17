package configuration_test

import (
	"bytes"
	"context"
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
	t.Cleanup(func() { slog.SetDefault(previous) })
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
	t.Cleanup(func() {
		pgtest.Exec(t, `UPDATE config.change_request SET proposed = proposed #- '{purchasing,future_key}', version = version + 1 WHERE id = $1`, uuid.MustParse(cr))
	})
}

// TestNewerProposalRefused: a proposal with a key this binary does not
// know is not activated here, which would drop the key; nor simulated:
// it stays queued for a replica that knows it.
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
	s.Problem(422, "config.validation_failed", "POST", "/v1/change-requests/"+id+":activate", "", "*")

	s.suite(t)
	tier3, op := s.tier3(t, "11")
	newer(t, apitest.String(t, tier3, "id"))
	w := worker(t, &fakeSimulator{sim: configuration.Simulation{Passed: true, Summary: "fine", Detail: map[string]any{}}})
	if _, err := w.RunOnce(context.Background()); err == nil {
		t.Error("simulation of a newer proposal: got nil error, want it refused")
	}
	if queued := s.status(t, "/v1/operations/"+apitest.String(t, op, "id")); queued["status"] != "queued" {
		t.Errorf("simulation of a newer proposal: got %v, want it queued for a newer replica", queued["status"])
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
