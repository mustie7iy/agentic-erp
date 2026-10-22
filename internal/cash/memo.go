package cash

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Memo records (ADR-0051 §7, T53d) are the expected receipts and payments
// people enter, upload or accept from an agent's proposal, until
// receivables exist (decision P6): one-off or a series expanded at read
// time, counting in the cash position on their house bank while active and
// not expired. They post nothing, so they need no approval; the product
// owner may revisit that for large amounts (ADR-0051, facts).

// Statuses, origins and certainties of memo records.
const (
	MemoProposed  = "proposed"
	MemoActive    = "active"
	MemoExpired   = "expired" // active, past its last expiry at the read: shown, never stored
	MemoRealised  = "realised"
	MemoCancelled = "cancelled"
	MemoRejected  = "rejected"

	OriginManual = "manual"
	OriginUpload = "upload"
	OriginAgent  = "agent"

	SourceMemo = "memo"
)

// certainties are a memo record's: never actual, which is the bank's.
var certainties = []string{CertaintyConfirmed, "expected", "planned"}

// defaultExpiryDays is how long after its value date a record counts when
// no expiry date is given (SAP's memo record expiry).
const defaultExpiryDays = 7

// HouseBank is an active house bank of a legal entity.
type HouseBank struct {
	ID             uuid.UUID
	Code, Currency string
}

// StatementLine is a bank statement line, as a memo record's realisation
// or an agent's evidence reads it.
type StatementLine struct {
	LegalEntityID, HouseBankID uuid.UUID
	Currency                   string
	Amount                     money.Minor
	ValueDate                  time.Time
}

// LineRef names a bank statement line.
type LineRef struct {
	StatementID uuid.UUID `json:"statement_id"`
	LineNo      int32     `json:"line_no"`
}

// Sessions is what the cash context asks of the agents context, through
// the adapter in internal/app: that a session is the calling agent's and
// running, with the agents context's problem when it is not.
type Sessions interface {
	Running(ctx context.Context, id uuid.UUID) error
}

// MemoRecord is a stored memo record. CountsUntil is the last day its last
// occurrence counts, nil for a series without end.
type MemoRecord struct {
	ID, LegalEntityID      uuid.UUID
	HouseBankID            *uuid.UUID
	Currency               string
	Exponent               int
	Amount                 money.Minor
	ValueDate, ExpiresOn   time.Time
	CountsUntil            *time.Time
	Certainty, Item        string
	Description, Reference *string
	Recurrence             *Recurrence
	RecurrenceOf           *uuid.UUID
	OccurrenceDate         *time.Time
	Status, Origin         string
	SessionID              *uuid.UUID
	Reason                 *string
	Evidence               []LineRef
	BatchKey               *string
	RealisedBy             *LineRef
	DecidedBy              *uuid.UUID
	DecidedAt              *time.Time
	Comment                *string
	Version                int32
	CreatedAt, UpdatedAt   time.Time
	CreatedBy, UpdatedBy   uuid.UUID
}

// StatusOn is the record's status as of the day: an active record whose
// last occurrence expired before it is expired.
func (m MemoRecord) StatusOn(day time.Time) string {
	if m.Status == MemoActive && m.CountsUntil != nil && m.CountsUntil.Before(day) {
		return MemoExpired
	}
	return m.Status
}

// expiryDays is how many days after its date an occurrence still counts.
func (m MemoRecord) expiryDays() int { return int(m.ExpiresOn.Sub(m.ValueDate).Hours() / 24) }

// MemoInput is a memo record to write: Amount is a decimal of Currency's
// minor-unit digits, signed (a receipt positive); a nil ExpiresOn is the
// value date plus seven days, an empty Certainty expected.
type MemoInput struct {
	LegalEntityID          uuid.UUID
	HouseBankID            *uuid.UUID
	Amount, Currency       string
	ValueDate              time.Time
	ExpiresOn              *time.Time
	Certainty, Item        string
	Description, Reference *string
	Recurrence             *Recurrence
	RecurrenceOf           *uuid.UUID
	OccurrenceDate         *time.Time
}

// memoValues are a memo record's checked values, as stored.
type memoValues struct {
	houseBank              *uuid.UUID
	currency               string
	amount                 money.Minor
	valueDate, expiresOn   time.Time
	countsUntil            *time.Time
	certainty, item        string
	description, reference *string
	recurrence             *Recurrence
	recurrenceOf           *uuid.UUID
	occurrenceDate         *time.Time
}

// checker checks memo records of one legal entity against its active
// house banks, the tenant's active liquidity items and the currencies.
type checker struct {
	tx          pgx.Tx
	legalEntity uuid.UUID
	banks       map[uuid.UUID]HouseBank
	items       map[string]string // code: direction
	exponents   map[string]int
}

// checker reads what memo records of the legal entity are checked
// against; a legal entity the caller cannot act in is not found.
func (e *Endpoints) checker(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID) (*checker, error) {
	if _, err := e.company(ctx, legalEntity); err != nil {
		return nil, err
	}
	banks, err := e.Payments.HouseBanks(ctx, legalEntity)
	if err != nil {
		return nil, fmt.Errorf("cash: house banks of %s: %w", legalEntity, err)
	}
	c := &checker{tx: tx, legalEntity: legalEntity, banks: map[uuid.UUID]HouseBank{}, items: map[string]string{}, exponents: map[string]int{}}
	for _, b := range banks {
		c.banks[b.ID] = b
	}
	rows, _ := tx.Query(ctx, `SELECT code, direction FROM cash.liquidity_item WHERE is_active`)
	var code, direction string
	if _, err := pgx.ForEachRow(rows, []any{&code, &direction}, func() error { c.items[code] = direction; return nil }); err != nil {
		return nil, fmt.Errorf("cash: liquidity items: %w", err)
	}
	return c, nil
}

