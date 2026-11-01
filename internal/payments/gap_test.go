package payments_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/payments"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// gapScenario is the matching scenario's statement in gap: the bank's
// statement before it closes at 900.00, and it opens at 1000.00. The rules
// matched its lines on import, and none posted.
func gapScenario(t *testing.T) matchScenario {
	t.Helper()
	return gapScenarioWith(t, "payments.statement.post", "payments.statement.gap.accept")
}

// gapScenarioWith is gapScenario with these permissions besides matching.
func gapScenarioWith(t *testing.T, perms ...string) matchScenario {
	t.Helper()
	e := matchSetupWith(t, perms...)
	e.statementRules(t, postRules)
	e.bankPosts(t)
	e.imported(t, "mt940", mtStatement("M0", "0", "900,00", "260415", "900,00", "260415"))
	sc := matchStatementOf(t, e)
	if s, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", ""); s["status"] != "gap" {
		t.Fatalf("statement after one closing at 900.00: got %v, want gap", s["status"])
	}
	return sc
}

// TestAcceptStatementGap: a statement in gap stops its bank's postings; a
// person accepts the gap with a reason, which is kept, audited and
// published, and the bank's lines post again on the next run of the rules
// (ADR-0047 Continuity).
func TestAcceptStatementGap(t *testing.T) {
	sc := gapScenario(t)
	e := sc.e
	path := "/v1/bank-statements/" + sc.statement + ":acceptGap"
	if got := strings.Join(e.lineStatuses(t, sc.statement)[:3], ","); got != "matched,matched,matched" {
		t.Errorf("lines 1-3 of a statement in gap: got %s, want matched, not posted", got)
	}
	e.Problem(409, "payments.statement_gap", "POST", sc.line(3, "post"), "", sc.tag(t))
	tag := sc.tag(t)
	e.Problem(400, "request.invalid_body", "POST", path, `{}`, tag)
	e.Problem(400, "request.invalid_body", "POST", path, `{"reason": ""}`, tag)
	e.Tenant.AllCompanies(t, e.Tenant.Agent)
	e.agent().Problem(403, "identity.agent_not_allowed", "POST", path, `{"reason": "x"}`, tag)
	e.Problem(412, "request.version_mismatch", "POST", path, `{"reason": "x"}`, `"999"`)

	key := apitest.NewKey()
	accept := func() (int, map[string]any, string) {
		r := e.API.Do(apitest.Req{Method: "POST", Path: path, Tenant: e.Tenant.ID, As: e.Client.As, Key: key, IfMatch: tag,
			Body: `{"reason": "The bank cannot deliver the statement of 15 April again."}`})
		return r.Code, apitest.JSON(t, r), r.Header().Get("Idempotent-Replayed")
	}
	code, s, _ := accept()
	if code != 200 {
		t.Fatalf("accept: got %d %v, want 200", code, s)
	}
	for name, want := range map[string]any{"status": "imported", "gap_accepted_by": e.Client.As.ID.String(),
		"gap_reason": "The bank cannot deliver the statement of 15 April again."} {
		if s[name] != want {
			t.Errorf("accepted statement's %s: got %v, want %v", name, s[name], want)
		}
	}
	if s["gap_accepted_at"] == nil || s["lines"] == nil {
		t.Errorf("accepted statement: got %v, want when it was accepted, and its lines", s)
	}
	if code, again, replayed := accept(); code != 200 || replayed != "true" || again["version"] != s["version"] {
		t.Errorf("the same request again: got %d %v replayed %q, want the first answer replayed", code, again, replayed)
	}
	e.Problem(409, "payments.statement_not_in_gap", "POST", path, `{"reason": "again"}`, sc.tag(t))
	evs := events(t, e.Tenant, "bank_statement_gap_accepted")
	if len(evs) != 1 {
		t.Fatalf("gap accepted events: got %d, want 1", len(evs))
	}
	if got := field(evs[0], "previous_closing_balance", "amount"); got != "900.00" {
		t.Errorf("event's previous closing balance: got %v, want 900.00", got)
	}
	if got := field(evs[0], "opening_balance", "amount"); got != "1000.00" {
		t.Errorf("event's opening balance: got %v, want 1000.00", got)
	}
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND action = 'payments.bank_statement.accept_gap'
		AND detail->>'reason' = 'The bank cannot deliver the statement of 15 April again.'`, []any{sc.statement}, &audited)
	if audited != 1 {
		t.Errorf("audit records of the acceptance: got %d, want 1", audited)
	}

	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":match", "", sc.tag(t))
	if got := strings.Join(e.lineStatuses(t, sc.statement)[:3], ","); got != "pending_approval,pending_approval,posted" {
		t.Errorf("lines 1-3 after the gap was accepted: got %s, want posted or waiting for approval", got)
	}
}

// TestAcceptGapNeedsThePermission: a person without
// payments.statement.gap.accept leaves the gap as it is.
func TestAcceptGapNeedsThePermission(t *testing.T) {
	sc := gapScenarioWith(t, "payments.statement.post")
	sc.e.Problem(403, "identity.permission_missing", "POST", "/v1/bank-statements/"+sc.statement+":acceptGap", `{"reason": "x"}`, sc.tag(t))
}

// TestGapFoundAgainAfterAcceptance: the missing statement arriving after
// the acceptance is compared again; where it does not lead to the accepted
// statement, a new gap stands and the acceptance goes.
func TestGapFoundAgainAfterAcceptance(t *testing.T) {
	sc := gapScenario(t)
	e := sc.e
	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":acceptGap", `{"reason": "Missing for good."}`, sc.tag(t))
	missing := e.imported(t, "mt940", mtStatement("MX", "9", "900,00", "260415", "950,00", "260415", ":61:2604150415C50,00NTRFNONREF//X1\n"))[0].(map[string]any)
	s, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	if s["status"] != "gap" || s["previous_statement_id"] != missing["id"] {
		t.Errorf("accepted statement after a missing one that does not lead to it: got %v after %v, want gap after %v", s["status"], s["previous_statement_id"], missing["id"])
	}
	for _, name := range []string{"gap_accepted_at", "gap_accepted_by", "gap_reason"} {
		if s[name] != nil {
			t.Errorf("a new gap's %s: got %v, want none", name, s[name])
		}
	}
}

// TestGapClosedAfterAcceptance: the missing statement arriving after the
// acceptance and leading to the accepted statement leaves it imported,
// its acceptance kept, and publishes no gap event.
func TestGapClosedAfterAcceptance(t *testing.T) {
	sc := gapScenario(t)
	e := sc.e
	e.Must(200, "POST", "/v1/bank-statements/"+sc.statement+":acceptGap", `{"reason": "Missing for now."}`, sc.tag(t))
	missing := e.imported(t, "mt940", mtStatement("MX", "9", "900,00", "260415", "1000,00", "260415", ":61:2604150415C100,00NTRFNONREF//X1\n"))[0].(map[string]any)
	s, _ := e.Must(200, "GET", "/v1/bank-statements/"+sc.statement, "", "")
	if s["status"] != "imported" || s["previous_statement_id"] != missing["id"] {
		t.Errorf("accepted statement after the missing one: got %v after %v, want imported after %v", s["status"], s["previous_statement_id"], missing["id"])
	}
	if s["gap_reason"] != "Missing for now." || s["gap_accepted_at"] == nil || s["gap_accepted_by"] == nil {
		t.Errorf("the acceptance after the gap closed: got %v, %v, %v, want it kept", s["gap_accepted_at"], s["gap_accepted_by"], s["gap_reason"])
	}
	if n := len(events(t, e.Tenant, "bank_statement_gap_closed")) + len(events(t, e.Tenant, "bank_statement_gap_found")); n != 0 {
		t.Errorf("gap events: got %d, want none: the statement was imported already", n)
	}
}

// TestAcceptGapDatabaseFailures: every statement of an acceptance fails in
// turn, and none runs outside a unit of work.
func TestAcceptGapDatabaseFailures(t *testing.T) {
	sc := gapScenario(t)
	e := sc.e
	h := endpoints(nil)
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "accept", func(ctx context.Context) error {
		_, err := payments.AcceptGap(ctx, uuidOf(sc.statement), nil, "x")
		return err
	})
	apitest.EveryFailure(t, e.Tenant, e.Tenant.Human, "accept handler", func(ctx context.Context) error {
		_, err := h.AcceptBankStatementGap(ctx, &gen.ReasonInput{Reason: "x"}, gen.AcceptBankStatementGapParams{ID: uuidOf(sc.statement), IfMatch: "*"})
		return err
	})
	_, err := h.AcceptBankStatementGap(context.Background(), &gen.ReasonInput{Reason: "x"}, gen.AcceptBankStatementGapParams{ID: uuidOf(sc.statement), IfMatch: "*"})
	pgtest.WantErr(t, err, "")
}
