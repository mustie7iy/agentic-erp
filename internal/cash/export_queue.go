package cash

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// Exports of snapshots to treasury systems (ADR-0051 §14, T53h): queued
// in the transaction that takes a snapshot, one per active treasury
// channel naming its company, or when a person asks for one; sent by the
// Exporter, which records each outcome.

// Statuses of an export.
const (
	ExportPending   = "pending"
	ExportDelivered = "delivered"
	ExportFailed    = "failed"
)

// TreasuryChannels are the integration context's treasury_export
// connectors, as the application wires them: a leaf reads them through
// this port.
type TreasuryChannels interface {
	// ForCompany are the active channels that export the company, by name.
	ForCompany(ctx context.Context, code string) ([]TreasuryChannel, error)
	// Channel is the channel of the id, active or not; found is false
	// when the id names no treasury_export connector.
	Channel(ctx context.Context, id uuid.UUID) (ch TreasuryChannel, found bool, err error)
	// Endpoint resolves a channel into the SFTP endpoint its files are
	// uploaded to, its private key included; a channel that cannot be
	// used is a problem.
	Endpoint(ctx context.Context, id uuid.UUID) (sftpclient.Endpoint, error)
}

// TreasuryChannel is a treasury_export connector.
type TreasuryChannel struct {
	ID        uuid.UUID
	Name      string
	Active    bool
	Format    string   // jsonl or csv
	Companies []string // the company codes it exports
}

// Export is an export of a snapshot to a treasury channel.
type Export struct {
	ID, Snapshot, LegalEntity, Connector uuid.UUID
	CompanyCode, ConnectorName           string
	SnapshotDate                         time.Time
	Format, Status                       string
	Attempts                             int
	NextAttemptAt                        *time.Time
	FileName                             *string
	SHA256                               []byte
	Resend                               *bool
	LastError                            *string
	DeliveredAt                          *time.Time
	RequestedBy                          uuid.UUID
	CreatedAt, UpdatedAt                 time.Time
}

const exportSelect = `SELECT x.id, x.snapshot_id, x.legal_entity_id, s.snapshot_date, x.connector_id, x.format, x.status, x.attempts, x.next_attempt_at,
		x.file_name, x.sha256, x.resend, x.last_error, x.delivered_at, x.requested_by, x.created_at, x.updated_at
	FROM cash.export x JOIN cash.snapshot s ON s.tenant_id = x.tenant_id AND s.id = x.snapshot_id`

func scanExport(row pgx.Row) (Export, error) {
	var x Export
	err := row.Scan(&x.ID, &x.Snapshot, &x.LegalEntity, &x.SnapshotDate, &x.Connector, &x.Format, &x.Status, &x.Attempts, &x.NextAttemptAt,
		&x.FileName, &x.SHA256, &x.Resend, &x.LastError, &x.DeliveredAt, &x.RequestedBy, &x.CreatedAt, &x.UpdatedAt)
	return x, errs.Wrapf(err, "scan cash export")
}

// SHA256Hex is the file's hash in hexadecimal; "" before it is first sent.
func (x Export) SHA256Hex() string { return hex.EncodeToString(x.SHA256) }

// queueExports queues the snapshot's export to every active treasury
// channel naming its company, each audited, in the transaction that takes
// it.
func (e *Endpoints) queueExports(ctx context.Context, tx pgx.Tx, p reqctx.Principal, snapshot, legalEntity uuid.UUID, company string) error {
	chans, err := e.Treasury.ForCompany(ctx, company)
	if err != nil {
		return fmt.Errorf("cash: treasury channels of %s: %w", company, err)
	}
	for _, ch := range chans {
		if _, err := e.queueExport(ctx, tx, p, snapshot, legalEntity, ch, true); err != nil {
			return err
		}
	}
	return nil
}

