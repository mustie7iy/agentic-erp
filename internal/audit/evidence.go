package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/approvals"
	"github.com/mustie7iy/agentic-erp/internal/ledger"
	trail "github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Audit evidence exports (T101a, ADR-0011 As built): an auditor asks for
// the evidence of companies and a period; the request is queued in its
// transaction and the EvidenceExporter writes the files and their
// manifest to object storage (ADR-0016).

// LegalEntities reads the tenant's legal entities (tenancy context).
type LegalEntities interface {
	Entity(ctx context.Context, id uuid.UUID) (tenancy.Entity, bool, error)
}

// Journal reads a company's journal entries posted in a period, of every
// ledger, with their lines (ledger context, ledger.Archive).
type Journal interface {
	EntriesBetween(ctx context.Context, legalEntity uuid.UUID, from, to time.Time, fn func(ledger.ArchivedEntry) error) error
	LinesBetween(ctx context.Context, legalEntity uuid.UUID, from, to time.Time) (int, error)
}

// Approvals reads the approval requests about documents with their
// decisions (approvals, approvals.Evidence).
type Approvals interface {
	About(ctx context.Context, objectIDs []uuid.UUID) ([]approvals.Request, error)
}

// People names principals (identity context).
type People interface {
	PersonNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// Requesters tells whether the person who asked for an export may still
// act (identity context, identity.Actors): one blocked or deprovisioned
// since is refused with a business problem (PO 2026-11-03).
type Requesters interface {
	MayAct(ctx context.Context, tx pgx.Tx, id uuid.UUID) error
}

// Statuses of an evidence export.
const (
	ExportQueued    = "queued"
	ExportCompleted = "completed"
	ExportFailed    = "failed"
)

// maxPeriodDays is the most days an export's period ends after its first:
// the longest fiscal year, 53 weeks, as the database checks it.
const maxPeriodDays = 370

// Company is a company an export covers.
type Company struct {
	ID   uuid.UUID
	Code string
}

// ExportFile is a file of a completed export, as its manifest names it.
type ExportFile struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Records   int32  `json:"records"`
}

// Export is an audit evidence export. A completed one has its manifest's
// figures and the chain's verification; a failed one its problem code.
type Export struct {
	ID                                         uuid.UUID
	LegalEntities                              []uuid.UUID
	Companies                                  []Company
	From, To                                   time.Time
	WithClientAddresses                        bool
	Status                                     string
	ManifestKey                                *string
	ManifestSHA256                             []byte
	Files                                      []ExportFile
	AuditEvents, JournalEntries, Decisions     *int32
	ChainVerified                              *bool
	ChainLastSeq, ChainChecked, ChainBrokenSeq *int64
	AnchorsChecked                             *int32
	FailureCode                                *string
	CompletedAt                                *time.Time
	RequestedBy                                uuid.UUID
	RequestedByName                            string
	CreatedAt, UpdatedAt                       time.Time
}

const exportSelect = `SELECT id, legal_entity_ids, period_from, period_to, with_client_addresses, status, manifest_key, manifest_sha256, files,
		audit_event_count, journal_entry_count, approval_decision_count, chain_verified, chain_last_seq, chain_checked, chain_anchors_checked,
		chain_broken_seq, failure_code, completed_at, requested_by, created_at, updated_at
	FROM audit.evidence_export`

func scanExport(row pgx.Row) (Export, error) {
	var x Export
	err := row.Scan(&x.ID, &x.LegalEntities, &x.From, &x.To, &x.WithClientAddresses, &x.Status, &x.ManifestKey, &x.ManifestSHA256, &x.Files,
		&x.AuditEvents, &x.JournalEntries, &x.Decisions, &x.ChainVerified, &x.ChainLastSeq, &x.ChainChecked, &x.AnchorsChecked,
		&x.ChainBrokenSeq, &x.FailureCode, &x.CompletedAt, &x.RequestedBy, &x.CreatedAt, &x.UpdatedAt)
	return x, errs.Wrapf(err, "audit: scan evidence export")
}

func exportNotFound() error { return problem.NotFoundOf("audit evidence export") }

// noObjects is the answer while the server has no object storage.
func noObjects() error {
	return problem.Unavailable("Object storage is not configured on this server; evidence exports cannot be written or read.", 0)
}

