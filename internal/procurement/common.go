package procurement

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Statuses.
const (
	Draft           = "draft"
	PendingApproval = "pending_approval"
	Approved        = "approved"
	Rejected        = "rejected"
	Cancelled       = "cancelled"
	Ordered         = "ordered"
	Released        = "released"
	Closed          = "closed"
	Active          = "active"
	Terminated      = "terminated"
)

func init() {
	problem.RegisterConstraint("purchase_order_one_per_requisition", func() *problem.Problem {
		return problem.Conflict("procurement.requisition_ordered", "The requisition already has a live purchase order.")
	})
	problem.RegisterConstraint("requisition_item_uom_code_fkey", unknownUnit)
	problem.RegisterConstraint("purchase_order_item_uom_code_fkey", unknownUnit)
	problem.RegisterConstraint("contract_item_uom_code_fkey", unknownUnit)
	problem.RegisterConstraint("contract_item_material_key", func() *problem.Problem {
		return problem.Unprocessable("procurement.duplicate_material_key", "Each material key appears once per contract.",
			problem.FieldError{Field: "items", Code: "duplicate", Message: "a material key used once"})
	})
}

func unknownUnit() *problem.Problem {
	return problem.Unprocessable("procurement.unknown_unit", "A unit of measure is not known.",
		problem.FieldError{Field: "items", Code: "unknown_unit", Message: "a unit of measure code from the reference data"})
}

func notFound(what string) error { return problem.NotFound("No " + what + " with this id.") }

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// reference turns "not found" for a referenced document into 422 on the
// referencing field; other errors pass.
func reference(err error, code, field, message string) error {
	if problem.From(err).Status == 404 {
		return invalid(code, field, message)
	}
	return err
}

// statusConflict refuses a transition from the current status.
func statusConflict(what, current, action string, allowed ...string) error {
	return problem.Conflict("procurement.invalid_status", fmt.Sprintf("The %s is %s; %s requires status %v.", what, current, action, allowed))
}

func allows(current string, allowed ...string) bool {
	for _, a := range allowed {
		if current == a {
			return true
		}
	}
	return false
}

// record publishes the events and adds the audit entry of a change.
func record(ctx context.Context, action, objectType string, id uuid.UUID, detail map[string]any, events ...outbox.Event) error {
	for _, e := range events {
		if _, err := outbox.Add(ctx, e); err != nil {
			return err
		}
	}
	return audit.Add(ctx, audit.Entry{Action: action, ObjectType: objectType, ObjectID: id, Outcome: "succeeded", Detail: detail})
}

func event(name, aggregate string, id uuid.UUID, payload map[string]any) outbox.Event {
	return outbox.Event{Context: "procurement", Name: name, AggregateType: "procurement." + aggregate, AggregateID: id, Payload: payload}
}

var extensionKey = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkExtensions enforces snake_case custom field keys (ADR-0009).
func checkExtensions(ext map[string]any) error {
	for k := range ext {
		if !extensionKey.MatchString(k) {
			return problem.Unprocessable("request.invalid_extension", "Custom field keys are snake_case.",
				problem.FieldError{Field: "extensions." + k, Code: "invalid_key", Message: "snake_case, starting with a letter"})
		}
	}
	return nil
}

// mergePatch applies an RFC 7396 merge patch to custom fields.
func mergePatch(target, patch map[string]any) map[string]any {
	out := make(map[string]any, len(target))
	for k, v := range target {
		out[k] = v
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			tv, _ := out[k].(map[string]any)
			out[k] = mergePatch(tv, pv)
		default:
			out[k] = v
		}
	}
	return out
}

// today is the database's current date, the date documents without one
// are numbered by.
func today(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var d time.Time
	err := tx.QueryRow(ctx, `SELECT current_date`).Scan(&d)
	return d, err
}

// exponent returns the currency's minor-unit digits; an unknown currency is
// 422 on field.
func exponent(ctx context.Context, tx pgx.Tx, currency, field string) (int, error) {
	var exp int
	err := tx.QueryRow(ctx, `SELECT exponent FROM ref.currency WHERE code = $1`, currency).Scan(&exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, problem.Unprocessable("procurement.unknown_currency", "The currency is not known.",
			problem.FieldError{Field: field, Code: "unknown", Message: "an ISO 4217 currency in the reference data"})
	}
	return exp, err
}

