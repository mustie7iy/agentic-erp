package records

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/ledger"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// ArchiveFormat names the archive file's form and its manifest's
// (ADR-0079 §7), which the manifest states.
const ArchiveFormat = "agentic-erp.records.journal_entry.v1"

// fileEntry is a journal entry as a line of the archive file (ADR-0079
// §7): JSON, one entry per line, its amounts decimal strings in their
// currency's digits, a debit positive and a credit negative.
type fileEntry struct {
	EntryID          uuid.UUID  `json:"entry_id"`
	Ledger           string     `json:"ledger"`
	DocumentType     string     `json:"document_type"`
	DocumentNumber   string     `json:"document_number"`
	FiscalYear       int16      `json:"fiscal_year"`
	FiscalPeriod     int16      `json:"fiscal_period"`
	DocumentDate     string     `json:"document_date"`
	PostingDate      string     `json:"posting_date"`
	Currency         string     `json:"currency"`
	LocalCurrency    string     `json:"local_currency"`
	GroupCurrency    string     `json:"group_currency"`
	RateToLocal      string     `json:"rate_to_local"`
	RateToGroup      string     `json:"rate_to_group"`
	Reference        *string    `json:"reference"`
	HeaderText       *string    `json:"header_text"`
	SourceContext    string     `json:"source_context"`
	SourceDocumentID *uuid.UUID `json:"source_document_id"`
	ReversalOfID     *uuid.UUID `json:"reversal_of_id"`
	ReversalOfYear   *int16     `json:"reversal_of_year"`
	PostedAt         string     `json:"posted_at"`
	PostedBy         uuid.UUID  `json:"posted_by"`
	Lines            []fileLine `json:"lines"`
}

// fileLine is a line of an archived entry.
type fileLine struct {
	LineNo      int32      `json:"line_no"`
	Account     string     `json:"account"`
	Amount      string     `json:"amount"`
	AmountLocal string     `json:"amount_local"`
	AmountGroup string     `json:"amount_group"`
	PartnerID   *uuid.UUID `json:"partner_id"`
	TaxCode     *string    `json:"tax_code"`
	Quantity    *string    `json:"quantity"`
	Unit        *string    `json:"unit"`
	Text        *string    `json:"text"`
}

// manifest describes an archive file (ADR-0079 §7): what it holds, its
// hash and size, and the count of its entries and lines and the sum of
// their debits in the company's currency. Every field is the run's or
// the file's, so the manifest of a run written again is the same bytes.
type manifest struct {
	Format               string    `json:"format"`
	ArchivingRunID       uuid.UUID `json:"archiving_run_id"`
	LegalEntityID        uuid.UUID `json:"legal_entity_id"`
	CompanyCode          string    `json:"company_code"`
	DocumentKind         string    `json:"document_kind"`
	FiscalYear           int16     `json:"fiscal_year"`
	RetentionYears       int16     `json:"retention_years"`
	ConfigurationVersion int32     `json:"configuration_version"`
	RequestedBy          uuid.UUID `json:"requested_by"`
	RequestedAt          string    `json:"requested_at"`
	File                 string    `json:"file"`
	SHA256               string    `json:"sha256"`
	SizeBytes            int64     `json:"size_bytes"`
	EntryCount           int32     `json:"entry_count"`
	LineCount            int32     `json:"line_count"`
	DebitTotal           string    `json:"debit_total"`
	Currency             string    `json:"currency"`
}

// archive is a run's archive file and manifest as written.
type archive struct {
	content, manifest      []byte
	sum, manifestSum       [32]byte
	entries, lines         int32
	debits                 money.Minor
	currency, key, metaKey string
}

// fileName is the archive file's name, by run id.
func fileName(id uuid.UUID) string { return id.String() + ".jsonl" }