// RequestExport queues an evidence export of the companies, among those
// asked, that the caller may act in, for the period from..to (dates,
// both included). The audit events carry their client addresses when the
// caller is a person who may read them on the trail (audit.event.read).
func (e *Endpoints) RequestExport(ctx context.Context, asked []uuid.UUID, from, to time.Time) (Export, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Export{}, err
	}
	if err := checkPeriod(from, to); err != nil {
		return Export{}, err
	}
	if e.Objects == nil {
		return Export{}, noObjects()
	}
	covered, err := e.covered(ctx, asked)
	if err != nil {
		return Export{}, err
	}
	addresses, err := authz.MayUse(ctx, "audit.event.read")
	if err != nil {
		return Export{}, fmt.Errorf("audit: may the requester read client addresses: %w", err)
	}
	addresses = addresses && !reqctx.Masked(ctx)
	ids := make([]uuid.UUID, len(covered))
	for i, c := range covered {
		ids[i] = c.ID
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO audit.evidence_export (tenant_id, id, legal_entity_ids, period_from, period_to, with_client_addresses, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, p.TenantID, id, ids, from, to, addresses, p.ID); err != nil {
		return Export{}, fmt.Errorf("audit: queue an evidence export: %w", err)
	}
	x, err := e.Export(ctx, id)
	if err != nil {
		return Export{}, err
	}
	return x, trail.Record(ctx, "audit.evidence_export.request", "audit.evidence_export", id, map[string]any{"legal_entity_ids": ids,
		"from": from.Format(time.DateOnly), "to": to.Format(time.DateOnly), "with_client_addresses": addresses})
}

// checkPeriod refuses a period that ends before it starts or is longer
// than the longest fiscal year.
func checkPeriod(from, to time.Time) error {
	if to.Before(from) || to.Sub(from) > maxPeriodDays*24*time.Hour {
		return problem.Unprocessable("request.validation_failed", "The request is not valid: the period's last day is not before its first and at most "+
			"370 days after it.", problem.FieldError{Field: "to", Code: "out_of_range", Message: "from <= to <= from + 370 days"})
	}
	return nil
}

// covered is the companies among those asked that exist and the caller
// may act in, by code; none is refused, without telling which of the two
// kept each out, so ids of other companies cannot be probed.
func (e *Endpoints) covered(ctx context.Context, asked []uuid.UUID) ([]Company, error) {
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, fmt.Errorf("audit: company access for an evidence export: %w", err)
	}
	var out []Company
	for _, id := range asked {
		if !access.Allows(id) { // the contract keeps the ids unique
			continue
		}
		le, found, err := e.LegalEntities.Entity(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("audit: company %s of an evidence export: %w", id, err)
		}
		if found {
			out = append(out, Company{ID: id, Code: le.Code})
		}
	}
	if len(out) == 0 {
		return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: none of the companies asked for is one you may act in.",
			problem.FieldError{Field: "legal_entity_ids", Code: "not_found", Message: "a company you may act in"})
	}
	slices.SortFunc(out, func(a, b Company) int { return strings.Compare(a.Code, b.Code) })
	return out, nil
}

// visibleTo tells whether the caller may act in every company of the
// export.
func visibleTo(access authz.Access, x Export) bool {
	for _, id := range x.LegalEntities {
		if !access.Allows(id) {
			return false
		}
	}
	return true
}

