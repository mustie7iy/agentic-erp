package records

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/jsonl"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ArchiveFormat names the archive file's form and its manifest's
// (ADR-0079 §7), which the manifest states. A line of the file is an
// entry in the ledger's record form (ledger.EntryRecord): JSON, one entry
// per line, its amounts decimal strings in their currency's digits, a
// debit positive and a credit negative.
const ArchiveFormat = "agentic-erp.records.journal_entry.v1"

// manifest describes an archive file (ADR-0079 §7): what it holds, its
// hash and size, and the count of its entries and lines and the sum of
// their debits in the company's currency. Every field is the run's or
// the file's, so the manifest of a run written again is the same bytes.
type manifest struct {
	Format               string        `json:"format"`
	ArchivingRunID       uuid.UUID     `json:"archiving_run_id"`
	LegalEntityID        uuid.UUID     `json:"legal_entity_id"`
	CompanyCode          string        `json:"company_code"`
	DocumentKind         string        `json:"document_kind"`
	FiscalYear           int16         `json:"fiscal_year"`
	RetentionYears       int16         `json:"retention_years"`
	ConfigurationVersion int32         `json:"configuration_version"`
	RequestedBy          uuid.UUID     `json:"requested_by"`
	RequestedAt          string        `json:"requested_at"`
	File                 string        `json:"file"`
	SHA256               string        `json:"sha256"`
	SizeBytes            int64         `json:"size_bytes"`
	EntryCount           int32         `json:"entry_count"`
	LineCount            int32         `json:"line_count"`
	DebitTotal           string        `json:"debit_total"`
	Currency             string        `json:"currency"`
	Parts                []ArchivePart `json:"parts,omitempty"`
}

