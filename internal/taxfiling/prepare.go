package taxfiling

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// prepared is what a prepare computed: the return as stored, without its
// read-back fields, the items it claimed and its boxes' values.
type prepared struct {
	r       Return
	items   []ledger.ClaimedItem
	amended *Return
}

// prepare prepares the registration's return of the period starting on
// start, of the kind (Original or Amendment), and opens its approval
// request (ADR-0049 §11). The registration is locked first, so two
// prepares of it run one after the other and the second finds the first's
// return; the items are claimed by an update that skips those another
// return claimed, so no item is in two returns.
func (e *Endpoints) prepare(ctx context.Context, id uuid.UUID, start time.Time, kind string) (Return, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Return{}, err
	}
	s, err := e.scope(ctx, id, start)
	if err != nil {
		return Return{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT FROM tax.registration WHERE id = $1 FOR UPDATE`, id); err != nil {
		return Return{}, fmt.Errorf("taxfiling: lock tax registration %s: %w", s.reg.Code, err)
	}
	pr := prepared{r: Return{ID: uuid.Must(uuid.NewV7()), Kind: kind}}
	if pr.amended, err = e.standing(ctx, tx, s, kind); err != nil {
		return Return{}, err
	}
	if _, err := e.closedPeriods(ctx, s.entities, s.codes, s.period.Start, s.period.End); err != nil {
		return Return{}, err
	}
	if err := e.claim(ctx, s, &pr); err != nil {
		return Return{}, err
	}
	if pr.r.ApprovalRequestID, err = e.openRequest(ctx, s, pr.r); err != nil {
		return Return{}, err
	}
	if err := insertReturn(ctx, tx, p, s, pr); err != nil {
		return Return{}, err
	}
	stored, err := readReturn(ctx, tx, pr.r.ID, nil, false)
	if err != nil {
		return Return{}, err
	}
	return stored, audit.Record(ctx, "tax.return.prepare", "tax.tax_return", stored.ID,
		map[string]any{"registration_code": stored.RegistrationCode, "kind": kind, "period_start": start.Format(time.DateOnly), "item_count": stored.ItemCount,
			"items_hash": stored.ItemsHash, "approval_request_id": stored.ApprovalRequestID},
		event("tax_return_prepared", stored, preparedEvent(stored)))
}

// standing checks what the period's returns allow: an original when none
// stands and every earlier period is filed (inOrder), an amendment of its
// latest filed return, which it returns, and either only when no return
// of the period is in progress. The indexes tax_return_one_original and
// tax_return_in_progress back it up.
func (e *Endpoints) standing(ctx context.Context, tx pgx.Tx, s *returnScope, kind string) (*Return, error) {
	var progress, original, filed *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT (array_agg(id) FILTER (WHERE status IN ('prepared', 'approved')))[1],
			(array_agg(id) FILTER (WHERE kind = 'original' AND status IN ('prepared', 'approved', 'filed')))[1],
			(array_agg(id ORDER BY created_at DESC, id DESC) FILTER (WHERE status = 'filed'))[1]
		FROM tax.tax_return WHERE registration_id = $1 AND period_start = $2`, s.reg.ID, s.period.Start).Scan(&progress, &original, &filed); err != nil {
		return nil, fmt.Errorf("taxfiling: returns of %s for %s: %w", s.reg.Code, s.period.Start.Format(time.DateOnly), err)
	}
	period := s.period.Start.Format(time.DateOnly) + " to " + s.period.End.Format(time.DateOnly)
	switch {
	case progress != nil:
		return nil, problem.Conflict("tax.return_in_progress", fmt.Sprintf("Return %s of %s for %s is prepared or approved; file or withdraw it first.",
			progress, s.reg.Code, period))
	case kind == Original && original != nil:
		return nil, problem.Conflict("tax.return_exists", fmt.Sprintf("Return %s of %s for %s stands; once filed, a correction is an amendment.",
			original, s.reg.Code, period))
	case kind == Original:
		return nil, inOrder(ctx, tx, s)
	case filed == nil:
		return nil, problem.Conflict("tax.nothing_to_amend", fmt.Sprintf("No return of %s for %s is filed: prepare the original.", s.reg.Code, period))
	}
	amended, err := readReturn(ctx, tx, *filed, nil, false)
	if err != nil {
		return nil, err
	}
	// The amendment follows the amended return's definition, so its
	// corrected figures add up box by box.
	pack := e.Packs.Get(amended.PackCountry, amended.PackVersion)
	if pack == nil || pack.Return(amended.ReturnType) == nil {
		return nil, fmt.Errorf("taxfiling: return %s was prepared by pack %s %s, which no longer defines %s", amended.ID, amended.PackCountry,
			amended.PackVersion, amended.ReturnType)
	}
	s.pack, s.def = pack, pack.Return(amended.ReturnType)
	return &amended, nil
}

