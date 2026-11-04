package cash

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Snapshots of the liquidity forecast (ADR-0051 §6, T53f): the forecast
// of a company as it was at a read, kept so that what was forecast can be
// compared with what the statements later show (the variance, §11). The
// TakeCashSnapshot workflow takes one a day per company after its
// cash.snapshot_time; a person may take one on demand. A snapshot is read
// in one database state (Consistent) and written whole in the caller's
// transaction, at document level for the value dates within 31 days and
// summed per bucket beyond, and never changed; the workflow deletes those
// older than 25 months.

// Kinds of snapshot.
const (
	SnapshotDaily    = "daily"
	SnapshotOnDemand = "on_demand"
)

// What reading and writing a daily snapshot did.
const (
	SnapshotTaken   = "taken"   // written
	SnapshotExists  = "exists"  // the company's snapshot of the date was taken before
	SnapshotSkipped = "skipped" // not the company's date any more, or no such legal entity
)

// documentLevelDays: the flows dated up to the snapshot's date plus these
// days (31 days with it) are kept per document, those after per bucket.
const documentLevelDays = 30

// retentionMonths: a snapshot dated more than this before the workflow's
// date is deleted (two years of comparison and a month).
const retentionMonths = 25

// SnapshotterActor is the principal of the daily snapshots, in every
// tenant: a system actor, never a token holder.
var SnapshotterActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:cash-snapshotter"))

