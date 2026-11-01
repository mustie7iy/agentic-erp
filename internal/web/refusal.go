package web

import (
	"net/http"
	"reflect"
	"slices"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// problemType is the body of every refusal of the typed client.
var problemType = reflect.TypeFor[gen.Problem]()

// problemOf is the problem an API answer carries. A refusal the contract
// declares response headers on (UnauthorizedHeaders, ConflictHeaders,
// TooManyRequestsHeaders, …) holds it as its Response; any other refusal
// is a Problem under its operation's name (ListLegalEntitiesForbidden, …).
func problemOf(res any) (gen.Problem, bool) {
	v := reflect.Indirect(reflect.ValueOf(res))
	if v.Kind() != reflect.Struct {
		return gen.Problem{}, false
	}
	if v.Type().ConvertibleTo(problemType) {
		return v.Convert(problemType).Interface().(gen.Problem), true
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
// (getCurrentPrincipal), a refused tax its figures.
func explain(title i18n.Key, prob gen.Problem, m me) *ui.Refusal {
	r := &ui.Refusal{Title: title, Message: i18n.Problems[prob.Code], Fallback: prob.Title, Code: prob.Code, Trace: prob.TraceID.Or("")}
	if slices.Contains(segregation, prob.Code) {
		r.Barred = m.Barred
	}
	if t, ok := prob.CalculatedTax.Get(); ok {
		r.Tax = taxFigures(t)
	}
	return r
}

// taxFigures are a refused tax's figures as the page shows them
// (payables.tax_mismatch, ADR-0052 decision D3): in the person's words
// rather than the detail's English.
func taxFigures(t gen.CalculatedTax) *ui.TaxFigures {
	f := &ui.TaxFigures{Stated: moneyOf(t.Stated), Calculated: moneyOf(t.Calculated)}
	for _, c := range t.Codes {
		f.Codes = append(f.Codes, ui.CodeTax{Code: c.TaxCode, Tax: moneyOf(c.Tax)})
	}
	return f
}

// inPlace is the refusal of one of a page's reads, explained in the
// screen's words and shown in the read's place under its own id. ok is
// false when the answer was written instead (apiFailed).
func (s *Server) inPlace(w http.ResponseWriter, r *http.Request, p *person, m me, screen map[string]i18n.Key, operation string, res any, err error,
	title i18n.Key, id string) (*ui.Refusal, bool) {
	prob, show := s.refusal(w, r, p, operation, res, err)
	if !show {
		return nil, false
	}
	refused := explainIn(screen, title, prob, m)
	refused.ID = id
	return refused, true
}
