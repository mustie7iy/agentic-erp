package web

import (
	"net/http"
	"reflect"
	"slices"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// problemOf is the problem an API answer carries: every refusal of the
// typed client (BadRequestHeaders, ForbiddenHeaders, ConflictHeaders, …)
// holds it as its Response.
func problemOf(res any) (gen.Problem, bool) {
	v := reflect.Indirect(reflect.ValueOf(res))
	if v.Kind() != reflect.Struct {
		return gen.Problem{}, false
	}
	f := v.FieldByName("Response")
	if !f.IsValid() {
		return gen.Problem{}, false
	}
	p, ok := f.Interface().(gen.Problem)
	return p, ok
}

// refusal is the API's refusal of a page's call when the person is to be
// told why: a problem with a 4xx status but 401 and 429. ok is false when
// the answer was written instead (apiFailed): the API unreachable, its 401,
// a rate limit, a server error or an answer that is no problem.
func (s *Server) refusal(w http.ResponseWriter, r *http.Request, p *person, operation string, res any, err error) (gen.Problem, bool) {
	prob, ok := problemOf(res)
	if err != nil || !ok || prob.Status < 400 || prob.Status >= 500 || prob.Status == http.StatusUnauthorized ||
		prob.Status == http.StatusTooManyRequests {
		s.apiFailed(w, r, p, operation, res, err)
		return gen.Problem{}, false
	}
	return prob, true
}

// segregation are the codes of refusals by segregation of duties: a rule
// barring permissions held together, or an object's own (whoever matched
// a bank line cannot approve its posting).
var segregation = []string{"identity.sod_conflict", "workflow.segregation_of_duties"}

// explain is a problem as the page says it (ADR-0052 §8): by its code in
// the person's language, the API's own title for a code the catalog does
// not hold, with the code and the trace id for support; a
// segregation-of-duties refusal lists the person's barred permissions
// (getCurrentPrincipal).
func explain(title i18n.Key, prob gen.Problem, m me) *ui.Refusal {
	r := &ui.Refusal{Title: title, Message: i18n.Problems[prob.Code], Fallback: prob.Title, Code: prob.Code, Trace: prob.TraceID.Or("")}
	if slices.Contains(segregation, prob.Code) {
		r.Barred = m.Barred
	}
	return r
}
