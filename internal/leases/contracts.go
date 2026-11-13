package leases

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Contract is a row of leases.contract. PaymentAmount is minor units of
// Currency, the company's local currency, whose minor-unit digits are
// Exponent; AnnualRate is the annual discount rate in percent as the
// database prints it, without trailing zeros. PaymentCount and Total are
// the number and sum of its payments; Payments are the payments by
// number, nil where a list read them without.
type Contract struct {
	ID               uuid.UUID
	DocumentNumber   string
	LegalEntityID    uuid.UUID
	LessorID         uuid.UUID
	CommencementDate time.Time
	TermMonths       int32
	PaymentAmount    money.Minor
	Currency         string
	Exponent         int
	Frequency        string
	Timing           string
	AnnualRate       string
	CostCenterID     uuid.UUID
	Status           string
	PaymentCount     int32
	Total            money.Minor
	Payments         []Payment
	Version          int32
	CreatedAt        time.Time
	CreatedBy        uuid.UUID
	UpdatedAt        time.Time
	UpdatedBy        uuid.UUID
}

// Terms are the terms the contract's schedule follows from.
func (c Contract) Terms() Terms {
	return Terms{Commencement: c.CommencementDate, TermMonths: int(c.TermMonths), Interval: frequencies[c.Frequency], Timing: c.Timing}
}

// contractColumns read a contract with the number and sum of its
// payments; a select adds its payments or none.
const contractColumns = `k.id, k.document_number, k.legal_entity_id, k.lessor_id, k.commencement_date, k.term_months, k.payment_amount,
		k.currency, c.exponent, k.payment_frequency, k.payment_timing, trim_scale(k.annual_rate)::text, k.cost_center_id, k.status,
		(SELECT count(*) FROM leases.payment p WHERE p.tenant_id = k.tenant_id AND p.contract_id = k.id)::int,
		(SELECT coalesce(sum(p.amount), 0) FROM leases.payment p WHERE p.tenant_id = k.tenant_id AND p.contract_id = k.id)::bigint,
		k.version, k.created_at, k.created_by, k.updated_at, k.updated_by`

// contractSelect reads contracts with their payments by number. The
// payments come from a subquery, not a join, and a due date is written as
// an instant at midnight UTC, which is what a date is everywhere in the
// context.
const contractSelect = `SELECT ` + contractColumns + `,
		coalesce((SELECT json_agg(json_build_object('payment_no', p.payment_no, 'due_date', to_char(p.due_date, 'YYYY-MM-DD"T00:00:00Z"'),
				'amount', p.amount) ORDER BY p.payment_no)
			FROM leases.payment p WHERE p.tenant_id = k.tenant_id AND p.contract_id = k.id), '[]')
	FROM leases.contract k JOIN ref.currency c ON c.code = k.currency`

// listSelect reads contracts without their payments: a page of them would
// otherwise carry up to 600 payments each.
const listSelect = `SELECT ` + contractColumns + `, NULL::json FROM leases.contract k JOIN ref.currency c ON c.code = k.currency`

func scanContract(row pgx.Row) (Contract, error) {
	var c Contract
	err := row.Scan(&c.ID, &c.DocumentNumber, &c.LegalEntityID, &c.LessorID, &c.CommencementDate, &c.TermMonths, &c.PaymentAmount, &c.Currency,
		&c.Exponent, &c.Frequency, &c.Timing, &c.AnnualRate, &c.CostCenterID, &c.Status, &c.PaymentCount, &c.Total, &c.Version, &c.CreatedAt,
		&c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy, &c.Payments)
	return c, errs.Wrapf(err, "scan lease contract")
}

