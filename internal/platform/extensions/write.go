package extensions

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Schemas reads a tenant's custom field schemas (configuration context).
type Schemas interface {
	// Schema returns the entity's schema in the configuration version the
	// request's transaction reads, nil when the tenant declares none.
	Schema(ctx context.Context, entity string) (*Schema, error)
}

// References says whether the record a reference field names exists, read
// in the request's transaction (partner context).
type References interface {
	Exists(ctx context.Context, kind Kind, id uuid.UUID) (bool, error)
}

// Writes checks the custom fields one entity's rows are written with
// (ADR-0046).
type Writes struct {
	Entity     string
	Schemas    Schemas
	References References
}

// Check validates ext, the whole extensions a row is written with, against
// the tenant's active schema of the entity: unknown keys, required fields,
// types and limits, money with its currency's minor-unit digits, and
// references to records that exist. It returns the schema version to write
// with the row, nil when the tenant declares no schema, and then every key
// is unknown. A mismatch is 422 request.invalid_extension with one field
// error per offending key, in key order. No message repeats a value, so a
// personal or sensitive value never reaches a log through the error.
func (w Writes) Check(ctx context.Context, ext map[string]any) (*int, error) {
	if b, _ := json.Marshal(ext); len(b) > MaxRowBytes { // decoded JSON: it always encodes
		return nil, refused(problem.FieldError{Field: "extensions", Code: "too_large", Message: fmt.Sprintf("at most %d bytes of custom fields", MaxRowBytes)})
	}
	c, err := w.schema(ctx)
	if err != nil {
		return nil, err
	}
	errs, err := c.check(ctx, w.References, ext)
	if err != nil {
		return nil, fmt.Errorf("extensions: check %s: %w", w.Entity, err)
	}
	if len(errs) > 0 {
		return nil, refused(errs...)
	}
	if c.Version == 0 {
		return nil, nil
	}
	return &c.Version, nil
}

// schema compiles the entity's active schema; a tenant without one has an
// empty schema of version 0.
func (w Writes) schema(ctx context.Context) (*Compiled, error) {
	s, err := w.Schemas.Schema(ctx, w.Entity)
	if err != nil {
		return nil, fmt.Errorf("extensions: read the %s schema: %w", w.Entity, err)
	}
	if s == nil {
		return &Compiled{Entity: w.Entity}, nil
	}
	c, issues := Compile(w.Entity, *s)
	if len(issues) > 0 { // the configuration context refuses such a schema before it activates
		return nil, fmt.Errorf("extensions: the active %s schema version %d does not compile: %v", w.Entity, s.Version, issues)
	}
	return c, nil
}

func refused(errs ...problem.FieldError) error {
	return problem.Unprocessable("request.invalid_extension", "The custom fields do not match the tenant's schema.", errs...)
}

// check returns a field error per offending key, unknown and missing keys
// included, in key order.
func (c *Compiled) check(ctx context.Context, refs References, ext map[string]any) ([]problem.FieldError, error) {
	var out []problem.FieldError
	for _, f := range c.Fields {
		if _, ok := ext[f.Key]; f.Required && !ok {
			out = append(out, problem.FieldError{Field: "extensions." + f.Key, Code: "required", Message: "a required custom field"})
		}
	}
	for _, k := range slices.Sorted(maps.Keys(ext)) {
		f, ok := c.Field(k)
		if !ok {
			out = append(out, problem.FieldError{Field: "extensions." + k, Code: "unknown_key", Message: "not a custom field of " + c.Entity})
			continue
		}
		code, msg, err := kinds[f.Kind](ctx, refs, f, ext[k])
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", k, err)
		}
		if code != "" {
			out = append(out, problem.FieldError{Field: "extensions." + k, Code: code, Message: msg})
		}
	}
	slices.SortStableFunc(out, func(a, b problem.FieldError) int { return cmp.Compare(a.Field, b.Field) })
	return out, nil
}

// A value check returns the field error's code and message, both empty
// when the value is valid, or the error of a failed read.
type valueCheck func(ctx context.Context, refs References, f Field, v any) (code, message string, err error)

