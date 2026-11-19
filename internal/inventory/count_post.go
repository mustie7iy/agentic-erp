package inventory

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// The movement types a count's differences are posted with (ADR-0082 As
// built S2d; SAP MI07, from memory): a gain into stock, a loss out of it.
const (
	countGain = "701"
	countLoss = "702"
)

// Opener opens approval requests (workflow context).
type Opener interface {
	Open(ctx context.Context, in approvals.NewRequest) (uuid.UUID, bool, error)
}

// PostDifferences posts a counted document's differences at the caller's
// version (ADR-0082 As built S2d; SAP MI07): the valuations and stock of
// its materials locked after the document, the differences valued at
// their materials' prices, and, when their value, gains and losses alike,
// is within the company's tolerance, posted; above it the document waits
// for the controller's approval, which posts them (Approvals).
func (e *Endpoints) PostDifferences(ctx context.Context, id uuid.UUID, ifMatch func(CountDocument) error) (CountDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CountDocument{}, err
	}
	d, err := loadCount(ctx, tx, id, true)
	if err != nil {
		return CountDocument{}, err
	}
	if err := ifMatch(d); err != nil {
		return CountDocument{}, err
	}
	if d.Status != CountCounted {
		return CountDocument{}, problem.Conflict("inventory.count_invalid_status", "Count document "+d.Number+" is "+d.Status+"; its differences are "+
			"posted once every item is counted, once.")
	}
	in, err := e.countPosting(ctx, d)
	if err != nil {
		return CountDocument{}, err
	}
	value, err := e.differenceValue(ctx, tx, p, in)
	if err != nil {
		return CountDocument{}, err
	}
	if value.amount > value.tolerance {
		return e.requestApproval(ctx, tx, p, in, value)
	}
	return e.postCount(ctx, tx, p, in)
}

// countPost is a count document about to be posted: its company and its
// date, what it names, and its differences as lines, the gains and the
// losses apart, each line the item's number.
type countPost struct {
	doc           CountDocument
	c             companyOf
	r             refs
	gains, losses []Line
}

// all are the lines of both movement types.
func (in countPost) all() []Line { return append(append([]Line{}, in.gains...), in.losses...) }

// countPosting reads what a document's posting needs and makes its lines:
// each item's difference, counted less book, in the material's base unit;
// an item without one moves nothing.
func (e *Endpoints) countPosting(ctx context.Context, d CountDocument) (countPost, error) {
	r, err := e.documentCountRefs(ctx, []CountDocument{d})
	if err != nil {
		return countPost{}, err
	}
	c, err := e.companyOf(ctx, d.LegalEntityID)
	if err != nil {
		return countPost{}, err
	}
	in := countPost{doc: d, c: c, r: r}
	for _, it := range d.Items {
		diff := new(big.Rat).Sub(money.ParseRat(*it.Counted), money.ParseRat(it.Book))
		l := Line{ItemNo: it.ItemNo, MaterialID: it.MaterialID, PlantID: d.PlantID, StorageLocationID: d.StorageLocationID,
			Quantity: money.PlainQuantity(diff), EntryQuantity: money.PlainQuantity(new(big.Rat).Abs(diff)), EntryUnit: r.materials[it.MaterialID].BaseUnit}
		switch diff.Sign() {
		case 1:
			l.LineNo = int16(len(in.gains) + 1) // at most 100 items (the contract's)
			in.gains = append(in.gains, l)
		case -1:
			l.LineNo = int16(len(in.losses) + 1)
			in.losses = append(in.losses, l)
		}
	}
	return in, nil
}

// differences is the value of a document's differences, gains and losses
// alike, and the company's tolerance, in minor units of its currency.
type differences struct {
	amount, tolerance money.Minor
	currency          string
	exponent          int
}

