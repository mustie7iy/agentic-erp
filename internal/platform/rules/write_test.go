package rules

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// source is a fixed Source: a tenant's rules in one version, or an error.
type source struct {
	a   Active
	err error
}

func (s source) Rules(context.Context, string) (Active, error) { return s.a, s.err }

// germanVAT is a business partner rule read against a schema with one
// custom field, in configuration version 7.
func germanVAT() Active {
	return Active{ConfigVersion: 7,
		Schema: &extensions.Schema{Version: 1, Type: "object", AdditionalProperties: new(bool),
			Properties: map[string]extensions.Property{"vat_id": {Type: "string", PII: extensions.PIINone}}},
		Rules: map[string]Rule{"german_vat": {Expression: `row.country_code != "DE" || ext.vat_id.orValue("") != ""`, Message: "a German partner needs a VAT id",
			Cases: []Case{{Row: partner("FR"), Valid: true}, {Row: partner("DE"), Valid: false}}}}}
}

// inRequest is a request's context for a new tenant, with its audit
// buffer; the rules read no database themselves.
func inRequest(t *testing.T) (context.Context, *audit.Buffer) {
	t.Helper()
	ctx := reqctx.WithPrincipal(reqctx.WithTx(context.Background(), &pgtest.FailingTx{}), reqctx.Principal{ID: uuid.New(), TenantID: uuid.New()})
	return audit.WithBuffer(ctx)
}

func write(country string) Write {
	return Write{Action: "partner.business_partner.create", Row: partner(country)}
}

// TestWritesCheck: a refusing rule is 422 request.rule_refused naming the
// rule with its message, and the refusal is buffered to outlive the
// rollback; an accepted write passes and records nothing.
func TestWritesCheck(t *testing.T) {
	ctx, buf := inRequest(t)
	w := Writes{Entity: "business_partner", Source: source{a: germanVAT()}}
	if err := w.Check(ctx, write("FR")); err != nil {
		t.Fatalf("accepted write: got %v, want none", err)
	}
	if buf.Len() != 0 {
		t.Errorf("audit of an accepted write: got %d entries, want 0", buf.Len())
	}
	err := w.Check(ctx, write("DE"))
	var p *problem.Problem
	if !errors.As(err, &p) || p.Status != 422 || p.Code != "request.rule_refused" {
		t.Fatalf("refused write: got %v, want 422 request.rule_refused", err)
	}
	want := problem.FieldError{Field: "rules.german_vat", Code: "rule_refused", Message: "a German partner needs a VAT id"}
	if len(p.Errors) != 1 || p.Errors[0] != want {
		t.Errorf("field errors: got %v, want [%v]", p.Errors, want)
	}
	if buf.Len() != 1 {
		t.Errorf("refusals buffered: got %d, want 1", buf.Len())
	}
}