// inOrder refuses an original while an earlier period of the registration
// has no filed return (ADR-0049 §11 As built, period order): the original
// claims the unreported items dated by its period's end, so an earlier
// period's would otherwise be claimed before their own return reported
// them. Once a period is filed, its items that arrive later are the next
// return's (P7) or its amendment's. The first period has none before it:
// items dated earlier are of the periods filed before the registration's
// first period, the cutover, and its return claims them as prior-period
// items (T61h). Filed is final (tax_return_fixed), so a concurrent prepare
// or filing can only make a refused prepare pass later, never one pass
// that should not; the caller holds the registration FOR UPDATE besides.
func inOrder(ctx context.Context, tx pgx.Tx, s *returnScope) error {
	earlier := config.TaxPeriods(s.reg.FirstPeriodStart, s.reg.Frequency, s.reg.LastPeriodEnd, s.period.Start)
	earlier = earlier[:len(earlier)-1] // scope found the period last
	if len(earlier) == 0 {
		return nil
	}
	rows, _ := tx.Query(ctx, `SELECT DISTINCT period_start FROM tax.tax_return WHERE registration_id = $1 AND period_start < $2 AND status = 'filed'`,
		s.reg.ID, s.period.Start)
	filed, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return fmt.Errorf("taxfiling: filed returns of %s before %s: %w", s.reg.Code, s.period.Start.Format(time.DateOnly), err)
	}
	for _, p := range earlier {
		if !slices.ContainsFunc(filed, p.Start.Equal) {
			return problem.Conflict("tax.earlier_period_unfiled", fmt.Sprintf("Period %s to %s of registration %s has no filed return: prepare, "+
				"approve and file it before the return of %s to %s, so each period's items are reported in its own return.",
				p.Start.Format(time.DateOnly), p.End.Format(time.DateOnly), s.reg.Code, s.period.Start.Format(time.DateOnly),
				s.period.End.Format(time.DateOnly)))
		}
	}
	return nil
}

// closedPeriods returns the leading ledger's posting periods of the
// companies that overlap the tax period, once none is open: a posting
// could still be under way in one (ADR-0049 §11, T32).
func (e *Endpoints) closedPeriods(ctx context.Context, entities []uuid.UUID, codes map[uuid.UUID]string, start, end time.Time) ([]ledger.Period, error) {
	periods, err := e.Periods.Overlapping(ctx, entities, start, end)
	if err != nil {
		return nil, err
	}
	for _, fp := range periods {
		if fp.Status == ledger.Open {
			return nil, problem.Conflict("tax.fiscal_period_open", fmt.Sprintf("Fiscal period %d/%02d of company %s (%s to %s) is open: close it first "+
				"(closeFiscalPeriod, %s), so no posting is under way in the tax period.", fp.FiscalYear, fp.Period, codes[fp.LegalEntityID],
				fp.StartDate.Format(time.DateOnly), fp.EndDate.Format(time.DateOnly), fp.ID))
		}
	}
	return periods, nil
}