// differenceValue locks the valuations and the stock the differences
// move, in the order every posting takes them, and values each as it would
// be posted (valueLines), on copies of the valuations: the posting reads
// them again under the same locks.
func (e *Endpoints) differenceValue(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in countPost) (differences, error) {
	out := differences{currency: in.c.le.LocalCurrency}
	if len(in.all()) == 0 {
		return out, nil
	}
	vals, err := lockCount(ctx, tx, p, in)
	if err != nil {
		return differences{}, err
	}
	copies := make(map[valuationKey]*Valuation, len(vals))
	for k, v := range vals {
		c := *v
		copies[k], out.exponent = &c, v.Exponent
	}
	if out.amount, err = totalValue(in, copies); err != nil {
		return differences{}, err
	}
	// A validated configuration's tolerance parses; none (empty, which
	// does not) reads as zero, so every difference of any value is
	// approved.
	out.tolerance, _ = money.ParseDecimal(in.c.co.InventoryTolerance(), out.exponent)
	return out, nil
}

// totalValue values the differences as they would be posted, gains and
// losses apart, and totals their values, gains and losses alike.
func totalValue(in countPost, copies map[valuationKey]*Valuation) (money.Minor, error) {
	var lines []Line
	for _, m := range []struct {
		code  string
		lines []Line
	}{{countGain, append([]Line{}, in.gains...)}, {countLoss, append([]Line{}, in.losses...)}} {
		if err := valueLines(m.lines, movementTypes[m.code], copies, nil, in.r); err != nil {
			return 0, countValueErr(err, in.gains, copies, in.r)
		}
		lines = append(lines, m.lines...)
	}
	return absTotal(in.doc.Number, lines)
}

// countValueErr is valueLines' refusal as a count's posting gives it: a
// surplus of a material without a moving price (the gain valueLines
// stopped at, its valuation untouched) has no amount to name, as a
// receipt has; its price is changed first. Any other refusal is as it is.
func countValueErr(err error, gains []Line, vals map[valuationKey]*Valuation, r refs) error {
	var p *problem.Problem
	if !errors.As(err, &p) || p.Code != "inventory.no_price" {
		return err
	}
	for _, l := range gains {
		if v := vals[valuationKey{l.MaterialID, l.PlantID}]; v.PriceControl == MovingAverage && v.Price == 0 {
			return problem.Unprocessable("inventory.no_price", "Material "+r.materials[l.MaterialID].Number+" has no moving price in plant "+
				r.units[l.PlantID].Code+" yet, so its surplus has no value; set its price (POST /v1/material-valuations/{id}:changePrice) and post "+
				"the differences again.")
		}
	}
	return err
}

// absTotal is the total of the lines' values, gains and losses alike
// (422 inventory.amount_too_large beyond an amount's range).
func absTotal(number string, lines []Line) (money.Minor, error) {
	values := make([]money.Minor, len(lines))
	for i, l := range lines {
		values[i] = max(l.Amount, -l.Amount)
	}
	total, err := money.Sum(values...)
	if err != nil {
		return 0, problem.Unprocessable("inventory.amount_too_large", "The value of count document "+number+"'s differences is beyond the range of "+
			"an amount.")
	}
	return total, nil
}

// lockCount locks the valuations and then the stock rows the differences
// move, each in the order every posting takes them, before the first of
// its two material documents does, so the second locks nothing a posting
// between them could hold. The document's own block is not checked: its
// items are what blocks the location, and a loss never exceeds the book
// quantity, which the block kept the stock at.
func lockCount(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in countPost) (map[valuationKey]*Valuation, error) {
	vals, err := lockValuations(ctx, tx, in.all(), in.r)
	if err != nil {
		return nil, err
	}
	keys := make([]stockKey, 0, len(in.all()))
	for _, l := range in.all() {
		keys = append(keys, keyOf(l))
	}
	slices.SortFunc(keys, stockKey.compare)
	if _, err := lockStock(ctx, tx, p, in.c.le.ID, keys); err != nil {
		return nil, err
	}
	return vals, nil
}

