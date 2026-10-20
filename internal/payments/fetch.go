package payments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// Statements fetched from a house bank's SFTP channel (ADR-0047, T49d): a
// workflow per bank lists the channel's statements directory, imports each
// file through the upload's path (importFile, so the same idempotency), and
// moves it to the archive directory once the import committed. What became
// of each file is recorded once per house bank and content
// (payments.statement_fetch): a file imported but not yet moved when a
// fetch was cut short is moved by the next one and never imported twice,
// and a file refused is refused once, with BankStatementFileRefused, and
// left for a person.

// Statement fetch statuses; FetchSkipped is not recorded.
const (
	FetchImported = "imported"
	FetchArchived = "archived"
	FetchRefused  = "refused"
	FetchSkipped  = "skipped" // a statement of another house bank's account, left for that bank
)

// FetcherActor is the principal of statement fetches, in every tenant.
var FetcherActor = uuid.NewSHA1(uuid.NameSpaceURL, []byte("urn:agentic-erp:bank-statement-fetcher"))

// Fetcher is the principal a fetch acts as in the tenant.
func Fetcher(tenant uuid.UUID) reqctx.Principal {
	return reqctx.Principal{ID: FetcherActor, Kind: reqctx.KindSystem, TenantID: tenant}
}

// ChannelBank is an active house bank with a bank channel, across tenants.
type ChannelBank struct {
	TenantID, HouseBankID, ConnectorID uuid.UUID
}

// ChannelBanks lists the active house banks with a bank channel, as the
// worker role reads them across tenants: the dispatcher starts a fetch for
// those whose channel names statement directories.
func ChannelBanks(ctx context.Context, tx pgx.Tx) ([]ChannelBank, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id, connector_id FROM payments.house_bank WHERE status = 'active' AND connector_id IS NOT NULL ORDER BY tenant_id, id`)
	banks, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ChannelBank])
	if err != nil {
		return nil, fmt.Errorf("payments: list the house banks with a channel: %w", err)
	}
	return banks, nil
}

// StatementChannel resolves the house bank's channel for a fetch, failing
// closed as a transmission does (the key is resolved on each use): a bank
// that is not active, has no channel, or whose channel names no statement
// directories is refused.
func (e *Endpoints) StatementChannel(ctx context.Context, houseBank uuid.UUID) (sftpclient.Endpoint, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	none := problem.Conflict("payments.no_statement_channel", "House bank "+houseBank.String()+
		" is not active or has no bank channel with statements_directory and statements_archive.")
	var connector *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT connector_id FROM payments.house_bank WHERE id = $1 AND status = 'active'`, houseBank).Scan(&connector)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && connector == nil) {
		return sftpclient.Endpoint{}, none
	}
	if err != nil {
		return sftpclient.Endpoint{}, fmt.Errorf("payments: the channel of house bank %s: %w", houseBank, err)
	}
	ep, err := e.Channels.Endpoint(ctx, *connector)
	if err != nil {
		return sftpclient.Endpoint{}, fmt.Errorf("payments: the channel of house bank %s: %w", houseBank, err)
	}
	if ep.Inbound == "" {
		return sftpclient.Endpoint{}, none
	}
	return ep, nil
}

// StatementPaths are where a fetched file is in the statements directory
// and where it is archived: under its name prefixed with its hash, so a
// file the bank delivers again under the name of an archived one is told
// apart by content, and the same content is the same target.
func StatementPaths(ep sftpclient.Endpoint, name string, sum []byte) (from, to string) {
	return path.Join(ep.Inbound, name), path.Join(ep.Archive, sha256Hex(sum)[:16]+"-"+name)
}

// Fetched is what became of a fetched file.
type Fetched struct {
	SHA256 []byte `json:"sha256"`
	Status string `json:"status"`
}

