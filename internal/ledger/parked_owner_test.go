package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// Changing and cancelling parked entries (T35d): only the owner does,
// while the entry is parked.

// colleague is another person of the tenant with the same roles and
// companies as its human.
func (e env) colleague(tb testing.TB) env {
	tb.Helper()
	other := pgtest.Actor{ID: uuid.Must(uuid.NewV7()), Kind: "human"}
	other.Subject = "human|" + other.ID.String()
	pgtest.Exec(tb, `INSERT INTO identity.principal (tenant_id, id, kind, external_issuer, external_subject, display_name, status, created_by, updated_by)
		VALUES ($1, $2, 'human', $3, $4, 'Colleague', 'active', $2, $2)`, e.ten.ID, other.ID, pgtest.Issuer(tb), other.Subject)
	pgtest.Exec(tb, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, created_by)
		SELECT $1, gen_random_uuid(), $2, role_id, current_date - 1, $2 FROM identity.principal_role WHERE principal_id = $3`, e.ten.ID, other.ID, e.ten.Human.ID)
	e.ten.AllCompanies(tb, other)
	e.As = other
	return e
}

// parked parks an entry of 2000 as e's principal and returns its path.
func (e env) parked(tb testing.TB) string {
	tb.Helper()
	bank, revenue := e.account(tb, "2000", "100000"), e.account(tb, "2000", "400000")
	pe, _ := e.Must(201, "POST", "/v1/parked-journal-entries", entry(e.de, "2026-03-15", "EUR", line{bank, "10.00", ""}, line{revenue, "-10.00", ""}), "")
	return "/v1/parked-journal-entries/" + pe["id"].(string)
}

func lines(a, b string, amounts ...string) string {
	out := ""
	for i, amount := range amounts {
		account := a
		if i%2 == 1 {
			account = b
		}
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf(`{"account_id": %q, "amount": {"amount": %q, "currency": "EUR"}}`, account, amount)
	}
	return `"lines": [` + out + `]`
}

func TestChangeParkedEntry(t *testing.T) {
	e := setup(t)
	bank, revenue := e.account(t, "2000", "100000"), e.account(t, "2000", "400000")
	path := e.parked(t)
	got, tag := e.Must(200, "PATCH", path, `{"header_text": "Accrual", "reference": "R-1", `+lines(bank, revenue, "5.00", "-2.00", "-3.00")+`}`, `"1"`)
	want(t, "changed entry", got, map[string]any{"header_text": "Accrual", "reference": "R-1", "version": float64(2), "status": "parked"})
	if tag != `"2"` {
		t.Errorf("changed ETag: got %s, want \"2\"", tag)
	}
	if got := amounts(t, got, "amount"); got != "5.00,-2.00,-3.00" {
		t.Errorf("changed lines: got %s, want 5.00,-2.00,-3.00", got)
	}
	// Fewer lines remove the rest; null clears; absent fields stay.
	got, _ = e.Must(200, "PATCH", path, `{"reference": null, `+lines(bank, revenue, "7.00", "-7.00")+`}`, `"2"`)
	want(t, "changed again", got, map[string]any{"header_text": "Accrual", "reference": nil, "posting_date": "2026-03-15"})
	if got := amounts(t, got, "amount"); got != "7.00,-7.00" {
		t.Errorf("shortened lines: got %s, want 7.00,-7.00", got)
	}
	got, _ = e.Must(200, "PATCH", path, `{"posting_date": "2026-04-01", "document_date": "2026-03-31", "document_type": "AB"}`, `"3"`)
	want(t, "redated", got, map[string]any{"posting_date": "2026-04-01", "document_date": "2026-03-31", "document_type": "AB"})
	if got := amounts(t, got, "amount"); got != "7.00,-7.00" {
		t.Errorf("lines kept: got %s, want 7.00,-7.00", got)
	}
	e.Problem(412, "request.version_mismatch", "PATCH", path, `{"header_text": "x"}`, `"1"`)
	e.Problem(422, "ledger.currency_mismatch", "PATCH", path, `{"currency": "USD"}`, `"4"`)
	e.Problem(422, "ledger.unbalanced", "PATCH", path, `{`+lines(bank, revenue, "7.00", "-6.00")+`}`, `"4"`)
	e.Problem(404, "request.not_found", "PATCH", "/v1/parked-journal-entries/"+uuid.NewString(), `{"header_text": "x"}`, `"1"`)
	var audited int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.parked_entry.update'`, []any{e.ten.ID}, &audited)
	if audited != 3 {
		t.Errorf("audit records of changes: got %d, want 3", audited)
	}
	// A change is not a status change: only parking published.
	if changed := events(t, e.ten, "parked_journal_entry_changed"); len(changed) != 1 {
		t.Errorf("parked events: got %d, want 1", len(changed))
	}
}

