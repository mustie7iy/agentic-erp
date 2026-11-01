package app_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// serviceEntries is newManualEntries with the service posting limit of
// legal entity 2000 (EUR) set by limit (ADR-0049 §7, T61n), and a service
// principal, an integration, that may post in every company.
type serviceEntries struct {
	manualEntries
	service apitest.Client
}

func newServiceEntries(t *testing.T, limit func(*config.Company)) serviceEntries {
	t.Helper()
	f := fixture.NewWith(t, nil, func(c *config.Config) { limit(c.Company("2000")) }, "ledger.entry.read", "ledger.entry.post", "ledger.entry.park",
		"ledger.period.read", "ledger.period.manage", "ledger.rate.read", "ledger.rate.maintain")
	f.Tenant.AllCompanies(t, f.Tenant.Service)
	m := manualEntries{Env: f, bank: f.Account(t, "2000", "100000").String(), revenue: f.Account(t, "2000", "400000").String()}
	return serviceEntries{manualEntries: m, service: f.As(f.Tenant.Service)}
}

// limitOf sets the company's service posting limit.
func limitOf(amount string) func(*config.Company) {
	return func(co *config.Company) { co.ServicePostingLimit = amount }
}

// journal counts the tenant's journal entries.
func (s serviceEntries) journal(t *testing.T) int {
	t.Helper()
	page, _ := s.Must(200, "GET", "/v1/journal-entries", "", "")
	return len(page["items"].([]any))
}

// TestServiceEntryWithinTheLimit: a service principal's entry at its
// company's service posting limit posts at once, as a person's does.
func TestServiceEntryWithinTheLimit(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	je, _ := s.service.Must(201, "POST", "/v1/journal-entries", s.direct("EUR", "1000.00"), "")
	expect(t, "service's entry at the limit", je, map[string]any{"posted_by": s.Tenant.Service.ID.String(), "source_context": "ledger"})
	if page, _ := s.Must(200, "GET", "/v1/parked-journal-entries", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("parked entries: got %v, want none", page["items"])
	}
}

// TestServiceEntryAboveTheLimit: above the limit the entry is parked for a
// person instead of posted, saying why with each figure's currency; a
// person posts it through :post, taking it on.
func TestServiceEntryAboveTheLimit(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	r := s.API.Do(apitest.Req{Method: "POST", Path: "/v1/journal-entries", Body: s.direct("EUR", "1000.01"), Tenant: s.Tenant.ID,
		As: s.Tenant.Service, Key: apitest.NewKey()})
	if r.Code != http.StatusOK {
		t.Fatalf("service's entry above the limit: got %d %s, want 200", r.Code, r.Body)
	}
	pe := apitest.JSON(t, r)
	path := "/v1/parked-journal-entries/" + pe["id"].(string)
	if got := r.Header().Get("Location"); got != path {
		t.Errorf("Location: got %q, want %q", got, path)
	}
	expect(t, "parked entry", pe, map[string]any{"status": "parked", "created_by_kind": "service", "owner_id": nil, "journal_entry_id": nil,
		"created_by": s.Tenant.Service.ID.String()})
	limit := pe["limit_exceeded"].(map[string]any)
	expect(t, "limit exceeded", limit, map[string]any{"kind": "service_posting_limit"})
	expect(t, "its debits", limit["debits"].(map[string]any), map[string]any{"amount": "1000.01", "currency": "EUR"})
	expect(t, "the limit", limit["limit"].(map[string]any), map[string]any{"amount": "1000.00", "currency": "EUR"})
	if d := limit["detail"].(string); !strings.Contains(d, "1000.01 EUR") || !strings.Contains(d, "1000.00 EUR") || strings.Contains(d, "converted") {
		t.Errorf("detail: got %q, want the debits and the limit with their currency, nothing converted", d)
	}
	if n := s.journal(t); n != 0 {
		t.Errorf("journal entries of the parked entry: got %d, want 0", n)
	}
	var detail string
	pgtest.Scan(t, `SELECT detail::text FROM audit.audit_event WHERE tenant_id = $1 AND action = 'ledger.parked_entry.park' AND object_id = $2::uuid`,
		[]any{s.Tenant.ID, pe["id"]}, &detail)
	if !strings.Contains(detail, `"limit_exceeded": "service_posting_limit"`) {
		t.Errorf("park audit detail: got %s, want the limit exceeded", detail)
	}
	changed := apitest.Published(t, s.Tenant, outbox.Event{Context: "ledger", Name: "parked_journal_entry_changed"})
	if len(changed) != 1 {
		t.Fatalf("parked events: got %d, want 1", len(changed))
	}
	expect(t, "park event", changed[0], map[string]any{"status": "parked", "created_by_kind": "service", "previous_status": nil})

	posted := s.post(t, path)
	expect(t, "entry a person posted", posted, map[string]any{"status": "posted", "owner_id": s.Tenant.Human.ID.String()})
	expect(t, "its reason, kept", posted["limit_exceeded"].(map[string]any), map[string]any{"kind": "service_posting_limit"})
	je, _ := s.Must(200, "GET", "/v1/journal-entries/"+posted["journal_entry_id"].(string), "", "")
	expect(t, "journal entry", je, map[string]any{"posted_by": s.Tenant.Human.ID.String(), "source_document_id": pe["id"]})
}

