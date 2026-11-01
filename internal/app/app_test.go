package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/analytics"
	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
	"github.com/mustie7iy/agentic-erp/internal/platform/authn/authntest"
	"github.com/mustie7iy/agentic-erp/internal/platform/fieldcrypt"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/ratelimit"
	"github.com/mustie7iy/agentic-erp/internal/platform/telemetry"
	"github.com/mustie7iy/agentic-erp/internal/procurement"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// TestQuotasAndDeadlines checks the hardening in the request chain: the
// rate limiter answers before the unit of work, and a request past its
// deadline is a 503 the client can retry. The chain is platform/httpapi's,
// but the check runs here because it needs a real operation behind the
// unit of work (the database answering the deadline).
func TestQuotasAndDeadlines(t *testing.T) {
	base, getPath := pipeline(t)
	get := func(h http.Handler) *httptest.ResponseRecorder { return getPath(h, "/v1/operations") }
	limited := base
	limited.Limits = &ratelimit.Limiter{Principal: ratelimit.Quota{Rate: 0.001, Burst: 1}}
	h, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	rec := get(h)
	if rec.Code != 200 {
		t.Fatalf("first status: got %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("first RateLimit-Remaining: got %q, want 0", got)
	}
	rec = get(h)
	if rec.Code != 429 {
		t.Fatalf("second status: got %d, want 429 (%s)", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("second Retry-After: got empty, want set (%v)", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "request.rate_limited") {
		t.Errorf("second body: got %s, want request.rate_limited", rec.Body)
	}
	expiring := base
	expiring.RequestTimeout = time.Nanosecond
	if h, err = New(expiring); err != nil {
		t.Fatal(err)
	}
	rec = get(h)
	if rec.Code != 503 {
		t.Fatalf("expired status: got %d, want 503 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("expired Retry-After: got %q, want 1", got)
	}
	if !strings.Contains(rec.Body.String(), "platform.unavailable") {
		t.Errorf("expired body: got %s, want platform.unavailable", rec.Body)
	}
}

// pipeline is the API's configuration over the test database and a GET of
// a path through a handler, as a principal allowed to read operations.
func pipeline(t *testing.T) (Config, func(h http.Handler, path string) *httptest.ResponseRecorder) {
	t.Helper()
	is := authntest.New(t)
	v, err := authn.New(context.Background(), authn.Development, []authn.Issuer{is.Config()})
	if err != nil {
		t.Fatal(err)
	}
	ten := pgtest.NewTenant(t, "platform.operation.read")
	pages, _ := pagination.NewCodec([]byte(strings.Repeat("p", 32)))
	legacy, _ := fieldcrypt.New([]byte(strings.Repeat("f", 32)))
	keys := &fieldcrypt.Keyring{Legacy: legacy}
	base := Config{Pool: pgtest.Pool(t), AppRole: "erp_app", Verifier: v, Pages: pages, FieldKeys: keys, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: telemetry.NewMetrics(), Tracer: tracenoop.NewTracerProvider(), Meter: noop.NewMeterProvider(), Now: time.Now}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+is.Token(t, ten.ID, ten.Human.Subject, nil))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	return base, get
}

// TestEveryResponseCarriesRateLimitHeaders holds what the contract states
// once, in its info.description, rather than on each response: every
// response to an authenticated request, a success, a problem or the 429
// itself, carries RateLimit-Limit, RateLimit-Remaining and RateLimit-Reset.
func TestEveryResponseCarriesRateLimitHeaders(t *testing.T) {
	base, get := pipeline(t)
	limited := base
	limited.Limits = &ratelimit.Limiter{Principal: ratelimit.Quota{Rate: 0.001, Burst: 3}}
	h, err := New(limited)
	if err != nil {
		t.Fatal(err)
	}
	// In order: the bucket of three is spent by the first three, so the fourth is refused.
	for _, step := range []struct {
		name, path string
		status     int
		remaining  string
	}{
		{"a success", "/v1/operations", http.StatusOK, "2"},
		{"a problem from the operation", "/v1/operations/" + uuid.NewString(), http.StatusNotFound, "1"},
		{"a problem from validation", "/v1/operations?page_size=0", http.StatusBadRequest, "0"},
		{"the refusal", "/v1/operations", http.StatusTooManyRequests, "0"},
	} {
		rec := get(h, step.path)
		if rec.Code != step.status {
			t.Fatalf("%s: status got %d, want %d (%s)", step.name, rec.Code, step.status, rec.Body)
		}
		for name, want := range map[string]string{"RateLimit-Limit": "3", "RateLimit-Remaining": step.remaining} {
			if got := rec.Header().Get(name); got != want {
				t.Errorf("%s: %s got %q, want %q", step.name, name, got, want)
			}
		}
		if got := rec.Header().Get("RateLimit-Reset"); got == "" {
			t.Errorf("%s: RateLimit-Reset got none, want the seconds until the window resets", step.name)
		}
	}
}

// TestNewWithoutModel: an unreadable governed model stops the API's
// construction rather than failing its first analytics query.
func TestNewWithoutModel(t *testing.T) {
	governedModel = func() (*analytics.Model, error) { return nil, errors.New("model unreadable") }
	t.Cleanup(func() { governedModel = analytics.DefaultModel })
	if _, err := New(Config{Log: slog.New(slog.DiscardHandler)}); err == nil || err.Error() != "model unreadable" {
		t.Errorf("got %v, want the model's error", err)
	}
}

// The server does not start on country packs that are not as released.
func TestCountryPacksRefused(t *testing.T) {
	_, err := New(Config{Packs: fstest.MapFS{}, Log: slog.New(slog.DiscardHandler)}) // no hash list
	if err == nil || !strings.Contains(err.Error(), "app: country packs") {
		t.Errorf("New: got %v, want the country packs refused", err)
	}
}

// The server does not start on unreleased packs it cannot load.
func TestUnreleasedPacksRefused(t *testing.T) {
	_, err := New(Config{UnreleasedPacks: filepath.Join(t.TempDir(), "none"), Log: slog.New(slog.DiscardHandler)})
	if err == nil || !strings.Contains(err.Error(), "app: country packs") {
		t.Errorf("New: got %v, want the country packs refused", err)
	}
}

// TestNewWithoutLogger: the API is not built without a logger, which the
// country packs' findings at start must reach.
func TestNewWithoutLogger(t *testing.T) {
	if _, err := New(Config{}); err == nil || !strings.Contains(err.Error(), "a logger is required") {
		t.Errorf("New: got %v, want a logger required", err)
	}
}

// TestApprovalObjectsLocated: every approval object type has the context
// that owns it, which says where the API reads its objects, so a request
// leads to its object (review:T72e). Objects read by their own id are
// placed here; an order change and a bank clearing are read through their
// order and statement, which their contexts' tests place.
func TestApprovalObjectsLocated(t *testing.T) {
	handlers := approvalHandlers(Config{})
	id := uuid.New()
	direct := map[string]string{
		approvals.Requisition: "/v1/requisitions/", approvals.PartnerChange: "/v1/partner-changes/", approvals.ConfigChange: "/v1/change-requests/",
		approvals.InvoiceException: "/v1/supplier-invoices/", approvals.PeriodUnlock: "/v1/fiscal-periods/",
		approvals.ManualEntry: "/v1/parked-journal-entries/", approvals.PurchaseOrder: "/v1/purchase-orders/", approvals.CenterChange: "/v1/center-changes/",
		approvals.PaymentRunRelease: "/v1/payment-runs/", approvals.GRIRClearing: "/v1/gr-ir-clearings/",
		approvals.TaxReturn: "/v1/tax-returns/",
	}
	for kind := range approvals.ObjectPermissions {
		t.Run(kind, func(t *testing.T) {
			l, ok := handlers[kind].(approvals.Locator)
			if !ok {
				t.Fatalf("handler of %s: got %T, want an approvals.Locator", kind, handlers[kind])
			}
			prefix, isDirect := direct[kind]
			if !isDirect {
				if _, err := l.Resources(context.Background(), []uuid.UUID{id}); err == nil {
					t.Error("outside a unit of work: got no error, want one")
				}
				return
			}
			paths, err := l.Resources(context.Background(), []uuid.UUID{id})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := paths[id], prefix+id.String(); got != want {
				t.Errorf("path: got %q, want %q", got, want)
			}
		})
	}
	if got, want := len(handlers), len(approvals.ObjectPermissions); got != want {
		t.Errorf("handlers: got %d, want one per object type (%d)", got, want)
	}
}

// TestNamingPortsNeedARequest: the ports that name what responses refer to
// read in the request's transaction, and refuse to run outside one.
func TestNamingPortsNeedARequest(t *testing.T) {
	for name, call := range map[string]func() error{
		"supplier names": func() error {
			_, err := partner.Payees{}.Names(context.Background(), []uuid.UUID{uuid.New()})
			return err
		},
		"bank accounts": func() error {
			_, err := partner.Payees{}.BankAccounts(context.Background(), []uuid.UUID{uuid.New()})
			return err
		},
		"order numbers": func() error {
			_, err := procurement.Orders{}.Numbers(context.Background(), []uuid.UUID{uuid.New()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Error("outside a unit of work: got no error, want one")
			}
		})
	}
}

// TestLedgerHoldsTaxDates: the ledger the API and the recurring entries'
// worker share (Ledger, ADR-0056) holds chosen tax dates to open returns
// through taxfiling (ADR-0049 §11, T61o). A ledger wired without the port
// fails every held posting, so losing it in a merge must fail here.
func TestLedgerHoldsTaxDates(t *testing.T) {
	if got := Ledger(Config{}).TaxReturns; got == nil {
		t.Errorf("Ledger(cfg).TaxReturns = nil, want taxfiling.Holds")
	}
}