// ImportFetched imports a file fetched from the house bank's channel, once
// per content: a file fetched before keeps what became of it (imported,
// archived, refused), and a new one is imported as an upload is and
// recorded imported. A refusal is returned for RefuseFetched to record, in
// a transaction of its own, since the import's rolls back.
func (e *Endpoints) ImportFetched(ctx context.Context, houseBank uuid.UUID, name string, file []byte) (Fetched, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Fetched{}, err
	}
	digest := sha256.Sum256(file)
	f := Fetched{SHA256: digest[:], Status: FetchImported}
	// Overlapping fetches of the bank wait here, so the second finds the
	// first's record rather than recording the file again.
	if err := lockStatements(ctx, tx, houseBank); err != nil {
		return Fetched{}, err
	}
	err = tx.QueryRow(ctx, `SELECT status FROM payments.statement_fetch WHERE house_bank_id = $1 AND file_sha256 = $2`, houseBank, f.SHA256).Scan(&f.Status)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Fetched{}, fmt.Errorf("payments: the fetch of %s for house bank %s: %w", name, houseBank, err)
	}
	bank, err := e.statementBank(ctx, tx, p.TenantID, houseBank, false)
	if err != nil {
		return Fetched{}, err
	}
	if _, _, _, err := e.importFile(ctx, tx, p, bank, statementFormat(file), file); err != nil {
		return Fetched{}, err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err := tx.Exec(ctx, `INSERT INTO payments.statement_fetch (tenant_id, id, house_bank_id, file_sha256, file_name, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'imported', $6, $6)`, p.TenantID, id, houseBank, f.SHA256, name, p.ID); err != nil {
		return Fetched{}, fmt.Errorf("payments: record the fetch of %s for house bank %s: %w", name, houseBank, err)
	}
	return f, record(ctx, "payments.statement_fetch.import", id, map[string]any{"house_bank_id": houseBank, "file_name": name, "file_sha256": sha256Hex(f.SHA256)})
}

// statementFormat tells the file's format by its content: camt.053 is XML,
// MT940 is not. A file that is neither is refused by the parser.
func statementFormat(file []byte) string {
	if bytes.HasPrefix(bytes.TrimLeft(bytes.TrimPrefix(file, []byte("\xef\xbb\xbf")), " \t\r\n"), []byte("<")) {
		return "camt.053"
	}
	return "mt940"
}

// RefuseFetched records that the import refused the fetched file, once:
// the first refusal publishes BankStatementFileRefused, and a fetch that
// runs again finds it recorded and publishes nothing.
func RefuseFetched(ctx context.Context, houseBank uuid.UUID, name string, sum []byte, refusal *problem.Problem) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	reason := refusal.Detail
	for _, fe := range refusal.Errors {
		reason += " " + fe.Message
	}
	var id, legalEntity uuid.UUID
	err = tx.QueryRow(ctx, `INSERT INTO payments.statement_fetch (tenant_id, id, house_bank_id, file_sha256, file_name, status, reason, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, 'refused', $6, $7, $7) ON CONFLICT (tenant_id, house_bank_id, file_sha256) DO NOTHING
		RETURNING id, (SELECT legal_entity_id FROM payments.house_bank WHERE id = $3)`, p.TenantID, uuid.Must(uuid.NewV7()), houseBank, sum, name, reason, p.ID).
		Scan(&id, &legalEntity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // refused before
	}
	if err != nil {
		return fmt.Errorf("payments: record the refusal of %s for house bank %s: %w", name, houseBank, err)
	}
	return record(ctx, "payments.statement_fetch.refuse", id, map[string]any{"house_bank_id": houseBank, "file_name": name, "code": refusal.Code, "reason": reason},
		outbox.Event{Context: "payments", Name: "bank_statement_file_refused", AggregateType: "payments.statement_fetch", AggregateID: id,
			Payload: map[string]any{"statement_fetch_id": id, "house_bank_id": houseBank, "legal_entity_id": legalEntity, "file_name": name,
				"file_sha256": sha256Hex(sum), "code": refusal.Code, "reason": reason}})
}

// ArchivedFetched records that the imported file was moved to the archive
// directory; a file recorded archived before is left as it is.
func ArchivedFetched(ctx context.Context, houseBank uuid.UUID, sum []byte) error {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return err
	}
	var id uuid.UUID
	var name string
	err = tx.QueryRow(ctx, `UPDATE payments.statement_fetch SET status = 'archived', version = version + 1, updated_by = $3
		WHERE house_bank_id = $1 AND file_sha256 = $2 AND status = 'imported' RETURNING id, file_name`, houseBank, sum, p.ID).Scan(&id, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // archived before
	}
	if err != nil {
		return fmt.Errorf("payments: record the archiving of file %x for house bank %s: %w", sum, houseBank, err)
	}
	return record(ctx, "payments.statement_fetch.archive", id, map[string]any{"house_bank_id": houseBank, "file_name": name})
}