// claim claims the items the return's boxes select and computes its
// snapshot from them: an original's of the period and earlier, an
// amendment's of the period alone, which must correct something.
func (e *Endpoints) claim(ctx context.Context, s *returnScope, pr *prepared) error {
	var from *time.Time
	if pr.amended != nil {
		from = &s.period.Start
	}
	var sels []ledger.TaxItemSelection
	for _, b := range s.def.Boxes {
		if b.Select != nil {
			sels = append(sels, selection(b.Select))
		}
	}
	groups, items, err := e.Items.Claim(ctx, s.entities, from, s.period.Start, s.period.End, sels, pr.r.ID)
	if err != nil {
		return err
	}
	if pr.amended != nil && len(items) == 0 {
		return problem.Conflict("tax.nothing_to_amend", fmt.Sprintf("No tax item dated %s to %s has arrived since return %s of %s was filed.",
			s.period.Start.Format(time.DateOnly), s.period.End.Format(time.DateOnly), pr.amended.ID, s.reg.Code))
	}
	sums, err := s.sum(groups)
	if err != nil {
		return err
	}
	all, prior, err := s.values(ctx, sums)
	if err != nil {
		return err
	}
	corrected, err := s.corrected(ctx, pr.amended, all)
	if err != nil {
		return err
	}
	pr.items, pr.r.ItemCount, pr.r.ItemsHash = items, int64(len(items)), itemsHash(items)
	pr.r.PriorItemCount, pr.r.PriorTax = sums.priorItems, sums.priorTax
	if threshold, set := s.def.Threshold(s.exp); set {
		pr.r.PriorThreshold, pr.r.AboveThreshold = &threshold, sums.aboveThreshold(threshold)
	}
	for _, b := range s.def.Boxes {
		box := ReturnBox{Code: b.Code, Label: b.Label, Kind: "expression", Amount: all[b.Code], PriorPeriodAmount: prior[b.Code],
			CorrectedAmount: corrected[b.Code]}
		if b.Select != nil {
			n := sums.boxes[b.Code].items
			box.Kind, box.ItemCount = "selection", &n
		}
		pr.r.Boxes = append(pr.r.Boxes, box)
	}
	return nil
}

// corrected is every box's figure for the period after the return: an
// original's own values; an amendment's selection boxes the amended
// return's corrected figures plus its own, its expression boxes computed
// from those.
func (s *returnScope) corrected(ctx context.Context, amended *Return, all map[string]money.Minor) (map[string]money.Minor, error) {
	if amended == nil {
		return all, nil
	}
	before := map[string]money.Minor{}
	for _, b := range amended.Boxes {
		before[b.Code] = b.CorrectedAmount
	}
	sel := map[string]money.Minor{}
	for _, b := range s.def.Boxes {
		if b.Select == nil {
			continue
		}
		v, err := money.Sum(before[b.Code], all[b.Code])
		if err != nil {
			return nil, fmt.Errorf("taxfiling: amendment of %s box %s: %w", amended.ID, b.Code, err)
		}
		sel[b.Code] = v
	}
	out, err := s.def.Evaluate(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("taxfiling: amendment of %s: %w", amended.ID, err)
	}
	return out, nil
}