func TestCancelParkedEntry(t *testing.T) {
	e := setup(t)
	path := e.parked(t)
	got, tag := e.Must(200, "POST", path+":cancel", `{"reason": "Duplicate"}`, `"1"`)
	want(t, "cancelled entry", got, map[string]any{"status": "cancelled", "version": float64(2)})
	e.Problem(409, "ledger.invalid_parked_status", "POST", path+":cancel", `{"reason": "Again"}`, tag)
	e.Problem(409, "ledger.invalid_parked_status", "PATCH", path, `{"header_text": "x"}`, tag)
	changed := events(t, e.ten, "parked_journal_entry_changed")
	if len(changed) != 2 {
		t.Fatalf("parked events: got %d, want 2", len(changed))
	}
	want(t, "cancel event", changed[1], map[string]any{"status": "cancelled", "previous_status": "parked"})
}

// TestParkedEntryOwners: a person's entry is theirs alone; an agent's
// proposal is the agent's until a person takes it on by changing or
// cancelling it.
func TestParkedEntryOwners(t *testing.T) {
	e := setup(t)
	colleague, agent := e.colleague(t), e.asAgent(t)
	mine := e.parked(t)
	for name, c := range map[string]env{"another person": colleague, "an agent": agent} {
		t.Run(name, func(t *testing.T) {
			c := c.in(t)
			c.Problem(403, "ledger.not_entry_owner", "PATCH", mine, `{"header_text": "x"}`, `"1"`)
			c.Problem(403, "ledger.not_entry_owner", "POST", mine+":cancel", `{"reason": "r"}`, `"1"`)
		})
	}
	proposal := agent.parked(t)
	got, _ := agent.Must(200, "PATCH", proposal, `{"header_text": "Agent's own"}`, `"1"`)
	want(t, "agent's change", got, map[string]any{"owner_id": nil, "header_text": "Agent's own"})
	other := e.Client
	other.As = e.ten.Service
	e.ten.AllCompanies(t, e.ten.Service)
	other.Problem(403, "ledger.not_entry_owner", "PATCH", proposal, `{"header_text": "x"}`, `"2"`)
	got, _ = e.Must(200, "PATCH", proposal, `{"header_text": "Taken on"}`, `"2"`)
	want(t, "taken on", got, map[string]any{"owner_id": e.ten.Human.ID.String(), "created_by_kind": "agent"})
	agent.Problem(403, "ledger.not_entry_owner", "PATCH", proposal, `{"header_text": "x"}`, `"3"`)
	colleague.Problem(403, "ledger.not_entry_owner", "POST", proposal+":cancel", `{"reason": "r"}`, `"3"`)
	// A person cancelling a proposal takes it on too.
	rejected := agent.parked(t)
	got, _ = colleague.Must(200, "POST", rejected+":cancel", `{"reason": "Not needed"}`, `"1"`)
	want(t, "proposal cancelled", got, map[string]any{"status": "cancelled", "owner_id": colleague.As.ID.String()})
	// The agent cancels its own proposal no person has taken on.
	withdrawn := agent.parked(t)
	got, _ = agent.Must(200, "POST", withdrawn+":cancel", `{"reason": "Superseded"}`, `"1"`)
	want(t, "proposal withdrawn", got, map[string]any{"status": "cancelled", "owner_id": nil})
}

// TestChangeWaitsForTheEntry: a change reads the entry FOR UPDATE, so one
// that waited for another sees its version and is refused rather than
// writing over it.
func TestChangeWaitsForTheEntry(t *testing.T) {
	e := setup(t)
	path := e.parked(t)
	id := uuid.MustParse(path[len("/v1/parked-journal-entries/"):])
	admin := pgtest.Admin(t)
	tx, err := admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid() FROM ledger.parked_entry WHERE id = $1 FOR UPDATE`, id).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _, _ := e.Do("PATCH", path, `{"header_text": "Mine"}`, `"1"`)
		done <- code
	}()
	pgtest.Eventually(t, 10*time.Second, func() bool {
		var n int
		pgtest.Scan(t, `SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, []any{pid}, &n)
		return n > 0
	}, "the change to wait for the entry")
	if _, err := tx.Exec(context.Background(), `UPDATE ledger.parked_entry SET header_text = 'Theirs', version = version + 1 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 412 {
		t.Errorf("change after another: got %d, want 412", code)
	}
}