// invalid is a 422 on one field.
func invalid(code, detail, field, message string) *problem.Problem {
	return problem.Unprocessable(code, detail, problem.FieldError{Field: field, Code: "invalid", Message: message})
}

// values checks a memo record and gives its stored values.
func (c *checker) values(ctx context.Context, in MemoInput) (memoValues, error) {
	v := memoValues{houseBank: in.HouseBankID, currency: in.Currency, valueDate: in.ValueDate, certainty: in.Certainty, item: in.Item,
		description: in.Description, reference: in.Reference, recurrence: in.Recurrence, recurrenceOf: in.RecurrenceOf, occurrenceDate: in.OccurrenceDate}
	var err error
	if v.amount, err = c.amount(ctx, in.Amount, in.Currency); err != nil {
		return memoValues{}, err
	}
	if err := c.bank(v); err != nil {
		return memoValues{}, err
	}
	if err := c.item(v); err != nil {
		return memoValues{}, err
	}
	if v.certainty == "" {
		v.certainty = "expected"
	}
	if !slices.Contains(certainties, v.certainty) {
		return memoValues{}, invalid("request.validation_failed", "A memo record is confirmed, expected or planned.", "certainty", "confirmed, expected or planned")
	}
	if err := v.dates(in.ExpiresOn); err != nil {
		return memoValues{}, err
	}
	return v, nil
}

// amount parses a signed, non-zero amount in a known currency.
func (c *checker) amount(ctx context.Context, amount, code string) (money.Minor, error) {
	exp, ok := c.exponents[code]
	if !ok {
		var err error
		if exp, err = currency.Exponent(ctx, c.tx, code, invalid("cash.unknown_currency", "The currency is not known.", "amount.currency",
			"an ISO 4217 currency in the reference data")); err != nil {
			return 0, err
		}
		c.exponents[code] = exp
	}
	m, err := money.ParseAmount(amount, exp)
	if err != nil {
		return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", code, exp),
			problem.FieldError{Field: "amount.amount", Code: "invalid", Message: err.Error()})
	}
	if m == 0 {
		return 0, invalid("cash.invalid_amount", "A memo record expects a receipt (a positive amount) or a payment (a negative one), not zero.", "amount.amount", "not zero")
	}
	return m, nil
}

// bank checks the house bank, if any: an active one of the legal entity in
// the record's currency.
func (c *checker) bank(v memoValues) error {
	if v.houseBank == nil {
		return nil
	}
	b, ok := c.banks[*v.houseBank]
	if !ok || b.Currency != v.currency {
		return invalid("cash.house_bank_invalid", "The house bank is not an active house bank of the legal entity in the record's currency.", "house_bank_id",
			"an active house bank of the legal entity in "+v.currency)
	}
	return nil
}

// item checks the liquidity item: an active one whose direction fits the
// amount's sign (the database checks it too, cash.check_memo_item).
func (c *checker) item(v memoValues) error {
	direction, ok := c.items[v.item]
	switch {
	case !ok:
		return invalid("cash.liquidity_item_invalid", "The liquidity item is not one of the configuration's active items.", "liquidity_item", "an active liquidity item")
	case direction == "inflow" && v.amount < 0, direction == "outflow" && v.amount > 0:
		return invalid("cash.liquidity_item_invalid", "The liquidity item is for "+direction+"s; the amount's sign says otherwise.", "liquidity_item",
			"an item for "+direction+"s takes a "+map[string]string{"inflow": "positive", "outflow": "negative"}[direction]+" amount")
	}
	return nil
}

// dates sets the expiry (default the value date plus seven days) and the
// last day the record counts, checking a series' rule.
func (v *memoValues) dates(expires *time.Time) error {
	v.expiresOn = v.valueDate.AddDate(0, 0, defaultExpiryDays)
	if expires != nil {
		v.expiresOn = *expires
	}
	if v.expiresOn.Before(v.valueDate) {
		return invalid("request.validation_failed", "A memo record expires on or after its value date.", "expires_on", "on or after the value date")
	}
	r := v.recurrence
	if r == nil {
		v.countsUntil = &v.expiresOn
		return nil
	}
	switch {
	case !slices.Contains([]string{Weekly, Monthly, Quarterly}, r.Frequency):
		return invalid("request.validation_failed", "A series is weekly, monthly or quarterly.", "recurrence.frequency", "weekly, monthly or quarterly")
	case r.Until != nil && r.Count != nil:
		return invalid("request.validation_failed", "A series ends on a day or after a count, not both.", "recurrence.until", "until or count")
	case r.Until != nil && r.Until.Before(v.valueDate):
		return invalid("request.validation_failed", "A series ends on or after its value date.", "recurrence.until", "on or after the value date")
	case v.recurrenceOf != nil:
		return invalid("request.validation_failed", "An occurrence's override is a single record, not a series.", "recurrence", "none on an override")
	}
	v.countsUntil = nil
	if last, ok := r.last(v.valueDate); ok {
		until := last.Add(v.expiresOn.Sub(v.valueDate))
		v.countsUntil = &until
	}
	return nil
}