// itemsHash is the SHA-256, in hex, of the claimed items' keys and local
// amounts, a line each in key order (the order Claim returns them in).
func itemsHash(items []ledger.ClaimedItem) string {
	var b bytes.Buffer
	for _, it := range items {
		fmt.Fprintf(&b, "%d|%s|%d|%d|%d|%d\n", it.FiscalYear, it.EntryID, it.ItemNo, it.BaseLC, it.TaxLC, it.RecoverableLC)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

// openRequest opens the return's tax_return approval request, which the
// fixed role always requires.
func (e *Endpoints) openRequest(ctx context.Context, s *returnScope, r Return) (uuid.UUID, error) {
	what := "return"
	if r.Kind == Amendment {
		what = "amendment of the return"
	}
	request, needed, err := e.Approvals.Open(ctx, approvals.NewRequest{ObjectType: approvals.TaxReturn, ObjectID: r.ID,
		Title: fmt.Sprintf("Approve the %s %s of registration %s for %s to %s", what, s.def.Type, s.reg.Code, s.period.Start.Format(time.DateOnly),
			s.period.End.Format(time.DateOnly)), LegalEntityID: s.reg.LegalEntityID, Company: s.codes[s.reg.LegalEntityID]})
	if err == nil && !needed {
		err = errors.New("the approval policy requires no approval, which it always must")
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("taxfiling: approval of return %s: %w", r.ID, err)
	}
	return request, nil
}

// insertReturn stores the prepared return, its boxes and the keys of its
// items.
func insertReturn(ctx context.Context, tx pgx.Tx, p reqctx.Principal, s *returnScope, pr prepared) error {
	r := pr.r
	var amends *uuid.UUID
	if pr.amended != nil {
		amends = &pr.amended.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tax.tax_return (tenant_id, id, registration_id, period_start, period_end, kind, amends_id, return_type,
			return_name, pack_country, pack_version, currency, item_count, items_hash, prior_item_count, prior_tax, prior_threshold, above_threshold,
			approval_request_id, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20)`,
		p.TenantID, r.ID, s.reg.ID, s.period.Start, s.period.End, r.Kind, amends, s.def.Type, s.def.Name, s.pack.Manifest.Country,
		s.pack.Manifest.Version, s.currency, r.ItemCount, r.ItemsHash, r.PriorItemCount, r.PriorTax, r.PriorThreshold, r.AboveThreshold,
		r.ApprovalRequestID, p.ID); err != nil {
		return fmt.Errorf("taxfiling: insert return %s of %s: %w", r.ID, s.reg.Code, err)
	}
	n := len(r.Boxes)
	codes, labels, kinds := make([]string, n), make([]string, n), make([]string, n)
	amounts, priors, corrected, counts := make([]money.Minor, n), make([]money.Minor, n), make([]money.Minor, n), make([]*int64, n)
	for i, b := range r.Boxes {
		codes[i], labels[i], kinds[i], amounts[i], priors[i], corrected[i], counts[i] = b.Code, b.Label, b.Kind, b.Amount, b.PriorPeriodAmount,
			b.CorrectedAmount, b.ItemCount
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tax.tax_return_box (tenant_id, tax_return_id, box_no, code, label, kind, amount, prior_period_amount,
			corrected_amount, item_count)
		SELECT $1, $2, x.n, x.c, x.l, x.k, x.a, x.p, x.r, x.i
		FROM unnest($3::text[], $4::text[], $5::text[], $6::bigint[], $7::bigint[], $8::bigint[], $9::bigint[]) WITH ORDINALITY AS x(c, l, k, a, p, r, i, n)`,
		p.TenantID, r.ID, codes, labels, kinds, amounts, priors, corrected, counts); err != nil {
		return fmt.Errorf("taxfiling: insert the boxes of return %s: %w", r.ID, err)
	}
	years, entries, nos := make([]int16, len(pr.items)), make([]uuid.UUID, len(pr.items)), make([]int32, len(pr.items))
	for i, it := range pr.items {
		years[i], entries[i], nos[i] = it.FiscalYear, it.EntryID, it.ItemNo
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tax.tax_return_item (tenant_id, tax_return_id, fiscal_year, journal_entry_id, item_no)
		SELECT $1, $2, x.y, x.e, x.n FROM unnest($3::smallint[], $4::uuid[], $5::integer[]) AS x(y, e, n)`, p.TenantID, r.ID, years, entries, nos); err != nil {
		return fmt.Errorf("taxfiling: insert the items of return %s: %w", r.ID, err)
	}
	return nil
}

// preparedEvent is TaxReturnPrepared's payload beside the return's names:
// its snapshot, in its currency.
func preparedEvent(r Return) map[string]any {
	boxes := make([]map[string]any, len(r.Boxes))
	for i, b := range r.Boxes {
		boxes[i] = map[string]any{"code": b.Code, "amount": moneyOf(b.Amount, r), "corrected_amount": moneyOf(b.CorrectedAmount, r)}
	}
	return map[string]any{"amends_id": r.AmendsID, "pack_country": r.PackCountry, "pack_version": r.PackVersion, "item_count": r.ItemCount,
		"items_hash": r.ItemsHash, "boxes": boxes, "approval_request_id": r.ApprovalRequestID}
}

// moneyOf is an amount of the return's currency as events carry it.
func moneyOf(v money.Minor, r Return) map[string]any {
	m := apitypes.Money(v, r.Currency, r.Exponent)
	return map[string]any{"amount": m.Amount, "currency": m.Currency}
}
