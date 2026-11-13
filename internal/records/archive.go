package records

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// Archiving runs (ADR-0079 §6-§8, T99b): a company's journal entries of a
// fiscal year past retention and closed, queued in the request's
// transaction and written by the Archiver to an archive file in object
// storage with a manifest. Nothing is deleted.

// Statuses of an archiving run.
const (
	RunQueued    = "queued"
	RunCompleted = "completed"
	RunFailed    = "failed"
)

// Run is an archiving run. A completed run has its file's figures; a
// failed one its problem code.
type Run struct {
	ID, LegalEntity            uuid.UUID
	CompanyCode                string
	Kind                       string
	FiscalYear, RetentionYears int16
	ConfigVersion              int32
	Status                     string
	ObjectKey, ManifestKey     *string
	SHA256, ManifestSHA256     []byte
	SizeBytes                  *int64
	EntryCount, LineCount      *int32
	DebitTotal                 *money.Minor
	Currency                   *string
	FailureCode                *string
	CompletedAt                *time.Time
	RequestedBy                uuid.UUID
	CreatedAt, UpdatedAt       time.Time
	RequestedByName            string
	exp                        int // the minor-unit digits of Currency
}

const runSelect = `SELECT id, legal_entity_id, document_kind, fiscal_year, retention_years, config_version, status, object_key, manifest_key,
		sha256, manifest_sha256, size_bytes, entry_count, line_count, debit_total, currency, failure_code, completed_at, requested_by, created_at, updated_at
	FROM records.archiving_run`

func scanRun(row pgx.Row) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.LegalEntity, &r.Kind, &r.FiscalYear, &r.RetentionYears, &r.ConfigVersion, &r.Status, &r.ObjectKey, &r.ManifestKey,
		&r.SHA256, &r.ManifestSHA256, &r.SizeBytes, &r.EntryCount, &r.LineCount, &r.DebitTotal, &r.Currency, &r.FailureCode, &r.CompletedAt,
		&r.RequestedBy, &r.CreatedAt, &r.UpdatedAt)
	return r, errs.Wrapf(err, "records: scan archiving run")
}

func runNotFound() error { return problem.NotFoundOf("archiving run") }

// noObjects is the answer while the server has no object storage.
func noObjects() error {
	return problem.Unavailable("Object storage is not configured on this server; archive files cannot be written or read.", 0)
}

// RequestRun queues an archiving run of the company's documents of the
// kind in the fiscal year (ADR-0079 §6) and reports whether it is new: a
// run of the company, kind and year that is queued or completed already
// is answered instead, whatever the rule or the year's periods say now.
// The first version archives journal entries. A new run's year must be
// past retention, as the status reads it now, and closed.
func (e *Endpoints) RequestRun(ctx context.Context, legalEntity uuid.UUID, kind string, year int16) (Run, bool, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, false, err
	}
	if r, found, err := e.existing(ctx, tx, legalEntity, kind, year); err != nil || found {
		return r, false, err
	}
	s, err := e.archivable(ctx, legalEntity, kind, year)
	if err != nil {
		return Run{}, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO records.archiving_run (tenant_id, id, legal_entity_id, document_kind, fiscal_year, retention_years, config_version, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, legal_entity_id, document_kind, fiscal_year) WHERE status <> 'failed' DO NOTHING RETURNING id`,
		p.TenantID, uuid.Must(uuid.NewV7()), legalEntity, kind, year, *s.Years, s.Version, p.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) { // a run queued at the same moment answers
		r, _, err := e.existing(ctx, tx, legalEntity, kind, year)
		return r, false, err
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("records: queue the archiving run of %s %d: %w", s.Company.Code, year, err)
	}
	r, err := e.Run(ctx, id)
	if err != nil {
		return Run{}, false, err
	}
	return r, true, audit.Record(ctx, "records.archiving_run.request", "records.archiving_run", id, map[string]any{"legal_entity_id": legalEntity,
		"document_kind": kind, "fiscal_year": year, "retention_years": *s.Years, "config_version": s.Version})
}

// existing is the run of the company, kind and year that is queued or
// completed, if there is one and the caller may act in the company.
func (e *Endpoints) existing(ctx context.Context, tx pgx.Tx, legalEntity uuid.UUID, kind string, year int16) (Run, bool, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, false, fmt.Errorf("records: company access for an archiving run: %w", err)
	}
	if !access.Allows(legalEntity) {
		return Run{}, false, nil // archivable answers it as not found
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE legal_entity_id = $1 AND document_kind = $2 AND fiscal_year = $3 AND status <> 'failed'`,
		legalEntity, kind, year))
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, fmt.Errorf("records: the archiving run of %s %d: %w", legalEntity, year, err)
	}
	rs := []Run{r}
	if err := e.named(ctx, tx, rs); err != nil {
		return Run{}, false, err
	}
	return rs[0], true, nil
}