// TestServiceEntryReplayed: the integration's retry with the same
// Idempotency-Key answers the parked entry again and parks nothing more.
func TestServiceEntryReplayed(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	req := apitest.Req{Method: "POST", Path: "/v1/journal-entries", Body: s.direct("EUR", "2000.00"), Tenant: s.Tenant.ID, As: s.Tenant.Service,
		Key: apitest.NewKey()}
	first, again := s.API.Do(req), s.API.Do(req)
	if first.Code != http.StatusOK || again.Code != http.StatusOK {
		t.Fatalf("parked twice: got %d and %d, want 200 both", first.Code, again.Code)
	}
	if got := again.Header().Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("Idempotent-Replayed: got %q, want true", got)
	}
	if a, b := apitest.JSON(t, first)["id"], apitest.JSON(t, again)["id"]; a != b {
		t.Errorf("replayed entry: got %v, want %v", b, a)
	}
	if page, _ := s.Must(200, "GET", "/v1/parked-journal-entries", "", ""); len(page["items"].([]any)) != 1 {
		t.Errorf("parked entries: got %d, want 1", len(page["items"].([]any)))
	}
}

// TestServiceEntryInAnotherCurrency: an entry in a foreign currency is
// measured in its company's currency, exactly, at the average rate of its
// posting date; its reason says so.
func TestServiceEntryInAnotherCurrency(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	s.Must(201, "POST", "/v1/exchange-rates", `{"rate_type": "average", "from_currency": "USD", "to_currency": "EUR", "valid_from": "2026-01-01",
		"rate": "1.25", "source": "ECB"}`, "")
	s.service.Must(201, "POST", "/v1/journal-entries", s.direct("USD", "800.00"), "")          // 1000.00 EUR
	pe, _ := s.service.Must(200, "POST", "/v1/journal-entries", s.direct("USD", "800.01"), "") // 1000.0125 EUR
	limit := pe["limit_exceeded"].(map[string]any)
	expect(t, "its debits", limit["debits"].(map[string]any), map[string]any{"amount": "800.01", "currency": "USD"})
	expect(t, "the limit", limit["limit"].(map[string]any), map[string]any{"amount": "1000.00", "currency": "EUR"})
	if d := limit["detail"].(string); !strings.Contains(d, "800.01 USD, converted into EUR at the average rate of its posting date") {
		t.Errorf("detail: got %q, want the conversion named", d)
	}
}

// TestServiceLimitDefault: a company without a service posting limit
// holds services to its manual entry approval limit, as before T61n, but
// parks above it rather than refusing. (A company the configuration lacks
// has a limit of 0: internal/ledger, TestServiceLimitOfACompanyNotConfigured.)
func TestServiceLimitDefault(t *testing.T) {
	s := newServiceEntries(t, limitOf(""))
	s.service.Must(201, "POST", "/v1/journal-entries", s.direct("EUR", "10000.00"), "")
	pe, _ := s.service.Must(200, "POST", "/v1/journal-entries", s.direct("EUR", "10000.01"), "")
	expect(t, "the limit", pe["limit_exceeded"].(map[string]any)["limit"].(map[string]any), map[string]any{"amount": "10000.00", "currency": "EUR"})
}

// TestServiceLimitLeavesPeople: people are held to the manual entry
// approval limit alone, whatever the service posting limit.
func TestServiceLimitLeavesPeople(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	s.Must(201, "POST", "/v1/journal-entries", s.direct("EUR", "5000.00"), "")
	s.Problem(409, "ledger.approval_required", "POST", "/v1/journal-entries", s.direct("EUR", "10000.01"), "")
	if page, _ := s.Must(200, "GET", "/v1/parked-journal-entries", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("parked entries of people's postings: got %v, want none", page["items"])
	}
}

// TestServiceEntryInAClosedPeriod: a closed period refuses a service's
// entry before it is measured, so nothing is parked that a person could
// not post.
func TestServiceEntryInAClosedPeriod(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	page, _ := s.Must(200, "GET", fmt.Sprintf("/v1/fiscal-periods?legal_entity_id=%s&fiscal_year=2026&order_by=period&page_size=3", s.DE), "", "")
	period := page["items"].([]any)[2].(map[string]any)
	s.Must(200, "POST", "/v1/fiscal-periods/"+period["id"].(string)+":close", "", fmt.Sprintf(`"%v"`, period["version"]))
	s.service.Problem(409, "ledger.period_not_open", "POST", "/v1/journal-entries", s.direct("EUR", "2000.00"), "")
	if page, _ := s.Must(200, "GET", "/v1/parked-journal-entries", "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("parked entries: got %v, want none", page["items"])
	}
}

// TestServiceNeverPostsWhatItParked: the entry waits for a person; the
// service principal cannot post it itself (:post is people's).
func TestServiceNeverPostsWhatItParked(t *testing.T) {
	s := newServiceEntries(t, limitOf("1000.00"))
	pe, _ := s.service.Must(200, "POST", "/v1/journal-entries", s.direct("EUR", "2000.00"), "")
	path := "/v1/parked-journal-entries/" + pe["id"].(string)
	_, tag := s.service.Must(200, "GET", path, "", "")
	s.service.Problem(403, "identity.agent_not_allowed", "POST", path+":post", "", tag)
}
