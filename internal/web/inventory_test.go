package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/api/gen"
)

// recorded is a transport that notes the API operation of every call the
// workspace makes, by the contract's own routes (gen.Server.FindRoute).
type recorded struct {
	next   http.RoundTripper
	router *gen.Server
	mu     sync.Mutex
	ids    map[string]bool
}

func (r *recorded) RoundTrip(req *http.Request) (*http.Response, error) {
	if route, ok := r.router.FindRoute(req.Method, req.URL.Path); ok {
		r.mu.Lock()
		r.ids[route.OperationID()] = true
		r.mu.Unlock()
	}
	return r.next.RoundTrip(req)
}

// take is the operations called since the last take.
func (r *recorded) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := slices.Sorted(maps.Keys(r.ids))
	r.ids = map[string]bool{}
	return ids
}

// anyToken lets every call through the router built only to name routes.
type anyToken struct{}

func (anyToken) HandleOidc(ctx context.Context, _ gen.OperationName, _ gen.Oidc) (context.Context, error) {
	return ctx, nil
}

// recordCalls points the stack's workspace at its API through a recorder.
func recordCalls(t *testing.T, s *stack) *recorded {
	t.Helper()
	router, err := gen.NewServer(gen.UnimplementedHandler{}, anyToken{})
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorded{next: http.DefaultTransport, router: router, ids: map[string]bool{}}
	api, err := NewAPI(s.api.URL, &http.Client{Transport: rec})
	if err != nil {
		t.Fatal(err)
	}
	s.srv.API = api
	return rec
}

// inventoried checks that every operation route called is in its
// inventory (Screens), which TestScreensAgainstTheContract holds to the
// contract and to the six reads a page may make: that test reads only the
// operations listed, so a call left out of the list escapes it.
func inventoried(t *testing.T, route string, called []string) {
	t.Helper()
	if len(called) == 0 {
		t.Errorf("%s: got no API call recorded, want the page's calls", route)
	}
	listed := map[string]bool{}
	for _, op := range Screens[route] {
		listed[op.ID] = true
	}
	for _, id := range called {
		if !listed[id] {
			t.Errorf("%s calls %s: got it missing from Screens, want every call inventoried", route, id)
		}
	}
}

// TestInvoiceFormCallsItsInventory: the parked invoice's change form,
// shown and sent with a line's account and cost center changed, calls
// only operations its routes' inventory lists. Batch 38's integration
// found T73e3's form reading accounts, cost centers and profit centers
// that the inventory left out.
func TestInvoiceFormCallsItsInventory(t *testing.T) {
	s, d := invoicesStack(t)
	b := s.browser()
	b.signIn()
	rec := recordCalls(t, s)
	status(t, "the form", b.get(editPath(d.account)), http.StatusOK)
	inventoried(t, "GET /invoices/{id}/edit", rec.take())
	form := editForm(t, b, d.account, url.Values{"item_account": {"530000"}, "item_cost_center": {"CC-UNKNOWN"}})
	rec.take()
	status(t, "a code no list holds", b.do(http.MethodPost, editPath(d.account), form), http.StatusUnprocessableEntity)
	inventoried(t, "POST /invoices/{id}/edit", rec.take())
}