// amount parses a money value in the document currency.
func amount(value, currency, docCurrency string, exp int, field string, positive bool) (int64, error) {
	if currency != docCurrency {
		return 0, invalid("procurement.currency_mismatch", field+".currency", "amounts are in the document currency "+docCurrency)
	}
	m, err := money.ParseAmount(value, exp)
	if err != nil {
		return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", docCurrency, exp),
			problem.FieldError{Field: field + ".amount", Code: "invalid", Message: err.Error()})
	}
	if m < 0 || (positive && m == 0) {
		return 0, invalid("procurement.invalid_amount", field+".amount", "the amount must be positive")
	}
	return int64(m), nil
}

// quantity parses a positive decimal quantity.
func quantity(value, field string) (*big.Rat, error) {
	q, ok := new(big.Rat).SetString(value)
	if !ok || q.Sign() <= 0 {
		return nil, invalid("procurement.invalid_quantity", field+".value", "a positive quantity")
	}
	return q, nil
}

// rat reads a decimal the database rendered as text.
func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s) // numeric columns render as decimals
	return r
}

// extend is quantity × price / price unit in minor units, rounded half up
// (quantities and prices are never negative).
func extend(qty string, price int64, priceUnit int32) int64 {
	v := new(big.Rat).Mul(rat(qty), new(big.Rat).SetInt64(price))
	v.Quo(v, new(big.Rat).SetInt64(int64(priceUnit)))
	q, m := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
	if m.Mul(m, big.NewInt(2)).Cmp(v.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

func moneyOf(minor int64, currency string, exp int) map[string]string {
	return map[string]string{"amount": money.Minor(minor).Plain(exp), "currency": currency}
}

// legalEntity loads a legal entity the caller may act in that is active.
func (e *Endpoints) legalEntity(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, err
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	switch {
	case err != nil:
		return tenancy.Entity{}, err
	case !ok || !access.Allows(id):
		return tenancy.Entity{}, problem.Unprocessable("request.reference_not_found", "No legal entity with this id.",
			problem.FieldError{Field: "legal_entity_id", Code: "not_found", Message: "no legal entity with this id that you can access"})
	case le.Status != "active":
		return tenancy.Entity{}, invalid("procurement.legal_entity_not_active", "legal_entity_id", "the legal entity is "+le.Status)
	}
	return le, nil
}

// supplier requires a supplier that can be ordered from in the legal entity.
func (e *Endpoints) supplier(ctx context.Context, id, le uuid.UUID) error {
	s, ok, err := e.Suppliers.Supplier(ctx, id, le)
	if err != nil {
		return err
	}
	why := "there is no business partner with this id"
	if ok {
		why = s.Usable()
	}
	if why != "" {
		return invalid("procurement.supplier_not_usable", "supplier_id", why)
	}
	return nil
}

// orgUnit requires an org unit of the kind in the legal entity.
func (e *Endpoints) orgUnit(ctx context.Context, id *uuid.UUID, le uuid.UUID, kind, field string) error {
	if id == nil {
		return nil
	}
	u, ok, err := e.LegalEntities.OrgUnit(ctx, *id)
	if err != nil {
		return err
	}
	if !ok || u.LegalEntityID != le || u.Kind != kind || u.Status != "active" {
		return invalid("procurement.invalid_org_unit", field, "an active "+kind+" of the legal entity")
	}
	return nil
}

// account requires an account the legal entity can post to.
func (e *Endpoints) account(ctx context.Context, le, id uuid.UUID, field string) error {
	ok, err := e.Accounts.IsPostable(ctx, le, id)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("procurement.account_not_postable", field, "an active, unblocked account of the legal entity that is not a reconciliation account")
	}
	return nil
}

// number allocates the next document number of the type for the date's
// fiscal year.
func (e *Endpoints) number(ctx context.Context, le uuid.UUID, documentType string, date time.Time) (string, error) {
	year, err := e.Calendar.FiscalYear(ctx, le, date)
	if err != nil {
		return "", err
	}
	return e.Numbers.Next(ctx, le, documentType, year)
}

// checkItemNo requires a given item number to be the item's position.
func checkItemNo(given *int32, i int, field string) error {
	if given != nil && *given != int32(i+1) {
		return invalid("procurement.invalid_item_no", field+".item_no", fmt.Sprintf("items are numbered 1 to n in order; this is item %d", i+1))
	}
	return nil
}

// removeItems marks the items beyond the first n removed.
func removeItems(ctx context.Context, tx pgx.Tx, table, column string, doc uuid.UUID, n int) error {
	_, err := tx.Exec(ctx, `UPDATE `+table+` SET is_removed = true WHERE `+column+` = $1 AND item_no > $2 AND NOT is_removed`, doc, n)
	return err
}
