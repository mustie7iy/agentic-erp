package assets

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Patch is a merge patch of an asset: nil fields are unchanged; a
// DepreciationStart pointing to nil clears it in every ledger.
type Patch struct {
	Description       *string
	DepreciationStart **time.Time
	Terms             []TermsPatch
}

// TermsPatch changes an asset's terms in one ledger, or gives it terms in
// a ledger it has none in (Method and UsefulLifeMonths then required).
// Residual is a decimal of the currency's minor-unit digits.
type TermsPatch struct {
	LedgerID                   uuid.UUID
	Method                     *string
	UsefulLifeMonths           *int32
	DecliningFactor            *string
	Residual, ResidualCurrency *string
	DepreciationStart          **time.Time
}

// changeAsset changes an asset that is not cancelled (422
// assets.asset_cancelled): its description, depreciation start and terms
// per ledger (ADR-0057 §7), refusing a change of terms depreciation has
// been posted with (409 assets.terms_in_use). A patch that changes
// nothing writes and publishes nothing.
func (e *Endpoints) changeAsset(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch Patch) (Asset, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Asset{}, err
	}
	a, err := readAsset(ctx, tx, id, true)
	if err != nil {
		return Asset{}, err
	}
	if err := ifMatch(a.Version); err != nil {
		return Asset{}, err
	}
	if a.Status == StatusCancelled {
		return Asset{}, problem.Unprocessable("assets.asset_cancelled", fmt.Sprintf("Asset %s is cancelled; it does not change any more.", a.Number))
	}
	terms, err := e.patchedTerms(ctx, tx, a, ch)
	if err != nil {
		return Asset{}, err
	}
	changed, err := changedTerms(a.Terms, terms)
	if err != nil {
		return Asset{}, err
	}
	description := a.Description
	if ch.Description != nil {
		if err := checkDescription(*ch.Description); err != nil {
			return Asset{}, err
		}
		description = *ch.Description
	}
	if description == a.Description && len(changed) == 0 {
		return a, nil
	}
	if err := touch(ctx, tx, p, id, description); err != nil {
		return Asset{}, err
	}
	if err := writeTerms(ctx, tx, p, id, changed); err != nil {
		return Asset{}, err
	}
	return written(ctx, tx, id, "change", "changed")
}

// termsFrame is what a terms entry is checked against: the ledgers the
// company keeps and its local currency with its minor-unit digits.
type termsFrame struct {
	kept     map[uuid.UUID]bool
	currency string
	exponent int
}

// frame reads the asset's company's kept ledgers and currency.
func (e *Endpoints) frame(ctx context.Context, tx pgx.Tx, le uuid.UUID) (termsFrame, error) {
	co, err := e.company(ctx, le)
	if err != nil {
		return termsFrame{}, err
	}
	exp, err := currency.Exponent(ctx, tx, co.Currency, nil)
	if err != nil {
		return termsFrame{}, fmt.Errorf("assets: currency %s: %w", co.Currency, err)
	}
	ledgers, err := e.Ledger.Ledgers(ctx, &le)
	if err != nil {
		return termsFrame{}, fmt.Errorf("assets: ledgers of %s: %w", le, err)
	}
	f := termsFrame{kept: map[uuid.UUID]bool{}, currency: co.Currency, exponent: exp}
	for _, l := range ledgers {
		f.kept[l.ID] = true
	}
	return f, nil
}

// patchedTerms are the asset's terms with the patch applied, a ledger new
// to the asset last: the depreciation start for every ledger first, then
// each entry of Terms, checked.
func (e *Endpoints) patchedTerms(ctx context.Context, tx pgx.Tx, a Asset, ch Patch) ([]Terms, error) {
	out := slices.Clone(a.Terms)
	if ch.DepreciationStart != nil {
		for i := range out {
			out[i].DepreciationStart = *ch.DepreciationStart
		}
	}
	if len(ch.Terms) == 0 {
		return out, nil
	}
	f, err := e.frame(ctx, tx, a.LegalEntityID)
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	for i, tp := range ch.Terms {
		field := fmt.Sprintf("terms[%d]", i)
		if seen[tp.LedgerID] {
			return nil, validation(field+".ledger_id", "each ledger once")
		}
		seen[tp.LedgerID] = true
		j := slices.IndexFunc(out, func(t Terms) bool { return t.LedgerID == tp.LedgerID })
		t, err := startTerms(field, j, out, tp, f, ch.DepreciationStart)
		if err != nil {
			return nil, err
		}
		if t, err = applyTerms(field, t, tp, f); err != nil {
			return nil, err
		}
		if j < 0 {
			out = append(out, t)
		} else {
			out[j] = t
		}
	}
	return out, nil
}

// startTerms is the terms an entry changes: the asset's in its ledger
// (index j), or new terms in a ledger the company keeps (422
// ledger.ledger_not_kept otherwise) that the entry gives a method and
// useful life, starting at the patch's depreciation start.
func startTerms(field string, j int, terms []Terms, tp TermsPatch, f termsFrame, start **time.Time) (Terms, error) {
	if j >= 0 {
		return terms[j], nil
	}
	if !f.kept[tp.LedgerID] {
		return Terms{}, problem.Unprocessable("ledger.ledger_not_kept", "The company does not keep this ledger.",
			problem.FieldError{Field: field + ".ledger_id", Code: "not_kept", Message: "a ledger the company keeps"})
	}
	if tp.Method == nil || tp.UsefulLifeMonths == nil {
		return Terms{}, validation(field+".method", "the method and useful life of terms in a ledger the asset has none in")
	}
	t := Terms{LedgerID: tp.LedgerID}
	if start != nil {
		t.DepreciationStart = *start
	}
	return t, nil
}