var kinds = map[Kind]valueCheck{
	String: pure(text), Text: pure(text), Integer: pure(integer), Decimal: pure(decimal), Date: pure(date),
	Boolean: pure(boolean), Enum: pure(enum), Money: moneyValue, Partner: reference, CostObject: reference,
}

func pure(check func(Field, any) (string, string)) valueCheck {
	return func(_ context.Context, _ References, f Field, v any) (string, string, error) {
		code, msg := check(f, v)
		return code, msg, nil
	}
}

func text(f Field, v any) (string, string) {
	s, ok := v.(string)
	if !ok {
		return "invalid_type", "a string"
	}
	if utf8.RuneCountInString(s) > f.MaxLength {
		return "too_long", fmt.Sprintf("at most %d characters", f.MaxLength)
	}
	return "", ""
}

// maxExact is the largest integer a JSON number decodes to exactly.
const maxExact = 1 << 53

// integer reads a JSON number as it encodes: a whole number has no
// fraction or exponent there, and every other value is no number at all.
func integer(f Field, v any) (string, string) {
	b, _ := json.Marshal(v) // decoded JSON: it always encodes
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n < -maxExact || n > maxExact {
		return "invalid_type", fmt.Sprintf("a whole number between -%d and %d", maxExact, maxExact)
	}
	if (f.Minimum != nil && n < *f.Minimum) || (f.Maximum != nil && n > *f.Maximum) {
		return "out_of_range", "outside the field's minimum and maximum"
	}
	return "", ""
}

func decimal(_ Field, v any) (string, string) {
	if s, ok := v.(string); ok {
		if _, err := money.ParseDecimalRat(s); err == nil {
			return "", ""
		}
	}
	return "invalid_type", `a decimal string such as "12.50", never a JSON number`
}

func date(_ Field, v any) (string, string) {
	if s, ok := v.(string); ok {
		if _, err := time.Parse(time.DateOnly, s); err == nil {
			return "", ""
		}
	}
	return "invalid_type", "a date, YYYY-MM-DD"
}

func boolean(_ Field, v any) (string, string) {
	if _, ok := v.(bool); !ok {
		return "invalid_type", "true or false"
	}
	return "", ""
}

func enum(f Field, v any) (string, string) {
	if s, ok := v.(string); !ok || !slices.Contains(f.Enum, s) {
		return "not_allowed", "one of the field's values"
	}
	return "", ""
}

var currencyCode = regexp.MustCompile(`^[A-Z]{3}$`)

var errUnknownCurrency = errors.New("unknown currency")

// moneyValue reads the API's Money shape (ADR-0005): a decimal string with
// exactly the currency's minor-unit digits, and the currency.
func moneyValue(ctx context.Context, _ References, _ Field, v any) (string, string, error) {
	const shape = `an amount and a currency, {"amount": "12.50", "currency": "EUR"}`
	m, ok := v.(map[string]any)
	amount, okA := m["amount"].(string)
	code, okC := m["currency"].(string)
	if !ok || len(m) != 2 || !okA || !okC || !currencyCode.MatchString(code) {
		return "invalid_type", shape, nil
	}
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return "", "", err
	}
	exp, err := currency.Exponent(ctx, tx, code, errUnknownCurrency)
	if errors.Is(err, errUnknownCurrency) {
		return "invalid", "an unknown currency", nil
	}
	if err != nil {
		return "", "", err
	}
	if !validAmount(amount, exp) {
		return "invalid", fmt.Sprintf("an amount with exactly %d decimals for the currency", exp), nil
	}
	return "", "", nil
}

func validAmount(s string, exp int) bool {
	_, err := money.ParseAmount(s, exp)
	return err == nil
}

// reference checks that a partner or cost object id names a record.
func reference(ctx context.Context, refs References, f Field, v any) (string, string, error) {
	s, _ := v.(string) // not a string: refused as no id below
	id, ok := parseID(s)
	if !ok {
		return "invalid_type", "an id", nil
	}
	exists, err := refs.Exists(ctx, f.Kind, id)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return "not_found", fmt.Sprintf("no %s with this id", f.Kind), nil
	}
	return "", "", nil
}

func parseID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil
}