// build writes the run's archive file and manifest: every journal entry
// of the company's fiscal year, of every ledger, as the ledger's one
// read gives them.
func (e *Endpoints) build(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, r Run, le tenancy.Entity) (archive, error) {
	exps, err := currency.Load(ctx, tx)
	if err != nil {
		return archive{}, fmt.Errorf("records: archive of run %s: %w", r.ID, err)
	}
	a := archive{currency: le.LocalCurrency, key: blobstore.Key(tenant, blobstore.KindArchives, fileName(r.ID)),
		metaKey: blobstore.Key(tenant, blobstore.KindArchives, r.ID.String()+".manifest.json")}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := e.Journal.Entries(ctx, r.LegalEntity, r.FiscalYear, func(x ledger.ArchivedEntry) error {
		line, err := a.add(x, exps)
		if err != nil {
			return err
		}
		return enc.Encode(line)
	}); err != nil {
		return archive{}, fmt.Errorf("records: archive of run %s: %w", r.ID, err)
	}
	a.content = buf.Bytes()
	a.sum = sha256.Sum256(a.content)
	m := manifest{Format: ArchiveFormat, ArchivingRunID: r.ID, LegalEntityID: r.LegalEntity, CompanyCode: le.Code, DocumentKind: r.Kind,
		FiscalYear: r.FiscalYear, RetentionYears: r.RetentionYears, ConfigurationVersion: r.ConfigVersion, RequestedBy: r.RequestedBy,
		RequestedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano), File: fileName(r.ID), SHA256: hexOf(a.sum[:]), SizeBytes: int64(len(a.content)),
		EntryCount: a.entries, LineCount: a.lines, DebitTotal: a.debits.Plain(exps[a.currency]), Currency: a.currency}
	a.manifest, _ = json.MarshalIndent(m, "", "  ") // strings, numbers and ids: it always marshals
	a.manifestSum = sha256.Sum256(a.manifest)
	return a, nil
}

// add counts the entry and its lines and its debits in the company's
// currency, and gives it in the file's form.
func (a *archive) add(x ledger.ArchivedEntry, exps currency.Exponents) (fileEntry, error) {
	out := fileEntry{EntryID: x.ID, Ledger: x.Ledger, DocumentType: x.DocumentType, DocumentNumber: x.DocumentNumber, FiscalYear: x.FiscalYear,
		FiscalPeriod: x.FiscalPeriod, DocumentDate: x.DocumentDate.Format(time.DateOnly), PostingDate: x.PostingDate.Format(time.DateOnly),
		Currency: x.Currency, LocalCurrency: x.LocalCurrency, GroupCurrency: x.GroupCurrency, RateToLocal: x.RateToLocal, RateToGroup: x.RateToGroup,
		Reference: x.Reference, HeaderText: x.HeaderText, SourceContext: x.SourceContext, SourceDocumentID: x.SourceDocumentID,
		ReversalOfID: x.ReversalOfID, ReversalOfYear: x.ReversalOfYear, PostedAt: x.PostedAt.UTC().Format(time.RFC3339Nano), PostedBy: x.PostedBy,
		Lines: make([]fileLine, len(x.Lines))}
	for i, l := range x.Lines {
		out.Lines[i] = fileLine{LineNo: l.LineNo, Account: l.Account, Amount: l.Amount.Plain(exps[x.Currency]), AmountLocal: l.AmountLocal.Plain(exps[x.LocalCurrency]),
			AmountGroup: l.AmountGroup.Plain(exps[x.GroupCurrency]), PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text}
		if l.AmountLocal > 0 {
			var ok bool
			if a.debits, ok = addDebit(a.debits, l.AmountLocal); !ok {
				return fileEntry{}, problem.Unprocessable("records.amount_too_large", "The year's debits add up to more than an amount holds.")
			}
		}
	}
	a.entries++
	a.lines += int32(len(x.Lines)) // #nosec G115 -- an entry's lines are numbered by an integer column
	return out, nil
}

// addDebit adds a debit to the sum, and reports false when the sum would
// be more than an amount holds.
func addDebit(sum, debit money.Minor) (money.Minor, bool) {
	if debit > math.MaxInt64-sum {
		return sum, false
	}
	return sum + debit, true
}