// postCount posts a document's differences (ADR-0082 As built S2d): its
// items closed first, lifting the posting block, then the gains as one
// 701 and the losses as one 702 material document, each valued and
// posted as any receipt or issue against the class's difference account
// and published as MaterialDocumentPosted; each item keeps its difference,
// its value and its material document, and the document is posted. The
// caller holds the document's row lock.
func (e *Endpoints) postCount(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in countPost) (CountDocument, error) {
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_item SET open = false WHERE count_document_id = $1`, in.doc.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: close count document %s: %w", in.doc.Number, err)
	}
	if len(in.all()) > 0 {
		if _, err := lockCount(ctx, tx, p, in); err != nil {
			return CountDocument{}, err
		}
	}
	posted := map[int16]Line{}
	docs := map[int16]uuid.UUID{}
	for _, m := range []struct {
		code  string
		lines []Line
	}{{countGain, in.gains}, {countLoss, in.losses}} {
		if len(m.lines) == 0 {
			continue
		}
		md, err := e.write(ctx, tx, p, posting{company: in.c.le, movementType: m.code, day: in.c.day.Date, lines: m.lines}, in.r)
		if err != nil {
			return CountDocument{}, err
		}
		if err := e.record(ctx, "inventory.material_document.post", md, in.r, map[string]any{"has_header_text": false,
			"count_document_id": in.doc.ID, "count_document_number": in.doc.Number}); err != nil {
			return CountDocument{}, err
		}
		for _, l := range md.Lines {
			posted[l.ItemNo], docs[l.ItemNo] = l, md.ID
		}
	}
	if err := writePosted(ctx, tx, in.doc, posted, docs); err != nil {
		return CountDocument{}, err
	}
	return closeCount(ctx, tx, p, in.doc, CountPosted, "post", in.r, in.c.le.LocalCurrency)
}

// writePosted writes each item's difference, value and material document;
// an item without a difference posted zero.
func writePosted(ctx context.Context, tx pgx.Tx, d CountDocument, posted map[int16]Line, docs map[int16]uuid.UUID) error {
	n := len(d.Items)
	nos, diffs, amounts, mds := make([]int16, n), make([]string, n), make([]money.Minor, n), make([]string, n)
	for i, it := range d.Items {
		nos[i], diffs[i] = it.ItemNo, "0"
		if l, ok := posted[it.ItemNo]; ok {
			diffs[i], amounts[i], mds[i] = l.Quantity, l.Amount, docs[it.ItemNo].String()
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_item i SET difference = x.d::numeric, amount = x.a, material_document_id = nullif(x.m, '')::uuid
		FROM unnest($2::smallint[], $3::text[], $4::bigint[], $5::text[]) AS x (n, d, a, m) WHERE i.count_document_id = $1 AND i.item_no = x.n`,
		d.ID, nos, diffs, amounts, mds); err != nil {
		return fmt.Errorf("inventory: differences of count document %s: %w", d.Number, err)
	}
	return nil
}

// closeCount moves a document to a closed status, posted, rejected or
// withdrawn, closing its items (the posting block lifted), with its audit
// record.
// A posted document takes its company's currency, its items' values'.
func closeCount(ctx context.Context, tx pgx.Tx, p reqctx.Principal, d CountDocument, status, action string, r refs, currency string) (CountDocument, error) {
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_item SET open = false WHERE count_document_id = $1 AND open`, d.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: close count document %s: %w", d.Number, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_document SET status = $2, closed_at = now(), currency = coalesce(currency, nullif($4, '')),
			version = version + 1, updated_by = $3 WHERE id = $1`, d.ID, status, p.ID, currency); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: %s count document %s: %w", action, d.Number, err)
	}
	closed, err := loadCount(ctx, tx, d.ID, false)
	if err != nil {
		return CountDocument{}, err
	}
	return closed, recordCount(ctx, "inventory.count_document."+action, closed, r)
}