// archivable is the company's retention status of the kind once the
// fiscal year may be archived: the kind is one the first version
// archives, the year is past retention and closed, and the server can
// write the file.
func (e *Endpoints) archivable(ctx context.Context, legalEntity uuid.UUID, kind string, year int16) (Status, error) {
	if kind != config.RetentionJournalEntry {
		return Status{}, problem.Unprocessable("request.validation_failed", "The request is not valid: the first version archives journal_entry.",
			problem.FieldError{Field: "document_kind", Code: "invalid", Message: "journal_entry"})
	}
	if e.Objects == nil {
		return Status{}, noObjects()
	}
	s, err := e.StatusOf(ctx, legalEntity, kind)
	if err != nil {
		return Status{}, err
	}
	if !pastRetention(s, year) {
		return Status{}, problem.Unprocessable("records.year_not_past_retention", fmt.Sprintf("Fiscal year %d of company %s is not past retention of %s "+
			"under the active configuration's rule, on the company's date %s; see GET /v1/retention-status.", year, s.Company.Code, kind, s.AsOf.Format(time.DateOnly)))
	}
	closed, err := e.Journal.YearClosed(ctx, legalEntity, year)
	if err != nil {
		return Status{}, fmt.Errorf("records: is fiscal year %d of company %s closed: %w", year, s.Company.Code, err)
	}
	if !closed {
		return Status{}, yearNotClosed(s.Company.Code, year)
	}
	return s, nil
}

// pastRetention tells whether the status lists the fiscal year past
// retention.
func pastRetention(s Status, year int16) bool {
	for _, y := range s.Fiscal {
		if y.FiscalYear == year {
			return y.Past
		}
	}
	return false
}

// yearNotClosed refuses a fiscal year with an open period.
func yearNotClosed(company string, year int16) *problem.Problem {
	return problem.Unprocessable("records.year_not_closed", fmt.Sprintf("Fiscal year %d of company %s is not closed: it has an open posting period, "+
		"or none; a year is archived once every period of it, in every ledger, is closed or locked.", year, company))
}