// queueExport queues one export, due now, and audits it.
func (e *Endpoints) queueExport(ctx context.Context, tx pgx.Tx, p reqctx.Principal, snapshot, legalEntity uuid.UUID, ch TreasuryChannel, taken bool) (uuid.UUID, error) {
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO cash.export (tenant_id, id, snapshot_id, legal_entity_id, connector_id, format, status, next_attempt_at, requested_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8)`, p.TenantID, id, snapshot, legalEntity, ch.ID, ch.Format, clock.Now(e.Now), p.ID); err != nil {
		return uuid.Nil, fmt.Errorf("cash: queue the export of snapshot %s to %s: %w", snapshot, ch.Name, err)
	}
	return id, audit.Record(ctx, "cash.export.request", "cash.export", id, map[string]any{"cash_snapshot_id": snapshot, "legal_entity_id": legalEntity,
		"connector_id": ch.ID, "connector_name": ch.Name, "format": ch.Format, "with_snapshot": taken})
}

// RequestExport queues the snapshot's export to the treasury channel: an
// active treasury_export connector naming the snapshot's company. A
// snapshot of a legal entity the caller cannot act in is not found.
func (e *Endpoints) RequestExport(ctx context.Context, snapshot, connector uuid.UUID) (Export, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Export{}, err
	}
	s, err := e.Snapshot(ctx, snapshot)
	if err != nil {
		return Export{}, err
	}
	ch, found, err := e.Treasury.Channel(ctx, connector)
	if err != nil {
		return Export{}, fmt.Errorf("cash: treasury channel %s: %w", connector, err)
	}
	if !found || !ch.Active || !slices.Contains(ch.Companies, s.CompanyCode) {
		return Export{}, problem.Unprocessable("cash.export_channel_invalid",
			"The connector is not an active treasury_export connector whose companies name "+s.CompanyCode+".",
			problem.FieldError{Field: "connector_id", Code: "invalid", Message: "an active treasury_export connector exporting company " + s.CompanyCode})
	}
	id, err := e.queueExport(ctx, tx, p, s.ID, s.LegalEntity, ch, false)
	if err != nil {
		return Export{}, err
	}
	return e.Export(ctx, id)
}

func exportNotFound() error { return problem.NotFoundOf("cash export") }

// Export is an export of a legal entity the caller may act in; any other
// is not found.
func (e *Endpoints) Export(ctx context.Context, id uuid.UUID) (Export, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return Export{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return Export{}, err
	}
	x, err := scanExport(tx.QueryRow(ctx, exportSelect+` WHERE x.id = $1`, id))
	if err = authz.Visible(err, access, x.LegalEntity, exportNotFound(), "cash: export %s", id); err != nil {
		return Export{}, err
	}
	xs := []Export{x}
	if err := e.named(ctx, xs); err != nil {
		return Export{}, err
	}
	return xs[0], nil
}

// ExportsQuery filters the exports listed.
type ExportsQuery struct {
	Snapshot, LegalEntity *uuid.UUID
	Status                string
	Page                  pagination.Request
}

var exportSort = pagination.Sort{Default: "created_at", DefaultDesc: true, Fields: []pagination.Field{
	{Name: "created_at", Column: "x.created_at", Type: "timestamptz"},
}}

// Exports lists a page of the exports of the legal entities the caller
// may act in, newest first, and the next page's token ("" on the last).
func (e *Endpoints) Exports(ctx context.Context, q ExportsQuery) ([]Export, string, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, "", err
	}
	ks, err := e.Pages.Keyset(exportSort, fmt.Sprintf("listCashExports|%s|%s|%s", q.Snapshot, q.LegalEntity, q.Status), q.Page)
	if err != nil {
		return nil, "", err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, "", err
	}
	query, args := ks.SQL(exportSelect+` WHERE ($1 OR x.legal_entity_id = ANY($2)) AND ($3::uuid IS NULL OR x.snapshot_id = $3)
		AND ($4::uuid IS NULL OR x.legal_entity_id = $4) AND ($5 = '' OR x.status = $5)`, []any{access.All, access.IDs, q.Snapshot, q.LegalEntity, q.Status})
	rows, _ := tx.Query(ctx, query, args...)
	xs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Export, error) { return scanExport(r) })
	if err != nil {
		return nil, "", fmt.Errorf("cash: list exports: %w", err)
	}
	xs, next := pagination.Page(ks, xs, func(x Export) (string, uuid.UUID) { return x.CreatedAt.Format(time.RFC3339Nano), x.ID })
	return xs, next, e.named(ctx, xs)
}

// named gives each export its company's code and its channel's name, each
// read once.
func (e *Endpoints) named(ctx context.Context, xs []Export) error {
	entities := make([]uuid.UUID, len(xs))
	for i, x := range xs {
		entities[i] = x.LegalEntity
	}
	codes, err := e.companyCodes(ctx, entities)
	if err != nil {
		return fmt.Errorf("cash: exports: %w", err)
	}
	names := map[uuid.UUID]string{}
	for i, x := range xs {
		xs[i].CompanyCode = codes[x.LegalEntity]
		if _, ok := names[x.Connector]; !ok {
			ch, found, err := e.Treasury.Channel(ctx, x.Connector)
			if err != nil {
				return fmt.Errorf("cash: treasury channel %s: %w", x.Connector, err)
			}
			if !found {
				return fmt.Errorf("cash: treasury channel %s is not held by the integration context", x.Connector)
			}
			names[x.Connector] = ch.Name
		}
		xs[i].ConnectorName = names[x.Connector]
	}
	return nil
}