// applyTerms applies an entry to terms and checks the result: a method of
// ADR-0057 §7 with a factor above 1 and at most 10 for declining balance
// alone (straight-line drops one), a useful life of 1-1200 months (the
// contract's bounds) and a residual value of zero or more in the company's
// currency.
func applyTerms(field string, t Terms, tp TermsPatch, f termsFrame) (Terms, error) {
	if tp.Method != nil {
		t.Method = *tp.Method
	}
	if tp.UsefulLifeMonths != nil {
		t.UsefulLifeMonths = *tp.UsefulLifeMonths
	}
	if tp.DecliningFactor != nil {
		t.DecliningFactor = tp.DecliningFactor
	}
	if tp.DepreciationStart != nil {
		t.DepreciationStart = *tp.DepreciationStart
	}
	switch t.Method {
	case config.StraightLine:
		if tp.DecliningFactor != nil {
			return Terms{}, validation(field+".declining_factor", "only with declining_balance")
		}
		t.DecliningFactor = nil
	case config.DecliningBalance:
		factor, err := decliningFactor(field, t.DecliningFactor)
		if err != nil {
			return Terms{}, err
		}
		t.DecliningFactor = &factor
	default:
		return Terms{}, validation(field+".method", "straight_line or declining_balance")
	}
	if tp.Residual == nil {
		return t, nil
	}
	residual, err := residualValue(field, *tp.Residual, *tp.ResidualCurrency, f)
	t.Residual = residual
	return t, err
}

// decliningFactor checks a declining balance factor, required, and gives
// it without trailing zeros, as the database reads it back.
func decliningFactor(field string, factor *string) (string, error) {
	const decimals, one, most = 4, 10_000, 100_000 // in ten-thousandths: above 1, at most 10
	if factor == nil {
		return "", validation(field+".declining_factor", "a factor with declining_balance")
	}
	f, err := money.ParseDecimal(*factor, decimals)
	if err != nil || f <= one || f > most {
		return "", validation(field+".declining_factor", "a plain decimal above 1 and at most 10, with up to four decimals")
	}
	whole, frac := int64(f)/one, fmt.Sprintf("%04d", int64(f)%one) // a factor, not money: ten-thousandths shown as a plain decimal
	if frac = strings.TrimRight(frac, "0"); frac == "" {
		return strconv.FormatInt(whole, 10), nil
	}
	return strconv.FormatInt(whole, 10) + "." + frac, nil
}

// residualValue parses a residual value, zero or more in the company's
// currency.
func residualValue(field, amount, code string, f termsFrame) (money.Minor, error) {
	if code != f.currency {
		return 0, validation(field+".residual_value.currency", "the company's currency, "+f.currency)
	}
	m, err := money.ParseAmount(amount, f.exponent)
	if err != nil {
		return 0, problem.BadRequest("request.invalid_amount", fmt.Sprintf("Amounts in %s have exactly %d decimals.", code, f.exponent),
			problem.FieldError{Field: field + ".residual_value.amount", Code: "invalid", Message: err.Error()})
	}
	if m < 0 {
		return 0, validation(field+".residual_value.amount", "zero or more")
	}
	return m, nil
}

// changedTerms are the terms that differ from the asset's, refusing a
// change in a ledger depreciation has been posted in (409
// assets.terms_in_use; the database holds it too, asset_terms_in_use).
func changedTerms(old, terms []Terms) ([]Terms, error) {
	var out []Terms
	for _, t := range terms {
		i := slices.IndexFunc(old, func(o Terms) bool { return o.LedgerID == t.LedgerID })
		if i >= 0 && sameTerms(old[i], t) {
			continue
		}
		if i >= 0 && old[i].booked() {
			return nil, termsInUse()
		}
		out = append(out, t)
	}
	return out, nil
}

// sameTerms reports whether two terms of a ledger are the same.
func sameTerms(a, b Terms) bool {
	return a.Method == b.Method && a.UsefulLifeMonths == b.UsefulLifeMonths && equalPtr(a.DecliningFactor, b.DecliningFactor) &&
		a.Residual == b.Residual && (a.DepreciationStart == nil) == (b.DepreciationStart == nil) &&
		(a.DepreciationStart == nil || a.DepreciationStart.Equal(*b.DepreciationStart))
}

// equalPtr reports whether two optional values are both absent or equal.
func equalPtr[T comparable](a, b *T) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// validation is a 422 request.validation_failed on one field.
func validation(field, message string) *problem.Problem {
	return invalid("request.validation_failed", "The request is not valid: "+field+": "+message+".", field, message)
}

// checkDescription refuses a description of spaces alone, which the
// database refuses too (asset_description_check).
func checkDescription(d string) error {
	if strings.TrimSpace(d) == "" {
		return validation("description", "a text, not only spaces")
	}
	return nil
}