// Run is an archiving run of a legal entity the caller may act in; any
// other is not found.
func (e *Endpoints) Run(ctx context.Context, id uuid.UUID) (Run, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Run{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Run{}, err
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE id = $1`, id))
	if err = authz.Visible(err, access, r.LegalEntity, runNotFound(), "records: archiving run %s", id); err != nil {
		return Run{}, err
	}
	rs := []Run{r}
	if err := e.named(ctx, tx, rs); err != nil {
		return Run{}, err
	}
	return rs[0], nil
}

// RunsQuery filters the archiving runs listed.
type RunsQuery struct {
	LegalEntity  *uuid.UUID
	Kind, Status string
	FiscalYear   *int32
	Page         pagination.Request
}

var runSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
}}

// Runs lists a page of the archiving runs of the legal entities the
// caller may act in, newest first, and the next page's token ("" on the
// last).
func (e *Endpoints) Runs(ctx context.Context, q RunsQuery) ([]Run, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	year := ""
	if q.FiscalYear != nil {
		year = fmt.Sprint(*q.FiscalYear)
	}
	ks, err := e.Pages.Keyset(runSort, fmt.Sprintf("listArchivingRuns|%s|%s|%s|%s", q.LegalEntity, q.Kind, q.Status, year), q.Page)
	if err != nil {
		return nil, "", err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, "", err
	}
	query, args := ks.SQL(runSelect+` WHERE ($1 OR legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR legal_entity_id = $3)
		AND ($4 = '' OR document_kind = $4) AND ($5 = '' OR status = $5) AND ($6::int IS NULL OR fiscal_year = $6)`,
		[]any{access.All, access.IDs, q.LegalEntity, q.Kind, q.Status, q.FiscalYear})
	rows, _ := tx.Query(ctx, query, args...)
	rs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
	if err != nil {
		return nil, "", fmt.Errorf("records: list archiving runs: %w", err)
	}
	rs, next := pagination.Page(ks, rs, func(r Run) (string, uuid.UUID) { return r.CreatedAt.Format(time.RFC3339Nano), r.ID })
	return rs, next, e.named(ctx, tx, rs)
}

// named gives each run its company's code, each read once, its
// requester's name and the digits of its currency.
func (e *Endpoints) named(ctx context.Context, tx pgx.Tx, rs []Run) error {
	var currencies []string
	requesters := make([]uuid.UUID, len(rs))
	for i, r := range rs {
		requesters[i] = r.RequestedBy
		if r.Currency != nil {
			currencies = append(currencies, *r.Currency)
		}
	}
	names, err := e.People.PersonNames(ctx, requesters)
	if err != nil {
		return fmt.Errorf("records: requesters of archiving runs: %w", err)
	}
	exps, err := currency.LoadCodes(ctx, tx, currencies...)
	if err != nil {
		return fmt.Errorf("records: archiving runs: %w", err)
	}
	codes := map[uuid.UUID]string{}
	for i, r := range rs {
		if _, ok := codes[r.LegalEntity]; !ok {
			le, found, err := e.LegalEntities.Entity(ctx, r.LegalEntity)
			if err != nil {
				return fmt.Errorf("records: company of archiving run %s: %w", r.ID, err)
			}
			if !found {
				return fmt.Errorf("records: company %s of archiving run %s is not held by the tenancy context", r.LegalEntity, r.ID)
			}
			codes[r.LegalEntity] = le.Code
		}
		rs[i].CompanyCode, rs[i].RequestedByName = codes[r.LegalEntity], names[r.RequestedBy]
		if r.Currency != nil {
			rs[i].exp = exps[*r.Currency]
		}
	}
	return nil
}

// File is a completed run's archive file, as the store holds it, once its
// hash is the one the run recorded.
func (e *Endpoints) File(ctx context.Context, id uuid.UUID) ([]byte, error) {
	r, err := e.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status != RunCompleted {
		return nil, problem.Conflict("records.archive_not_completed", "Archiving run "+id.String()+" is "+r.Status+"; only a completed run has a file.")
	}
	if e.Objects == nil {
		return nil, noObjects()
	}
	content, _, err := e.Objects.Get(ctx, *r.ObjectKey)
	if err != nil {
		return nil, problem.Unavailable("The file store did not return the archive file; try again.", 30).WithCause(err)
	}
	if sum := sha256.Sum256(content); !bytes.Equal(sum[:], r.SHA256) {
		return nil, archiveChanged(*r.ObjectKey)
	}
	return content, nil
}

// archiveChanged refuses a stored file that is not the run's.
func archiveChanged(key string) *problem.Problem {
	return problem.Conflict("records.archive_changed", "The file store holds another file than the archiving run's under "+key+"; it is not used.")
}

// hexOf is a hash in hexadecimal.
func hexOf(b []byte) string { return hex.EncodeToString(b) }