// requestApproval asks the controller to approve differences above the
// company's tolerance (object type inventory_difference): the document
// waits, pending_approval, naming the request and the value it is asked
// for, its posting block held.
func (e *Endpoints) requestApproval(ctx context.Context, tx pgx.Tx, p reqctx.Principal, in countPost, v differences) (CountDocument, error) {
	d := in.doc
	currency := v.currency
	wf, needed, err := e.Requests.Open(ctx, approvals.NewRequest{ObjectType: approvals.InventoryDifference, ObjectID: d.ID,
		Title: fmt.Sprintf("Inventory differences of count document %s at %s: %s %s", d.Number, in.r.where(stockKey{plant: d.PlantID,
			location: d.StorageLocationID}), v.amount.Plain(v.exponent), v.currency), Amount: &v.amount, Currency: &currency, LegalEntityID: d.LegalEntityID})
	if err != nil {
		return CountDocument{}, fmt.Errorf("inventory: open the approval of count document %s: %w", d.Number, err)
	}
	// The fixed role always asks one approval; a policy that asked none
	// would be a fault of the server's wiring, not of the request, so it
	// fails as one (500) rather than posting differences unapproved.
	if !needed {
		return CountDocument{}, fmt.Errorf("inventory: count document %s: the approval policy asks no approval of its differences", d.Number)
	}
	if _, err := tx.Exec(ctx, `UPDATE inventory.count_document SET status = 'pending_approval', workflow_request_id = $2, difference_value = $3,
			currency = $4, version = version + 1, updated_by = $5 WHERE id = $1`, d.ID, wf, v.amount, v.currency, p.ID); err != nil {
		return CountDocument{}, fmt.Errorf("inventory: count document %s pending approval: %w", d.Number, err)
	}
	pending, err := loadCount(ctx, tx, d.ID, false)
	if err != nil {
		return CountDocument{}, err
	}
	return pending, recordCount(ctx, "inventory.count_document.request", pending, in.r)
}

// decide moves a document pending approval on as its request completed:
// approved, its differences posted as valued now; rejected or withdrawn,
// closed with nothing posted.
func (e *Endpoints) decide(ctx context.Context, id uuid.UUID, status, action string) (CountDocument, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return CountDocument{}, err
	}
	d, err := loadCount(ctx, tx, id, true)
	if err != nil {
		return CountDocument{}, err
	}
	if d.Status != CountPendingApproval {
		return CountDocument{}, problem.Conflict("inventory.count_invalid_status", "Count document "+d.Number+" is "+d.Status+"; "+action+
			" requires it pending approval.")
	}
	in, err := e.countPosting(ctx, d)
	if err != nil {
		return CountDocument{}, err
	}
	if status == CountPosted {
		return e.postCount(ctx, tx, p, in)
	}
	return closeCount(ctx, tx, p, d, status, action, in.r, "")
}

// Approvals moves count documents on when the approval requests of their
// differences complete (approvals.Handler, approvals.Locator).
type Approvals struct{ Endpoints *Endpoints }

// Approved posts the differences.
func (a Approvals) Approved(ctx context.Context, document, _ uuid.UUID) error {
	_, err := a.Endpoints.decide(ctx, document, CountPosted, "post")
	return err
}

// Rejected closes the document with nothing posted: its materials are
// recounted by a new document.
func (a Approvals) Rejected(ctx context.Context, document, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.decide(ctx, document, CountRejected, "reject")
	return err
}

// Cancelled closes the document with nothing posted: its requester
// withdrew the request.
func (a Approvals) Cancelled(ctx context.Context, document, _ uuid.UUID, _ string) error {
	_, err := a.Endpoints.decide(ctx, document, CountWithdrawn, "withdraw")
	return err
}

// Resources is where the API reads each count document.
func (Approvals) Resources(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	return approvals.Paths(countPath+"%s", ids), nil
}
