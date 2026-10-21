package extensions_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/extensions"
	"github.com/mustie7iy/agentic-erp/internal/platform/fixture"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

type schemas struct {
	s   *extensions.Schema
	err error
}

func (f schemas) Schema(context.Context, string) (*extensions.Schema, error) { return f.s, f.err }

type refs struct{ err error }

func (r refs) Exists(context.Context, extensions.Kind, uuid.UUID) (bool, error) { return false, r.err }

func every() *extensions.Schema {
	lo, hi := int64(0), int64(10)
	none := extensions.PIINone
	return &extensions.Schema{Version: 2, Type: "object", AdditionalProperties: new(bool), Required: []string{"req"},
		Properties: map[string]extensions.Property{
			"s": {Type: "string", MaxLength: 3, PII: none}, "t": {Type: "string", Format: "text", PII: extensions.PIIPersonal},
			"n": {Type: "integer", Minimum: &lo, Maximum: &hi, PII: none}, "d": {Type: "string", Format: "decimal", PII: none},
			"m": {Type: "object", Format: "money", PII: none}, "dt": {Type: "string", Format: "date", PII: none},
			"b": {Type: "boolean", PII: none}, "e": {Type: "string", Enum: []string{"a", "b"}, PII: none},
			"p":   {Type: "string", Format: "partner", PII: extensions.PIISensitive},
			"req": {Type: "string", PII: none},
		}}
}

// refused lists the field errors of a refusal as field:code.
func refused(t *testing.T, err error) []string {
	t.Helper()
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != "request.invalid_extension" || p.Status != 422 {
		t.Fatalf("got %v, want 422 request.invalid_extension", err)
	}
	var out []string
	for _, f := range p.Errors {
		out = append(out, f.Field+":"+f.Code)
	}
	return out
}

// TestWritesCheck: each offending key gets one field error, in key order,
// and a valid row is written under the schema's version.
func TestWritesCheck(t *testing.T) {
	f := fixture.New(t)
	w := extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: partner.References{}}
	valid := func() map[string]any {
		return map[string]any{"req": "r", "s": "abc", "t": "long text", "n": float64(10), "d": "-12.345", "m": map[string]any{"amount": "1.50", "currency": "EUR"},
			"dt": "2026-02-28", "b": false, "e": "b", "p": f.Supplier.String()}
	}
	for name, c := range map[string]struct {
		change map[string]any
		want   string // the field errors, "" for none
	}{
		"valid":            {nil, ""},
		"unknown key":      {map[string]any{"zz": 1}, "extensions.zz:unknown_key"},
		"missing required": {map[string]any{"req": nil}, "extensions.req:required"},
		"too long":         {map[string]any{"s": "abcd"}, "extensions.s:too_long"},
		"not a string":     {map[string]any{"s": float64(1)}, "extensions.s:invalid_type"},
		"fraction":         {map[string]any{"n": 1.5}, "extensions.n:invalid_type"},
		"inexact integer":  {map[string]any{"n": float64(1 << 60)}, "extensions.n:invalid_type"},
		"above maximum":    {map[string]any{"n": float64(11)}, "extensions.n:out_of_range"},
		"below minimum":    {map[string]any{"n": float64(-1)}, "extensions.n:out_of_range"},
		"decimal number":   {map[string]any{"d": 1.5}, "extensions.d:invalid_type"},
		"decimal exponent": {map[string]any{"d": "1e3"}, "extensions.d:invalid_type"},
		"money digits":     {map[string]any{"m": map[string]any{"amount": "1.5", "currency": "EUR"}}, "extensions.m:invalid"},
		"money currency":   {map[string]any{"m": map[string]any{"amount": "1.50", "currency": "QQQ"}}, "extensions.m:invalid"},
		"money shape":      {map[string]any{"m": map[string]any{"amount": "1.50", "currency": "EUR", "x": 1}}, "extensions.m:invalid_type"},
		"money number":     {map[string]any{"m": map[string]any{"amount": 1.5, "currency": "EUR"}}, "extensions.m:invalid_type"},
		"money lower code": {map[string]any{"m": map[string]any{"amount": "1.50", "currency": "eur"}}, "extensions.m:invalid_type"},
		"bad date":         {map[string]any{"dt": "2026-02-30"}, "extensions.dt:invalid_type"},
		"date number":      {map[string]any{"dt": float64(1)}, "extensions.dt:invalid_type"},
		"boolean string":   {map[string]any{"b": "true"}, "extensions.b:invalid_type"},
		"enum value":       {map[string]any{"e": "c"}, "extensions.e:not_allowed"},
		"partner id":       {map[string]any{"p": "P1"}, "extensions.p:invalid_type"},
		"partner missing":  {map[string]any{"p": uuid.NewString()}, "extensions.p:not_found"},
		"several in order": {map[string]any{"zz": 1, "b": 1, "req": nil}, "extensions.b:invalid_type extensions.req:required extensions.zz:unknown_key"},
	} {
		t.Run(name, func(t *testing.T) {
			ext := valid()
			for k, v := range c.change {
				if v == nil {
					delete(ext, k)
				} else {
					ext[k] = v
				}
			}
			var version *int
			err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) (err error) {
				version, err = w.Check(ctx, ext)
				return err
			})
			if c.want == "" {
				if err != nil || version == nil || *version != 2 {
					t.Fatalf("got version %v, error %v; want version 2", version, err)
				}
				return
			}
			if got := strings.Join(refused(t, err), " "); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestReferencesOtherKinds: the partner context holds business partners
// only, so it knows no record of another kind.
func TestReferencesOtherKinds(t *testing.T) {
	if ok, err := (partner.References{}).Exists(context.Background(), extensions.Kind("cost_object"), uuid.New()); ok || err != nil {
		t.Errorf("cost object: got %v, %v; want false, nil", ok, err)
	}
}

// A refusal never repeats the value: the messages reach logs, and a
// personal or sensitive value must not.
func TestWritesCheckRepeatsNoValue(t *testing.T) {
	f := fixture.New(t)
	w := extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: partner.References{}}
	secret := uuid.NewString()
	err := apitest.InRequest(t, f.Tenant, f.Tenant.Human, func(ctx context.Context) error {
		_, err := w.Check(ctx, map[string]any{"req": "r", "p": secret, "t": float64(7), "e": "Geheim"})
		return err
	})
	if s := fmt.Sprint(problem.From(err).Errors); strings.Contains(s, secret) || strings.Contains(s, "Geheim") {
		t.Errorf("got %s, want no value repeated", s)
	}
}