// Export is an evidence export whose every company the caller may act
// in; any other is not found.
func (e *Endpoints) Export(ctx context.Context, id uuid.UUID) (Export, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Export{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Export{}, fmt.Errorf("audit: company access for evidence export %s: %w", id, err)
	}
	x, err := scanExport(tx.QueryRow(ctx, exportSelect+` WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !visibleTo(access, x)) {
		return Export{}, exportNotFound()
	}
	if err != nil {
		return Export{}, fmt.Errorf("audit: evidence export %s: %w", id, err)
	}
	xs := []Export{x}
	if err := e.named(ctx, xs); err != nil {
		return Export{}, err
	}
	return xs[0], nil
}

// ExportsQuery filters the evidence exports listed.
type ExportsQuery struct {
	LegalEntity *uuid.UUID
	Status      string
	Page        pagination.Request
}

var exportSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "created_at", Type: "timestamptz"},
}}

// Exports lists a page of the evidence exports whose every company the
// caller may act in, newest first, and the next page's token ("" on the
// last).
func (e *Endpoints) Exports(ctx context.Context, q ExportsQuery) ([]Export, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	ks, err := e.Pages.Keyset(exportSort, fmt.Sprintf("listAuditEvidenceExports|%s|%s", q.LegalEntity, q.Status), q.Page)
	if err != nil {
		return nil, "", err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("audit: company access for evidence exports: %w", err)
	}
	query, args := ks.SQL(exportSelect+` WHERE ($1 OR legal_entity_ids <@ $2::uuid[]) AND ($3::uuid IS NULL OR $3 = ANY(legal_entity_ids))
		AND ($4 = '' OR status = $4)`, []any{access.All, access.IDs, q.LegalEntity, q.Status})
	rows, _ := tx.Query(ctx, query, args...)
	xs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Export, error) { return scanExport(r) })
	if err != nil {
		return nil, "", fmt.Errorf("audit: list evidence exports: %w", err)
	}
	xs, next := pagination.Page(ks, xs, func(x Export) (string, uuid.UUID) { return x.CreatedAt.Format(time.RFC3339Nano), x.ID })
	return xs, next, e.named(ctx, xs)
}

// named gives each export its companies' codes, each read once, and its
// requester's name.
func (e *Endpoints) named(ctx context.Context, xs []Export) error {
	requesters := make([]uuid.UUID, len(xs))
	for i, x := range xs {
		requesters[i] = x.RequestedBy
	}
	names, err := e.People.PersonNames(ctx, requesters)
	if err != nil {
		return fmt.Errorf("audit: requesters of evidence exports: %w", err)
	}
	codes := map[uuid.UUID]string{}
	for i := range xs {
		x := &xs[i]
		x.RequestedByName = names[x.RequestedBy]
		x.Companies = make([]Company, 0, len(x.LegalEntities))
		for _, id := range x.LegalEntities {
			if _, ok := codes[id]; !ok {
				le, found, err := e.LegalEntities.Entity(ctx, id)
				if err != nil {
					return fmt.Errorf("audit: company of evidence export %s: %w", x.ID, err)
				}
				if !found {
					return fmt.Errorf("audit: company %s of evidence export %s is not held by the tenancy context", id, x.ID)
				}
				codes[id] = le.Code
			}
			x.Companies = append(x.Companies, Company{ID: id, Code: codes[id]})
		}
	}
	return nil
}

// File is a file of a completed export as the store holds it, once its
// hash is the one recorded: the manifest's the export's, any other file's
// the manifest's. The audit events of an export with client addresses
// are handed only to a person who may read them on the trail.
func (e *Endpoints) File(ctx context.Context, id uuid.UUID, name string) ([]byte, error) {
	_, p, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	x, err := e.Export(ctx, id)
	if err != nil {
		return nil, err
	}
	if name != manifestName && !slices.Contains(fileNames, name) {
		return nil, problem.NotFound("An evidence export has no file " + name + "; its files are " + manifestName + " and " + strings.Join(fileNames, ", ") + ".")
	}
	if x.Status != ExportCompleted {
		return nil, problem.Conflict("audit.evidence_not_completed", "Evidence export "+id.String()+" is "+x.Status+"; only a completed export has files.")
	}
	if e.Objects == nil {
		return nil, noObjects()
	}
	if name == auditEventsName && x.WithClientAddresses {
		if err := mayReadAddresses(ctx); err != nil {
			return nil, err
		}
	}
	m, err := e.manifestOf(ctx, x)
	if err != nil || name == manifestName {
		return m, err
	}
	sum, err := fileSum(m, name)
	if err != nil {
		return nil, err
	}
	return e.stored(ctx, blobstore.Key(p.TenantID, blobstore.KindEvidence, fileKey(x.ID, name, sum)), sum)
}

// mayReadAddresses refuses a caller who may not read client addresses on
// the trail.
func mayReadAddresses(ctx context.Context) error {
	may, err := authz.MayUse(ctx, "audit.event.read")
	if err != nil {
		return fmt.Errorf("audit: may the caller read client addresses: %w", err)
	}
	if !may || reqctx.Masked(ctx) {
		return problem.Forbidden("audit.client_addresses_withheld", "The export's audit events hold people's client addresses, which only a person "+
			"holding audit.event.read reads.")
	}
	return nil
}

// manifestOf is the export's manifest as the store holds it, once its
// hash is the export's.
func (e *Endpoints) manifestOf(ctx context.Context, x Export) ([]byte, error) {
	var sum [32]byte
	copy(sum[:], x.ManifestSHA256)
	return e.stored(ctx, *x.ManifestKey, sum)
}

// stored is the object under the key once its SHA-256 is sum.
func (e *Endpoints) stored(ctx context.Context, key string, sum [32]byte) ([]byte, error) {
	content, _, err := e.Objects.Get(ctx, key)
	if err != nil {
		return nil, problem.Unavailable("The file store did not return the evidence file; try again.", 30).WithCause(err)
	}
	if got := sha256.Sum256(content); !bytes.Equal(got[:], sum[:]) {
		return nil, evidenceChanged(key)
	}
	return content, nil
}

// evidenceChanged refuses stored bytes that are not the export's.
func evidenceChanged(key string) *problem.Problem {
	return problem.Conflict("audit.evidence_changed", "The file store holds other bytes than the evidence export's under "+key+"; they are not used.")
}

// hexOf is a hash in hexadecimal.
func hexOf(b []byte) string { return hex.EncodeToString(b) }