// TestWritesHold: a write that lands held gets a reason naming each
// refusing rule and its message, and the hold is audited with the write.
func TestWritesHold(t *testing.T) {
	ctx, buf := inRequest(t)
	w := Writes{Entity: "business_partner", Source: source{a: germanVAT()}}
	reason, err := w.Hold(ctx, write("FR"))
	if err != nil || reason != "" {
		t.Fatalf("accepted: got %q, %v, want no reason", reason, err)
	}
	reason, err = w.Hold(ctx, write("DE"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Rule german_vat refuses it: a German partner needs a VAT id."; reason != want {
		t.Errorf("reason: got %q, want %q", reason, want)
	}
	if buf.Len() != 1 {
		t.Errorf("audit of the hold: got %d entries, want 1", buf.Len())
	}
}

// TestWritesFailures: what keeps the rules from judging a write is an
// error, never a pass.
func TestWritesFailures(t *testing.T) {
	boom := errors.New("boom")
	bad := germanVAT()
	bad.Rules = map[string]Rule{"german_vat": {Expression: "row.nope", Message: "m"}}
	badSchema := germanVAT()
	badSchema.Schema = &extensions.Schema{Version: 1, Type: "array"}
	for name, c := range map[string]struct {
		w    Writes
		wr   Write
		want string
	}{
		"no reader":        {Writes{Entity: "business_partner"}, write("DE"), "no rule reader"},
		"read":             {Writes{Entity: "business_partner", Source: source{err: boom}}, write("DE"), "read the business_partner rules: boom"},
		"schema invalid":   {Writes{Entity: "business_partner", Source: source{a: badSchema}}, write("DE"), "schema of configuration version 7 does not compile"},
		"rules invalid":    {Writes{Entity: "business_partner", Source: source{a: bad}}, write("DE"), "rules of configuration version 7 do not compile"},
		"input wrong type": {Writes{Entity: "business_partner", Source: source{a: germanVAT()}}, Write{Row: map[string]any{"name": 1}}, "configuration version 7: rules: business_partner"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, _ := inRequest(t)
			if err := c.w.Check(ctx, c.wr); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("check: got %v, want %q", err, c.want)
			}
			if _, err := c.w.Hold(ctx, c.wr); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("hold: got %v, want %q", err, c.want)
			}
		})
	}
	w := Writes{Entity: "business_partner", Source: source{a: germanVAT()}}
	t.Run("outside a request", func(t *testing.T) {
		if err := w.Check(context.Background(), write("DE")); !errors.Is(err, reqctx.ErrOutsideRequest) {
			t.Errorf("got %v, want %v", err, reqctx.ErrOutsideRequest)
		}
	})
	t.Run("no audit buffer", func(t *testing.T) {
		ctx := reqctx.WithPrincipal(reqctx.WithTx(context.Background(), &pgtest.FailingTx{}), reqctx.Principal{ID: uuid.New(), TenantID: uuid.New()})
		if err := w.Check(ctx, write("DE")); !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("check: got %v, want %v", err, audit.ErrNoBuffer)
		}
		if _, err := w.Hold(ctx, write("DE")); !errors.Is(err, audit.ErrNoBuffer) {
			t.Errorf("hold: got %v, want %v", err, audit.ErrNoBuffer)
		}
	})
}

// A tenant without rules on the entity has every write accepted.
func TestWritesWithoutRules(t *testing.T) {
	ctx, _ := inRequest(t)
	if err := (Writes{Entity: "business_partner", Source: source{}}).Check(ctx, write("DE")); err != nil {
		t.Errorf("got %v, want none", err)
	}
}

// A configuration version's rules compile once: the version is immutable,
// so a second write reads the compiled set, and a full cache starts over.
func TestCompiledSets(t *testing.T) {
	k := setKey{uuid.New(), 7, "business_partner"}
	first, err := compiledSet(k, germanVAT())
	if err != nil {
		t.Fatal(err)
	}
	again, err := compiledSet(k, Active{ConfigVersion: 7, Rules: map[string]Rule{"x": {}}}) // would not compile
	if err != nil || again != first {
		t.Errorf("second read: got %p, %v, want the compiled set %p", again, err, first)
	}
	cache.Lock()
	for len(cache.sets) < maxCached {
		cache.sets[setKey{uuid.New(), 1, "business_partner"}] = first
	}
	cache.Unlock()
	if _, err := compiledSet(setKey{uuid.New(), 7, "business_partner"}, germanVAT()); err != nil {
		t.Fatal(err)
	}
	cache.Lock()
	n := len(cache.sets)
	cache.Unlock()
	if n != 1 {
		t.Errorf("sets after a full cache: got %d, want 1", n)
	}
}

// Optional inputs read nil as absent and a value as its string.
func TestOptionalInputs(t *testing.T) {
	s, id, d := "x", uuid.New(), time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct{ got, want any }{
		"text":    {Text(&s), "x"},
		"no text": {Text(nil), nil},
		"id":      {ID(&id), id.String()},
		"no id":   {ID(nil), nil},
		"date":    {Date(&d), "2026-10-20"},
		"no date": {Date(nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %v, want %v", c.got, c.want)
			}
		})
	}
}