// Without a schema the entity has no custom fields: none is the only
// valid row, written without a version.
func TestWritesCheckWithoutSchema(t *testing.T) {
	w := extensions.Writes{Entity: "purchase_order", Schemas: schemas{}, References: refs{}}
	if v, err := w.Check(context.Background(), map[string]any{}); v != nil || err != nil {
		t.Errorf("no custom fields: got %v, %v; want no version, no error", v, err)
	}
	_, err := w.Check(context.Background(), map[string]any{"cost_center": "A"})
	if got := strings.Join(refused(t, err), " "); got != "extensions.cost_center:unknown_key" {
		t.Errorf("got %s, want extensions.cost_center:unknown_key", got)
	}
	_, err = w.Check(context.Background(), map[string]any{"a": strings.Repeat("x", extensions.MaxRowBytes)})
	if got := strings.Join(refused(t, err), " "); got != "extensions:too_large" {
		t.Errorf("got %s, want extensions:too_large", got)
	}
}

// A row that lands held may miss required fields, which are returned;
// any other mismatch is refused as by Check.
func TestWritesCheckHeld(t *testing.T) {
	w := extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: refs{}}
	v, missing, err := w.CheckHeld(context.Background(), map[string]any{"s": "abc"})
	if err != nil || v == nil || *v != 2 || strings.Join(missing, ",") != "req" {
		t.Errorf("got version %v, missing %v, error %v; want version 2, missing req", v, missing, err)
	}
	_, _, err = w.CheckHeld(context.Background(), map[string]any{"s": "abcd"})
	if got := strings.Join(refused(t, err), " "); got != "extensions.s:too_long" {
		t.Errorf("got %s, want extensions.s:too_long", got)
	}
	none := extensions.Writes{Entity: "purchase_order", Schemas: schemas{}}
	if v, missing, err := none.CheckHeld(context.Background(), nil); v != nil || missing != nil || err != nil {
		t.Errorf("without a schema: got %v, %v, %v; want nothing", v, missing, err)
	}
	if _, _, err := none.CheckHeld(context.Background(), map[string]any{"a": strings.Repeat("x", extensions.MaxRowBytes)}); err == nil {
		t.Error("too large: got no error, want one")
	}
}

func TestWritesCheckFailures(t *testing.T) {
	boom := errors.New("boom")
	bad := every()
	bad.Type = "array"
	for name, c := range map[string]struct {
		w   extensions.Writes
		ext map[string]any
	}{
		"schema read":      {extensions.Writes{Schemas: schemas{err: boom}}, nil},
		"no schema reader": {extensions.Writes{Entity: "supplier_invoice"}, nil},
		"schema invalid":   {extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: bad}}, nil},
		"reference read":   {extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: refs{err: boom}}, map[string]any{"req": "r", "p": uuid.NewString()}},
		"partner outside a request": {extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: partner.References{}},
			map[string]any{"req": "r", "p": uuid.NewString()}},
		"outside a request": {extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}}, map[string]any{"req": "r", "m": map[string]any{"amount": "1", "currency": "JPY"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if v, err := c.w.Check(context.Background(), c.ext); err == nil || problem.From(err).Status != 500 {
				t.Errorf("got %v, %v; want an internal error", v, err)
			}
		})
	}
	f := fixture.New(t)
	w := extensions.Writes{Entity: "supplier_invoice", Schemas: schemas{s: every()}, References: partner.References{}}
	apitest.EveryFailure(t, f.Tenant, f.Tenant.Human, "check", func(ctx context.Context) error {
		_, err := w.Check(ctx, map[string]any{"req": "r", "m": map[string]any{"amount": "1", "currency": "JPY"}, "p": f.Supplier.String()})
		return err
	})
}

func TestMain(m *testing.M) { pgtest.Main(m) }
