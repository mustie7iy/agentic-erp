package procurement

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
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
	Expired         = "expired"
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

func invalid(code, field, message string) error {
	return problem.Unprocessable(code, message, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// referencedContract reads the contract a requisition or order names. An
// unknown one is procurement.invalid_contract on contract_id, the code of
// every other reason the contract cannot be used, rather than the generic
// problem.Reference; other errors pass.
func referencedContract(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Contract, error) {
	c, err := getContract(ctx, tx, id, false)
	if err != nil && problem.From(err).Status == http.StatusNotFound {
		return Contract{}, invalid("procurement.invalid_contract", "contract_id", "no contract with this id that you can access")
	}
	return c, err
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

func event(name, aggregate string, id uuid.UUID, payload map[string]any) outbox.Event {
	return outbox.Event{Context: "procurement", Name: name, AggregateType: "procurement." + aggregate, AggregateID: id, Payload: payload}
}

// exponent returns the currency's minor-unit digits; an unknown currency is
// 422 on field.
func exponent(ctx context.Context, tx pgx.Tx, code, field string) (int, error) {
	exp, err := currency.Exponent(ctx, tx, code, problem.Unprocessable("procurement.unknown_currency", "The currency is not known.",
		problem.FieldError{Field: field, Code: "unknown", Message: "an ISO 4217 currency in the reference data"}))
	if err != nil {
		return 0, fmt.Errorf("procurement: %w", err)
	}
	return exp, nil
}

// amount parses a money value in the document currency.
func amount(value, currency, docCurrency string, exp int, field string, positive bool) (money.Minor, error) {
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
	return m, nil
}

// quantity checks a document quantity (money.PositiveQuantity): API input
// is bounded by the contract already, migrated documents are not, and
// itemsTotal's Scale reads only the plain form.
func quantity(value, field string) error {
	if money.PositiveQuantity(value) != nil {
		return invalid("procurement.invalid_quantity", field+".value", fmt.Sprintf("a positive plain decimal quantity of at most %d integer digits and %d decimals", money.QuantityIntDigits, money.QuantityDecimals))
	}
	return nil
}

// itemsTotal is the sum of the items' extended amounts, quantity × price /
// price unit rounded half away from zero, checked for range.
func itemsTotal(items []Item) (money.Minor, error) {
	amounts := make([]money.Minor, len(items))
	for i, it := range items {
		a, err := money.Scale(it.UnitPrice, it.Quantity, int64(it.PriceUnit))
		if err != nil {
			return 0, err
		}
		amounts[i] = a
	}
	return money.Sum(amounts...)
}

// checkTotal refuses items whose extended amounts or total are out of
// range: a wrapped total would select the wrong approval tier and pass a
// contract's remaining value. quantity admits only plain decimal
// quantities, from the API and from a migration alike, so what fails is
// the range.
func checkTotal(items []Item) error {
	_, err := documentTotal(items)
	return err
}

// documentTotal is the items' total for a decision (approval tier, contract
// consumption). Unlike Total it fails closed: items stored before
// checkTotal refused totals out of range are refused here.
func documentTotal(items []Item) (money.Minor, error) {
	sum, err := itemsTotal(items)
	if err != nil {
		return 0, invalid("procurement.amount_too_large", "items", "extended amounts and their total must be in the range of an amount")
	}
	return sum, nil
}

// legalEntity loads a legal entity the caller may act in that is active.
func (e *Endpoints) legalEntity(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, err
	}
	le, err := tenancy.Accessible(ctx, e.LegalEntities, access, id, "legal_entity_id")
	if err != nil {
		return tenancy.Entity{}, err
	}
	if le.Status != "active" {
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

// removeItems marks the items beyond the first n removed; table is an item
// table of the procurement schema and column its document's id.
func removeItems(ctx context.Context, tx pgx.Tx, table, column string, doc uuid.UUID, n int) error {
	_, err := tx.Exec(ctx, `UPDATE `+pgx.Identifier{"procurement", table}.Sanitize()+` SET is_removed = true WHERE `+pgx.Identifier{column}.Sanitize()+
		` = $1 AND item_no > $2 AND NOT is_removed`, doc, n)
	if err != nil {
		return fmt.Errorf("procurement: remove items of procurement.%s %s: %w", table, doc, err)
	}
	return nil
}