// Snapshotter is the principal the daily snapshots are read and taken as
// in the tenant (workflows.CashSnapshots).
func (*Endpoints) Snapshotter(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: SnapshotterActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// Snapshot is a snapshot of a legal entity's forecast with its totals per
// currency.
type Snapshot struct {
	ID, LegalEntity uuid.UUID
	CompanyCode     string // its legal entity's company code, as the directory has it
	Kind            string
	Name            *string
	Date            time.Time // the company's date at the read, in TimeZone
	TimeZone        string
	HorizonEnd      time.Time
	ConfigVersion   int32
	TakenAt         time.Time
	Currencies      []SnapshotCurrency
}

// DocumentLevelUntil is the last day the snapshot keeps per document.
func (s Snapshot) DocumentLevelUntil() time.Time { return s.Date.AddDate(0, 0, documentLevelDays) }

// SnapshotCurrency is a snapshot's totals in a currency: what the
// forecast's totals counted (every source but payable_blocked), what
// blocked suppliers were owed, and the lines kept.
type SnapshotCurrency struct {
	Currency       string
	Exponent       int
	Total, Blocked money.Minor
	Lines          int
}

// snapshotLine is a line a snapshot keeps: what a flow pays of a document
// under a liquidity item (end nil), or a bucket's sum of the flows of a
// source, certainty, item and bank from date to end.
type snapshotLine struct {
	flowNo                      int
	date                        time.Time
	end                         *time.Time
	bank                        *uuid.UUID
	currency, source, certainty string
	late                        bool
	item                        string // "": unclassified
	amount                      money.Minor
	count                       int
	supplier, payment           *uuid.UUID
	doc                         *Document
}

// taken is a forecast read for a snapshot: its company's code, date,
// zone, horizon, configuration version and lines.
type taken struct {
	company       string
	date, horizon time.Time
	zone          string
	version       int32
	lines         []snapshotLine
}

// TakeSnapshot takes a named snapshot of the legal entity's forecast on
// demand, the forecast read in a consistent read of its own (Consistent)
// and written in the request's transaction. A legal entity the caller
// cannot act in is not found.
func (e *Endpoints) TakeSnapshot(ctx context.Context, legalEntity uuid.UUID, name string) (Snapshot, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	co, err := e.company(ctx, legalEntity)
	if err != nil {
		return Snapshot{}, err
	}
	if e.Consistent == nil {
		return Snapshot{}, errors.New("cash: snapshots need a consistent read, which the composition root wires")
	}
	rctx, rtx, err := e.Consistent.Read(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	t, err := e.readSnapshot(rctx, rtx, co, legalEntity)
	// Read only: nothing to keep, and its connection goes back before the write.
	_ = rtx.Rollback(context.WithoutCancel(ctx))
	if err != nil {
		return Snapshot{}, err
	}
	id, _, err := store(ctx, tx, p, legalEntity, SnapshotOnDemand, &name, t)
	if err != nil {
		return Snapshot{}, err
	}
	return e.Snapshot(ctx, id)
}

// ReadDailySnapshot reads the legal entity's daily snapshot of the date in
// the caller's transaction, which the workflow begins read-only at
// REPEATABLE READ as its principal, who acts in every company, and returns
// what writes it: run in a transaction of its own after this one ends, so
// the workflow never holds one connection while it waits for another. It
// returns no writer, with the outcome, when there is nothing to write: the
// snapshot of the date exists (run again, it finds the first), the legal
// entity is not known, or the company's date is no longer the date (the
// workflow ran past midnight; a past day's forecast cannot be read again).
func (e *Endpoints) ReadDailySnapshot(ctx context.Context, legalEntity uuid.UUID, date time.Time) (func(ctx context.Context) (string, error), string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	co, ok, err := e.LegalEntities.Company(ctx, legalEntity)
	if err != nil {
		return nil, "", fmt.Errorf("cash: legal entity %s: %w", legalEntity, err)
	}
	if !ok {
		return nil, SnapshotSkipped, nil
	}
	found, err := dailyTaken(ctx, tx, legalEntity, date)
	if err != nil {
		return nil, "", err
	}
	if found {
		return nil, SnapshotExists, nil
	}
	t, err := e.readSnapshot(ctx, tx, co, legalEntity)
	if err != nil {
		return nil, "", err
	}
	if !t.date.Equal(date) {
		return nil, SnapshotSkipped, nil
	}
	return func(ctx context.Context) (string, error) { return storeDaily(ctx, legalEntity, t) }, "", nil
}

// storeDaily writes a daily snapshot read before: taken, or, when another
// run wrote the company's snapshot of the date meanwhile, nothing.
func storeDaily(ctx context.Context, legalEntity uuid.UUID, t taken) (string, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return "", err
	}
	_, written, err := store(ctx, tx, p, legalEntity, SnapshotDaily, nil, t)
	switch {
	case err != nil:
		return "", err
	case !written:
		return SnapshotExists, nil
	}
	return SnapshotTaken, nil
}

// dailyTaken reports whether the legal entity's daily snapshot of the
// date exists.
func dailyTaken(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, date time.Time) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM cash.snapshot WHERE legal_entity_id = $1 AND snapshot_date = $2 AND kind = 'daily')`,
		legalEntity, date).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("cash: daily snapshot of %s on %s: %w", legalEntity, date.Format(time.DateOnly), err)
	}
	return found, nil
}

// readSnapshot reads the legal entity's forecast in tx, which reads one
// database state, in the company's buckets, every certainty, and gives its
// lines.
func (e *Endpoints) readSnapshot(ctx context.Context, tx pgx.Tx, co Company, legalEntity uuid.UUID) (taken, error) {
	var bs []Bucket
	fr, err := e.readForecastOf(ctx, tx, co, legalEntity, CertaintyPlanned, func(r reader) (time.Time, error) {
		bs = buckets(r.read, r.settings.BucketsOrDefault())
		return bs[len(bs)-1].End, nil
	})
	if err != nil {
		return taken{}, err
	}
	t := taken{company: co.Code, date: fr.r.read, horizon: bs[len(bs)-1].End, zone: fr.zone, version: fr.version, lines: fr.snapshotLines(bs)}
	if fr.r.sums.overflow {
		return taken{}, errForecastTooLarge()
	}
	return t, nil
}

// snapshotLines are the forecast's flows up to the last bucket as a
// snapshot keeps them, by day: within 31 days of the read date each
// document a flow pays under each item it is classified in, beyond each
// bucket's sum per currency, source, certainty, item and bank.
func (fr forecastRead) snapshotLines(bs []Bucket) []snapshotLine {
	flows := slices.Clone(fr.flows)
	slices.SortStableFunc(flows, func(a, b datedFlow) int { return cmp.Compare(flowKey(a), flowKey(b)) })
	docEnd, horizon := fr.r.read.AddDate(0, 0, documentLevelDays), bs[len(bs)-1].End
	byCode := map[string]uuid.UUID{}
	for _, b := range fr.banks {
		byCode[b.Code] = b.ID
	}
	var out []snapshotLine
	sums, flowNo := newBucketSums(bs, docEnd.AddDate(0, 0, 1)), 0
	for i := range flows {
		f := &flows[i]
		if f.date.After(horizon) {
			continue
		}
		if f.bank == nil && f.bankCode != "" {
			if id, ok := byCode[f.bankCode]; ok {
				f.bank = &id
			}
		}
		if f.date.After(docEnd) {
			sums.add(fr.r.sums, *f)
			continue
		}
		flowNo++
		out = append(out, documentLines(*f, flowNo)...)
	}
	return append(out, sums.lines(flowNo+1)...)
}

// documentLines are a flow's lines at document level, of flow flowNo:
// each document it
// pays under each item that document's offsetting lines derive, when its
// documents are what it is split over (a supplier payment's, a memo
// record's); else (a run's payment, or a payment in transit, paying the
// items of its run) the flow's shares, each naming its one document. A
// share of nothing is no line.
func documentLines(f datedFlow, flowNo int) []snapshotLine {
	line := func(item string, amount money.Minor, doc *Document) snapshotLine {
		return snapshotLine{flowNo: flowNo, date: f.date, bank: f.bank, currency: f.currency, source: f.source, certainty: f.certainty, late: f.late,
			item: item, amount: amount, count: 1, supplier: f.supplier, payment: f.payment, doc: doc}
	}
	var out []snapshotLine
	if len(f.partShares) == len(f.documents) {
		for j, shares := range f.partShares {
			for _, sh := range shares {
				if sh.Amount != 0 {
					out = append(out, line(sh.Item, sh.Amount, &f.documents[j].Document))
				}
			}
		}
		return out
	}
	var doc *Document
	if len(f.documents) == 1 {
		doc = &f.documents[0].Document
	}
	for _, sh := range f.shares {
		if sh.Amount != 0 {
			out = append(out, line(sh.Item, sh.Amount, doc))
		}
	}
	return out
}

// bucketKey is what a bucket's sum is of.
type bucketKey struct {
	bucket                            int
	currency, source, certainty, item string
	bank                              uuid.UUID
}

// bucketSums are the sums per bucket of the flows after the document
// level, in the order their keys first appear (the flows' order).
type bucketSums struct {
	bs    []Bucket
	first time.Time // the first day summed: a bucket holding the document level's end starts after it
	order []bucketKey
	sum   map[bucketKey]*snapshotLine
}

func newBucketSums(bs []Bucket, first time.Time) *bucketSums {
	return &bucketSums{bs: bs, first: first, sum: map[bucketKey]*snapshotLine{}}
}

// add adds each of a flow's shares to its bucket's sum of its key.
func (b *bucketSums) add(s *sums, f datedFlow) {
	at, _ := slices.BinarySearchFunc(b.bs, f.date, func(bk Bucket, d time.Time) int { return cmp.Compare(bk.End.Unix(), d.Unix()) })
	var bank uuid.UUID
	if f.bank != nil {
		bank = *f.bank
	}
	for _, sh := range f.shares {
		if sh.Amount == 0 { // a share of nothing is no line, as at document level
			continue
		}
		k := bucketKey{bucket: at, currency: f.currency, source: f.source, certainty: f.certainty, item: sh.Item, bank: bank}
		l, ok := b.sum[k]
		if !ok {
			end := b.bs[at].End
			l = &snapshotLine{date: maxDay(b.bs[at].Start, b.first), end: &end, currency: f.currency, source: f.source, certainty: f.certainty, item: sh.Item}
			if f.bank != nil {
				l.bank = &bank
			}
			b.sum[k], b.order = l, append(b.order, k)
		}
		l.amount, l.count = s.add(l.amount, sh.Amount), l.count+1
	}
}

// lines are the sums by bucket, then in the order their keys appeared,
// numbered as flows from next.
func (b *bucketSums) lines(next int) []snapshotLine {
	slices.SortStableFunc(b.order, func(x, y bucketKey) int { return cmp.Compare(x.bucket, y.bucket) })
	out := make([]snapshotLine, len(b.order))
	for i, k := range b.order {
		out[i] = *b.sum[k]
		out[i].flowNo = next + i
	}
	return out
}

// maxDay is the later day.
func maxDay(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// store writes the snapshot and its lines, audits it and publishes
// CashSnapshotTaken; a daily snapshot of a company and date that exists
// is not written again (written false, and the id is zero).
func store(ctx context.Context, tx pgx.Tx, p reqctx.Principal, legalEntity uuid.UUID, kind string, name *string, t taken) (uuid.UUID, bool, error) {
	id := uuid.Must(uuid.NewV7())
	tag, err := tx.Exec(ctx, `INSERT INTO cash.snapshot (tenant_id, id, legal_entity_id, kind, name, snapshot_date, time_zone, horizon_end, config_version, taken_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (tenant_id, legal_entity_id, snapshot_date) WHERE kind = 'daily' DO NOTHING`,
		p.TenantID, id, legalEntity, kind, name, t.date, t.zone, t.horizon, t.version, p.ID)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("cash: snapshot of %s: %w", legalEntity, err)
	}
	if tag.RowsAffected() == 0 {
		return uuid.Nil, false, nil
	}
	if err := insertLines(ctx, tx, id, t.lines); err != nil {
		return uuid.Nil, false, err
	}
	detail := map[string]any{"legal_entity_id": legalEntity, "company_code": t.company, "kind": kind, "snapshot_date": t.date.Format(time.DateOnly),
		"config_version": t.version, "line_count": len(t.lines)}
	if name != nil {
		detail["name"] = *name
	}
	return id, true, audit.Record(ctx, "cash.snapshot.take", "cash.snapshot", id, detail,
		outbox.Event{Context: "cash", Name: "cash_snapshot_taken", AggregateType: "cash.snapshot", AggregateID: id,
			Payload: map[string]any{"cash_snapshot_id": id, "legal_entity_id": legalEntity, "company_code": t.company, "kind": kind,
				"snapshot_date": t.date.Format(time.DateOnly), "line_count": len(t.lines)}})
}

// insertLines writes a snapshot's lines in one statement, numbered from 1.
func insertLines(ctx context.Context, tx pgx.Tx, id uuid.UUID, lines []snapshotLine) error {
	n := len(lines)
	flowNo, dates, ends, banks := make([]int, n), make([]time.Time, n), make([]*time.Time, n), make([]*uuid.UUID, n)
	currencies, sources, certainties, late := make([]string, n), make([]string, n), make([]string, n), make([]bool, n)
	items, amounts, counts := make([]*string, n), make([]int64, n), make([]int, n)
	suppliers, payments, kinds, docs, numbers := make([]*uuid.UUID, n), make([]*uuid.UUID, n), make([]*string, n), make([]*uuid.UUID, n), make([]*string, n)
	for i, l := range lines {
		flowNo[i], dates[i], ends[i], banks[i] = l.flowNo, l.date, l.end, l.bank
		currencies[i], sources[i], certainties[i], late[i] = l.currency, l.source, l.certainty, l.late
		items[i], amounts[i], counts[i], suppliers[i], payments[i] = orNil(l.item), int64(l.amount), l.count, l.supplier, l.payment
		if d := l.doc; d != nil {
			kinds[i], docs[i], numbers[i] = &d.Kind, &d.ID, orNil(d.Number)
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO cash.snapshot_flow (tenant_id, snapshot_id, line_no, flow_no, value_date, bucket_end, house_bank_id, currency, source,
			certainty, late, liquidity_item, amount, flow_count, supplier_id, payment_id, document_kind, document_id, document_number)
		SELECT platform.current_tenant(), $1, x.ord, x.flow_no, x.value_date, x.bucket_end, x.house_bank_id, x.currency, x.source, x.certainty, x.late,
			x.liquidity_item, x.amount, x.flow_count, x.supplier_id, x.payment_id, x.document_kind, x.document_id, x.document_number FROM unnest($2::int[], $3::date[], $4::date[], $5::uuid[], $6::text[], $7::text[], $8::text[], $9::bool[],
			$10::text[], $11::bigint[], $12::int[], $13::uuid[], $14::uuid[], $15::text[], $16::uuid[], $17::text[]) WITH ORDINALITY AS x(
			flow_no, value_date, bucket_end, house_bank_id, currency, source, certainty, late, liquidity_item, amount, flow_count, supplier_id, payment_id,
			document_kind, document_id, document_number, ord)`,
		id, flowNo, dates, ends, banks, currencies, sources, certainties, late, items, amounts, counts, suppliers, payments, kinds, docs, numbers)
	if err != nil {
		return fmt.Errorf("cash: lines of snapshot %s: %w", id, err)
	}
	return nil
}

