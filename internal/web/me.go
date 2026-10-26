package web

import (
	"context"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// me is the signed-in person as getCurrentPrincipal says (ADR-0052 G2):
// who they are, the permissions they may use now, and those a
// segregation-of-duties rule bars. It is advisory: the workspace builds
// its navigation from it and hides what the person can never do, and the
// API still decides every call (§7).
type me struct {
	ID          uuid.UUID
	permissions map[string]bool
	Barred      []string
}

// may reports whether the person may use the permission now.
func (m me) may(permission string) bool { return m.permissions[permission] }

// whoami reads the signed-in person. ok is false when the answer is
// already written: the API's 401 (sign in again) or an API that could not
// answer.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request, p *person) (me, bool) {
	res, err := s.API.GetCurrentPrincipal(r.Context())
	switch res := res.(type) {
	case *gen.CurrentPrincipal:
		m := me{ID: res.ID, permissions: map[string]bool{}, Barred: res.BarredPermissions}
		for _, perm := range res.Permissions {
			m.permissions[perm] = true
		}
		return m, true
	case *gen.GetCurrentPrincipalForbidden:
		// The contract lets every principal read itself; a refusal leaves
		// nothing to offer, and each screen shows the API's own answer.
		return me{}, true
	}
	s.apiFailed(w, r, p, "getCurrentPrincipal", res, err)
	return me{}, false
}

// apiFailed answers an API call that did not succeed and that the page does
// not explain: its 401 signs the person in again (or steps up); anything
// else (the API unreachable, a rate limit, a server error, an answer the
// page did not expect) is logged and the page is unavailable.
func (s *Server) apiFailed(w http.ResponseWriter, r *http.Request, p *person, operation string, res any, err error) {
	if u, ok := res.(*gen.UnauthorizedHeaders); ok {
		s.unauthorized(w, r, p, u)
		return
	}
	if err != nil {
		s.Log.ErrorContext(r.Context(), "web: the API could not be reached", "operation", operation, "error", err)
	} else {
		s.Log.WarnContext(r.Context(), "web: the API refused a page's call", "operation", operation, "answer", res)
	}
	s.unavailable(w, r)
}

// page is the shell of a signed-in person's page, with the navigation
// their permissions offer.
func (s *Server) page(title i18n.Key, current string, p *person, m me) ui.Shell {
	sh := s.shell(title, current)
	sh.Person = &ui.Person{CSRF: p.CSRF}
	sh.Nav = ui.SignedInNav(ui.NavJobs{Approvals: m.may(permReadApprovals), Invoices: m.may(permReadInvoices), Bank: m.may(permReadStatements),
		Payments: m.may(permReadRuns), Journal: m.may(permReadEntries), Close: m.may(permReadPeriods), Reports: m.mayReport()})
	return sh
}

// Names are cached per session for five minutes (ADR-0052 G3), for at most
// maxNamedSessions sessions per replica.
const (
	nameTTL          = 5 * time.Minute
	maxNamedSessions = 10000
	namesPerCall     = 100 // listPrincipalNames's limit
)

// nameCache holds the names each session was given, for nameTTL from
// the first. Names are no business data and hold nothing a session may
// not read again; the cache is kept per session all the same, so that one
// person's call never names principals for another.
type nameCache struct {
	mu       sync.Mutex
	sessions map[string]namedSession
	max      int // 0: maxNamedSessions
}

type namedSession struct {
	until time.Time
	names map[uuid.UUID]ui.Name
}

// get is a copy of the session's names, none once they expired.
func (c *nameCache) get(session string, now time.Time) map[uuid.UUID]ui.Name {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[uuid.UUID]ui.Name{}
	if e, ok := c.sessions[session]; ok && now.Before(e.until) {
		maps.Copy(out, e.names)
	}
	return out
}

// put adds names to the session's. A session's first names start its
// five minutes; a full cache drops the expired sessions, and is emptied
// when none has expired.
func (c *nameCache) put(session string, now time.Time, names map[uuid.UUID]ui.Name) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.sessions[session]
	if !ok || !now.Before(e.until) {
		limit := c.max
		if limit == 0 {
			limit = maxNamedSessions
		}
		if len(c.sessions) >= limit {
			maps.DeleteFunc(c.sessions, func(_ string, e namedSession) bool { return !now.Before(e.until) })
		}
		if c.sessions == nil || len(c.sessions) >= limit {
			c.sessions = map[string]namedSession{}
		}
		e = namedSession{until: now.Add(nameTTL), names: map[uuid.UUID]ui.Name{}}
		c.sessions[session] = e
	}
	maps.Copy(e.names, names)
}

// names are the principals' names (listPrincipalNames, ADR-0052 G3): the
// session's cached ones, then one call per hundred of the others. An id
// the API does not name is remembered as unknown. A call that fails
// leaves its names unknown, logged: names are no reason to refuse a page.
func (s *Server) names(ctx context.Context, p *person, ids []uuid.UUID) map[uuid.UUID]ui.Name {
	now := s.now()
	known := s.nameCache.get(p.ID, now)
	var missing []uuid.UUID
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			known[id] = ui.Name{} // asked once, even when listed twice
			missing = append(missing, id)
		}
	}
	for len(missing) > 0 {
		batch := missing[:min(len(missing), namesPerCall)]
		missing = missing[len(batch):]
		res, err := s.API.ListPrincipalNames(ctx, gen.ListPrincipalNamesParams{Ids: batch})
		page, ok := res.(*gen.PrincipalNames)
		if !ok {
			s.Log.WarnContext(ctx, "web: principals' names could not be read", "answer", res, "error", err)
			return known
		}
		found := map[uuid.UUID]ui.Name{}
		for _, id := range batch {
			found[id] = ui.Name{}
		}
		for _, n := range page.Items {
			found[n.ID] = ui.Name{Text: n.DisplayName, Agent: n.Kind == "agent"}
		}
		s.nameCache.put(p.ID, now, found)
		maps.Copy(known, found)
	}
	return known
}