// load reads a contract whatever its company; GetContract applies company
// access.
func load(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Contract, error) {
	c, err := scanContract(tx.QueryRow(ctx, contractSelect+` WHERE k.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Contract{}, problem.NotFoundOf("lease contract")
	}
	if err != nil {
		return Contract{}, fmt.Errorf("leases: read contract %s: %w", id, err)
	}
	return c, nil
}

// GetContract returns a lease contract of a legal entity the caller may
// act in; another's is not found, so ids cannot be probed (04 §4.9).
func GetContract(ctx context.Context, id uuid.UUID) (Contract, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Contract{}, fmt.Errorf("leases: company access for contract %s: %w", id, err)
	}
	c, err := load(ctx, tx, id)
	if err == nil && !access.Allows(c.LegalEntityID) {
		return Contract{}, problem.NotFoundOf("lease contract")
	}
	return c, err
}

// Amount is an amount as the request gives it: a decimal string and its
// currency.
type Amount struct{ Value, Currency string }

// Input is a lease contract as a person enters it (ADR-0073 §3).
type Input struct {
	LegalEntityID, LessorID, CostCenterID uuid.UUID
	Commencement                          time.Time
	TermMonths                            int32
	Payment                               Amount
	Frequency, Timing                     string
	// Rate is the annual discount rate in percent, a decimal string.
	Rate string
}

// maxRateDecimals are the decimals of a discount rate the database keeps
// (numeric(9, 6)).
const maxRateDecimals = 6

// terms checks what the request says of the schedule, before anything is
// read: a known frequency that divides the term into whole intervals, a
// known timing and a discount rate that is a plain decimal above zero and
// at most 100, with up to six decimals. Each refusal is 422 naming the
// request's field. The term's range, 1 to 600 months, is the contract's.
func terms(in Input) (Terms, error) {
	months, ok := frequencies[in.Frequency]
	if !ok {
		return Terms{}, problem.Invalid("leases.unknown_frequency", "payment_frequency",
			fmt.Sprintf("%q is not a payment frequency; the frequencies are monthly, quarterly and yearly.", in.Frequency))
	}
	if in.Timing != Advance && in.Timing != Arrears {
		return Terms{}, problem.Invalid("leases.unknown_timing", "payment_timing",
			fmt.Sprintf("%q is not a payment timing; payments are due in advance or in arrears.", in.Timing))
	}
	if int(in.TermMonths)%months != 0 {
		return Terms{}, problem.Invalid("leases.frequency_not_dividing_term", "term_months",
			fmt.Sprintf("A term of %d months is not a whole number of %s intervals of %d months.", in.TermMonths, in.Frequency, months))
	}
	r, err := money.ParsePlain(in.Rate)
	_, frac, _ := strings.Cut(in.Rate, ".")
	if err != nil || len(frac) > maxRateDecimals || r.Sign() <= 0 || r.Cmp(big.NewRat(100, 1)) > 0 {
		return Terms{}, problem.Invalid("leases.invalid_discount_rate", "annual_discount_rate_percent",
			fmt.Sprintf("%q is not a discount rate: a percentage above zero and at most 100, with up to %d decimals, such as \"6\" or \"4.25\".",
				in.Rate, maxRateDecimals))
	}
	return Terms{Commencement: in.Commencement, TermMonths: int(in.TermMonths), Interval: months, Timing: in.Timing}, nil
}

// payment reads the payment amount: in the company's local currency (422
// leases.foreign_currency, a lease in a foreign currency being left to a
// later version), with the currency's decimals (400
// request.invalid_amount) and more than zero (422
// leases.payment_not_positive); with the payments' sum within the range of
// an amount (422 leases.amount_too_large).
func payment(ctx context.Context, tx pgx.Tx, le tenancy.Entity, in Amount, count int) (money.Minor, error) {
	if in.Currency != le.LocalCurrency {
		return 0, problem.Invalid("leases.foreign_currency", "payment_amount.currency", "The payments are in "+in.Currency+" and company "+le.Code+
			"'s local currency is "+le.LocalCurrency+"; leases in a foreign currency are not supported yet.")
	}
	exp, err := currency.Exponent(ctx, tx, in.Currency, nil) // the company's local currency is a known one
	if err != nil {
		return 0, err
	}
	amount, err := currency.ParseAmount(in.Value, in.Currency, exp, "payment_amount.amount")
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, problem.Invalid("leases.payment_not_positive", "payment_amount.amount", "A lease payment is more than zero.")
	}
	all := make([]money.Minor, count)
	for i := range all {
		all[i] = amount
	}
	if _, err := money.Sum(all...); err != nil {
		return 0, problem.Invalid("leases.amount_too_large", "payment_amount.amount",
			fmt.Sprintf("The %d payments add up to more than an amount can hold.", count))
	}
	return amount, nil
}

// lessor requires a business partner that is active, a supplier and set
// up and not blocked for posting in the company, as procurement and
// payables require of a supplier (422 leases.lessor_not_usable).
func (e *Endpoints) lessor(ctx context.Context, id, le uuid.UUID) error {
	s, ok, err := e.Lessors.Supplier(ctx, id, le)
	if err != nil {
		return fmt.Errorf("leases: lessor %s: %w", id, err)
	}
	why := "there is no business partner with this id"
	if ok {
		why = s.Usable()
	}
	if why != "" {
		return problem.Invalid("leases.lessor_not_usable", "lessor_id", "The lessor cannot be used: "+why+".")
	}
	return nil
}

// costCenter requires an active cost center of the company in force on
// the commencement date (422 leases.cost_center_invalid).
func (e *Endpoints) costCenter(ctx context.Context, id, le uuid.UUID, day time.Time) error {
	refs, err := e.CostCenters.CostCenterRefs(ctx, []uuid.UUID{id})
	if err != nil {
		return fmt.Errorf("leases: cost center %s: %w", id, err)
	}
	valid := false
	if ref, ok := refs[id]; ok && ref.LegalEntityID == le && ref.Active {
		if valid, err = e.CostCenters.CostCenterValidOn(ctx, id, day); err != nil {
			return fmt.Errorf("leases: cost center %s on %s: %w", id, day.Format(time.DateOnly), err)
		}
	}
	if !valid {
		return problem.Invalid("leases.cost_center_invalid", "cost_center_id",
			"No active cost center of the company with this id is in force on the commencement date, "+day.Format(time.DateOnly)+".")
	}
	return nil
}

// company is the company a contract is entered in, one the caller may act
// in (422 request.reference_not_found naming legal_entity_id).
func (e *Endpoints) company(ctx context.Context, id uuid.UUID) (tenancy.Entity, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("leases: company access for legal entity %s: %w", id, err)
	}
	le, ok, err := e.LegalEntities.Entity(ctx, id)
	if err != nil {
		return tenancy.Entity{}, fmt.Errorf("leases: legal entity %s: %w", id, err)
	}
	if !ok || !access.Allows(id) {
		return tenancy.Entity{}, problem.ReferenceNotFound("legal_entity_id", "legal entity", "no legal entity with this id that you can access")
	}
	return le, nil
}

// number draws the contract's number from the company's lease_contract
// range of the fiscal year of the company's date today, in its time zone.
func (e *Endpoints) number(ctx context.Context, le tenancy.Entity) (string, error) {
	day, err := config.DocumentDay(ctx, e.Settings, le.Code, clock.Now(e.Now))
	if err != nil {
		return "", fmt.Errorf("leases: today in company %s: %w", le.Code, err)
	}
	year, err := e.Calendar.FiscalYear(ctx, le.ID, day.Date)
	if err != nil {
		return "", fmt.Errorf("leases: fiscal year of %s: %w", day.Date.Format(time.DateOnly), err)
	}
	number, err := e.Numbers.Next(ctx, le.ID, ContractRange, year)
	if err != nil {
		return "", fmt.Errorf("leases: number a lease contract: %w", err)
	}
	return number, nil
}

// Create enters a lease contract as lessee (ADR-0073 §3-§4): the request's
// terms checked, then its company, payment, lessor and cost center; the
// contract is a draft numbered from the company's lease_contract range,
// with one payment per interval written with it. The audit record and
// LeaseContractChanged are written in the request's transaction. Nothing
// is posted.
func (e *Endpoints) Create(ctx context.Context, in Input) (Contract, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Contract{}, err
	}
	t, err := terms(in)
	if err != nil {
		return Contract{}, err
	}
	le, err := e.company(ctx, in.LegalEntityID)
	if err != nil {
		return Contract{}, err
	}
	amount, err := payment(ctx, tx, le, in.Payment, t.intervals())
	if err != nil {
		return Contract{}, err
	}
	if err := e.lessor(ctx, in.LessorID, le.ID); err != nil {
		return Contract{}, err
	}
	if err := e.costCenter(ctx, in.CostCenterID, le.ID, in.Commencement); err != nil {
		return Contract{}, err
	}
	number, err := e.number(ctx, le)
	if err != nil {
		return Contract{}, err
	}
	names, err := e.Partners.Names(ctx, []uuid.UUID{in.LessorID})
	if err != nil {
		return Contract{}, fmt.Errorf("leases: lessor %s: %w", in.LessorID, err)
	}
	c, err := insert(ctx, tx, p, in, le, number, amount, t.Payments(amount))
	if err != nil {
		return Contract{}, err
	}
	return c, record(ctx, "leases.contract.create", c, le, names[in.LessorID].Number, nil)
}

// insert writes the draft contract and its payments, and reads it back.
func insert(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in Input, le tenancy.Entity, number string, amount money.Minor,
	payments []Payment) (Contract, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO leases.contract (tenant_id, id, document_number, legal_entity_id, lessor_id, commencement_date, term_months,
			payment_amount, currency, payment_frequency, payment_timing, annual_rate, cost_center_id, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::numeric, $13, $14, $15, $15)`,
		p.TenantID, id, number, le.ID, in.LessorID, in.Commencement, in.TermMonths, amount, le.LocalCurrency, in.Frequency, in.Timing, in.Rate,
		in.CostCenterID, Draft, p.ID); err != nil {
		return Contract{}, fmt.Errorf("leases: insert contract %s: %w", number, err)
	}
	numbers, dates, amounts := make([]int32, len(payments)), make([]time.Time, len(payments)), make([]int64, len(payments))
	for i, pay := range payments {
		numbers[i], dates[i], amounts[i] = pay.No, pay.Due, int64(pay.Amount)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO leases.payment (tenant_id, contract_id, payment_no, due_date, amount)
		SELECT $1, $2, x.no, x.due, x.amount FROM unnest($3::int[], $4::date[], $5::bigint[]) AS x (no, due, amount)`,
		p.TenantID, id, numbers, dates, amounts); err != nil {
		return Contract{}, fmt.Errorf("leases: insert the payments of contract %s: %w", number, err)
	}
	return load(ctx, tx, id)
}