// orNil is the text, nil for "".
func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PruneSnapshots deletes the legal entity's snapshots dated more than 25
// months before the date, their lines with them, each audited, and says
// how many: run again, it finds none.
func (e *Endpoints) PruneSnapshots(ctx context.Context, legalEntity uuid.UUID, date time.Time) (int, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return 0, err
	}
	// The database computes the same day (cash.delete_expired_snapshots,
	// months back clamped to the month's last day); it is the audit's.
	before := addMonths(date, -retentionMonths)
	rows, _ := tx.Query(ctx, `SELECT deleted_id, deleted_kind, deleted_date FROM cash.delete_expired_snapshots($1, $2)`, legalEntity, date)
	type gone struct {
		id   uuid.UUID
		kind string
		date time.Time
	}
	deleted, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (gone, error) {
		var g gone
		return g, r.Scan(&g.id, &g.kind, &g.date)
	})
	if err != nil {
		return 0, fmt.Errorf("cash: delete the snapshots of %s before %s: %w", legalEntity, before.Format(time.DateOnly), err)
	}
	audited := make([]error, len(deleted))
	for i, g := range deleted {
		audited[i] = audit.Add(ctx, audit.Entry{Action: "cash.snapshot.delete", ObjectType: "cash.snapshot", ObjectID: g.id, Outcome: "succeeded",
			Detail: map[string]any{"legal_entity_id": legalEntity, "kind": g.kind, "snapshot_date": g.date.Format(time.DateOnly), "reason": "retention",
				"before": before.Format(time.DateOnly)}})
	}
	return len(deleted), errors.Join(audited...)
}