// ArchivePart is a part of an archive file written in parts, as the
// manifest and the run name it.
type ArchivePart struct {
	Part       int32  `json:"part"`
	Name       string `json:"name"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
	EntryCount int32  `json:"entry_count"`
}

// The most entries, and bytes, a part of an archive file holds (ADR-0079
// §7, review:batch75): the archiver writes a year a part at a time, each
// part an object of its own, and holds at most two in memory (the first
// until the second shows the file needs parts).
const (
	ArchivePartEntries = 50_000
	ArchivePartBytes   = 32 << 20
)

// archive is a run's archive file and manifest as written.
type archive struct {
	manifest               []byte
	sum, manifestSum       [32]byte
	size                   int64
	entries, lines         int32
	debits                 money.Minor
	currency, key, metaKey string
	parts                  []ArchivePart
}

// fileName is the archive file's name, by run id.
func fileName(id uuid.UUID) string { return id.String() + ".jsonl" }

// partName is the name of the n-th part, from 1, of a run's file written
// in parts.
func partName(id uuid.UUID, n int) string { return fmt.Sprintf("%s.part-%d.jsonl", id, n) }

// writer writes a run's file a part at a time: the first part is held
// until a second shows the file needs parts; a file within one part is
// written whole under the file's name. whole hashes the file across its
// parts.
type writer struct {
	e      *Endpoints
	ctx    context.Context
	tenant uuid.UUID
	run    uuid.UUID
	whole  hash.Hash
	size   int64
	held   []byte
	heldN  int32
	// holding tells that the first part is held: an empty year's one part
	// is held as no bytes at all.
	holding bool
	parts   []ArchivePart
}

// part takes a full part (or the last).
func (w *writer) part(content []byte, entries int32) error {
	w.whole.Write(content) // a hash's Write never fails
	w.size += int64(len(content))
	if !w.holding && len(w.parts) == 0 {
		w.held, w.heldN, w.holding = bytes.Clone(content), entries, true
		return nil
	}
	if w.holding {
		if err := w.write(w.held, w.heldN); err != nil {
			return err
		}
		w.held, w.holding = nil, false
	}
	return w.write(content, entries)
}

// write keeps the next part under its name.
func (w *writer) write(content []byte, entries int32) error {
	n := len(w.parts) + 1
	sum := sha256.Sum256(content)
	if err := w.e.keep(w.ctx, blobstore.Key(w.tenant, blobstore.KindArchives, partName(w.run, n)), content, "application/x-ndjson"); err != nil {
		return err
	}
	w.parts = append(w.parts, ArchivePart{Part: int32(n), Name: partName(w.run, n), SHA256: hexOf(sum[:]), SizeBytes: int64(len(content)), // #nosec G115 -- a part number, at most a year's entries
		EntryCount: entries})
	return nil
}

// finish writes a file held in one part whole and returns the key the
// run names: the file's, or its first part's.
func (w *writer) finish() (string, error) {
	if !w.holding {
		return blobstore.Key(w.tenant, blobstore.KindArchives, partName(w.run, 1)), nil
	}
	key := blobstore.Key(w.tenant, blobstore.KindArchives, fileName(w.run))
	return key, w.e.keep(w.ctx, key, w.held, "application/x-ndjson")
}

// build writes the run's archive file, a part at a time, and its
// manifest: every journal entry of the company's fiscal year, of every
// ledger, as the ledger's one read gives them.
func (e *Endpoints) build(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, r Run, le tenancy.Entity) (archive, error) {
	exps, err := currency.Load(ctx, tx)
	if err != nil {
		return archive{}, fmt.Errorf("records: archive of run %s: %w", r.ID, err)
	}
	a := archive{currency: le.LocalCurrency, metaKey: blobstore.Key(tenant, blobstore.KindArchives, r.ID.String()+".manifest.json")}
	w := &writer{e: e, ctx: ctx, tenant: tenant, run: r.ID, whole: sha256.New()}
	out := jsonl.New(cmp.Or(e.PartEntries, ArchivePartEntries), ArchivePartBytes, w.part)
	if err := e.Journal.Entries(ctx, r.LegalEntity, r.FiscalYear, func(x ledger.ArchivedEntry) error {
		line, err := a.add(x, exps)
		if err != nil {
			return err
		}
		return out.Add(line)
	}); err != nil {
		return archive{}, fmt.Errorf("records: archive of run %s: %w", r.ID, err)
	}
	if err := out.Close(); err != nil {
		return archive{}, fmt.Errorf("records: archive of run %s: %w", r.ID, err)
	}
	if a.key, err = w.finish(); err != nil {
		return archive{}, err
	}
	copy(a.sum[:], w.whole.Sum(nil))
	a.size, a.parts = w.size, w.parts
	m := manifest{Format: ArchiveFormat, ArchivingRunID: r.ID, LegalEntityID: r.LegalEntity, CompanyCode: le.Code, DocumentKind: r.Kind,
		FiscalYear: r.FiscalYear, RetentionYears: r.RetentionYears, ConfigurationVersion: r.ConfigVersion, RequestedBy: r.RequestedBy,
		RequestedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), File: fileName(r.ID), SHA256: hexOf(a.sum[:]), SizeBytes: a.size,
		EntryCount: a.entries, LineCount: a.lines, DebitTotal: a.debits.Plain(exps[a.currency]), Currency: a.currency, Parts: a.parts}
	a.manifest, _ = json.MarshalIndent(m, "", "  ") // strings, numbers and ids: it always marshals
	a.manifestSum = sha256.Sum256(a.manifest)
	return a, nil
}

// add counts the entry and its lines and its debits in the company's
// currency, and gives it in the file's form.
func (a *archive) add(x ledger.ArchivedEntry, exps currency.Exponents) (ledger.EntryRecord, error) {
	for _, l := range x.Lines {
		if l.AmountLocal > 0 {
			var ok bool
			if a.debits, ok = addDebit(a.debits, l.AmountLocal); !ok {
				return ledger.EntryRecord{}, problem.Unprocessable("records.amount_too_large", "The year's debits add up to more than an amount holds.")
			}
		}
	}
	a.entries++
	a.lines += int32(len(x.Lines)) // #nosec G115 -- an entry's lines are numbered by an integer column
	return x.Record(exps), nil
}

// addDebit adds a debit to the sum, and reports false when the sum would
// be more than an amount holds.
func addDebit(sum, debit money.Minor) (money.Minor, bool) {
	if debit > math.MaxInt64-sum {
		return sum, false
	}
	return sum + debit, true
}
