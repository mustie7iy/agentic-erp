package integration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/secrets"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

// BankChannels are the bank connectors house banks transmit through
// (ADR-0038, T44b), published to the payments context, which reaches them
// through ports the application wires, so neither context imports the
// other. Only bank_sftp is a channel until EBICS is certified (T44d).
type BankChannels struct {
	Secrets secrets.Resolver // resolves a channel's private key
}

// ChannelByName finds the bank channel of that name: a connector of a kind
// the server transmits through. Any other connector is not one.
func (BankChannels) ChannelByName(ctx context.Context, name string) (uuid.UUID, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM integration.connector WHERE name = $1 AND kind = $2`, name, BankSFTP).Scan(&id)
	if noRows(err) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("integration: bank channel %q: %w", name, err)
	}
	return id, true, nil
}

// Endpoint resolves a house bank's connector into the SFTP endpoint its
// files are uploaded to, failing closed: a connector that is disabled, not
// a bank channel, without a secret_ref for its private key, or whose key
// or settings do not resolve is refused, and nothing is sent. The key is
// resolved on each use, so a rotated secret takes effect without a restart.
func (b BankChannels) Endpoint(ctx context.Context, id uuid.UUID) (sftpclient.Endpoint, error) {
	return resolve(ctx, b.Secrets, id, sftpChannel{kind: BankSFTP, noun: "bank channel", use: "transmit",
		endpoint: sftpEndpoint}) // a connector stored before T44a checked its settings is refused here
}

// sftpChannel is a kind of SFTP channel as resolve refuses and reads it.
type sftpChannel struct {
	kind, noun string
	use        string // what the connector is enabled to do
	endpoint   func(map[string]any) (sftpclient.Endpoint, error)
}

// resolve reads the connector as an SFTP channel of the kind, failing
// closed as BankChannels.Endpoint says, its private key resolved now.
func resolve(ctx context.Context, keys secrets.Resolver, id uuid.UUID, k sftpChannel) (sftpclient.Endpoint, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	c, err := getConnector(ctx, tx, id, false)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	switch {
	case c.Kind != k.kind:
		return sftpclient.Endpoint{}, notChannel(k, c)
	case c.Status != "active":
		return sftpclient.Endpoint{}, problem.Conflict("integration.connector_disabled", "The "+k.noun+" "+c.Name+" is disabled; enable it to "+k.use+".")
	case c.SecretRef == nil:
		return sftpclient.Endpoint{}, problem.Unprocessable("integration.secret_required",
			"Connector "+c.Name+" names no secret_ref: a "+k.noun+" signs in with a private key only.")
	}
	ep, err := k.endpoint(c.Settings)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	if ep.Signer, err = signer(ctx, keys, k.noun, *c.SecretRef); err != nil {
		return sftpclient.Endpoint{}, err
	}
	return ep, nil
}

// notChannel refuses a connector of another kind than the channel's.
func notChannel(k sftpChannel, c Connector) error {
	detail := "Connector " + c.Name + " is a " + c.Kind + " connector, not a " + k.noun + "."
	if k.kind == TreasuryExport {
		return problem.Unprocessable("integration.not_a_treasury_channel", detail)
	}
	return problem.Unprocessable("integration.not_a_bank_channel", detail)
}

// signer resolves the channel's private key.
func signer(ctx context.Context, keys secrets.Resolver, noun, ref string) (ssh.Signer, error) {
	key, err := keys.Resolve(ctx, ref)
	if err != nil {
		return nil, problem.Unprocessable("integration.secret_unavailable", "The "+noun+"'s private key ("+ref+") cannot be resolved.").WithCause(err)
	}
	s, err := sftpclient.ParseSigner([]byte(key))
	if err != nil {
		return nil, problem.Unprocessable("integration.secret_unavailable", "The "+noun+"'s secret ("+ref+") is not a private key.").WithCause(err)
	}
	return s, nil
}

// TreasuryChannels are the treasury_export connectors the cash context
// exports its companies' snapshots to (ADR-0051 §14, T53h), published to
// it as BankChannels are to payments: through ports the application wires.
type TreasuryChannels struct {
	Secrets secrets.Resolver // resolves a channel's private key
}

// TreasuryChannel is a treasury_export connector as the export reads it.
type TreasuryChannel struct {
	ID        uuid.UUID
	Name      string
	Active    bool
	Format    string   // jsonl or csv; "" when its settings do not resolve
	Companies []string // the company codes it exports
}

// ForCompany are the active treasury channels that export the company,
// by name. One whose format or companies do not resolve is left out; one
// whose SFTP settings do not is not, and its export is refused when
// claimed, so the failure shows on the export.
func (TreasuryChannels) ForCompany(ctx context.Context, code string) ([]TreasuryChannel, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	rows, _ := tx.Query(ctx, `SELECT `+connectorColumns+` FROM integration.connector
		WHERE kind = $1 AND status = 'active' AND settings->'companies' ? $2 ORDER BY name, id`, TreasuryExport, code)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) { return scanConnector(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: treasury channels of company %s: %w", code, err)
	}
	var out []TreasuryChannel
	for _, c := range cs {
		if ch := treasuryChannel(c); ch.Format != "" {
			out = append(out, ch)
		}
	}
	return out, nil
}

// Channel is the treasury channel of the id, active or not; found is false
// when there is no connector of the id or it is of another kind.
func (TreasuryChannels) Channel(ctx context.Context, id uuid.UUID) (TreasuryChannel, bool, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return TreasuryChannel{}, false, err
	}
	c, err := getConnector(ctx, tx, id, false)
	switch {
	case problem.From(err).Status == 404:
		return TreasuryChannel{}, false, nil
	case err != nil:
		return TreasuryChannel{}, false, err
	}
	return treasuryChannel(c), c.Kind == TreasuryExport, nil
}

// treasuryChannel is the connector as a treasury channel: its format and
// companies when its settings name them as treasurySettings checks them,
// none otherwise (an export to it is refused when claimed).
func treasuryChannel(c Connector) TreasuryChannel {
	ch := TreasuryChannel{ID: c.ID, Name: c.Name, Active: c.Status == "active"}
	if format, companies, ok := treasuryFields(c.Settings); ok {
		ch.Format, ch.Companies = format, companies
	}
	return ch
}

// Endpoint resolves a treasury channel into the SFTP endpoint its files
// are uploaded to, failing closed as BankChannels.Endpoint does.
func (t TreasuryChannels) Endpoint(ctx context.Context, id uuid.UUID) (sftpclient.Endpoint, error) {
	return resolve(ctx, t.Secrets, id, sftpChannel{kind: TreasuryExport, noun: "treasury channel", use: "export",
		endpoint: func(settings map[string]any) (sftpclient.Endpoint, error) {
			t, err := treasurySettings(settings)
			return t.Endpoint, err
		}})
}

// TenantConnector is a connector of a tenant.
type TenantConnector struct {
	TenantID, ID uuid.UUID
}

// StatementChannels are the active bank channels that name statement
// directories, across tenants as the worker role reads them: the
// dispatcher fetches the statements of their house banks (T49d).
func StatementChannels(ctx context.Context, tx pgx.Tx) (map[TenantConnector]bool, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id FROM integration.connector WHERE kind = $1 AND status = 'active' AND settings ? 'statements_directory'`, BankSFTP)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[TenantConnector])
	if err != nil {
		return nil, fmt.Errorf("integration: list the bank channels with statements: %w", err)
	}
	out := make(map[TenantConnector]bool, len(list))
	for _, c := range list {
		out[c] = true
	}
	return out, nil
}

// EInvoiceConnectors lists, across tenants as the worker role, the active
// einvoice_provider connectors of active tenants, by tenant and id: those
// the workflow dispatcher fetches e-invoices from once per period
// (ADR-0061 §4). A suspended or offboarded tenant's connectors are left
// out, so its provider is not polled for it (PO 2026-11-09).
func EInvoiceConnectors(ctx context.Context, tx pgx.Tx) ([]TenantConnector, error) {
	rows, _ := tx.Query(ctx, `SELECT c.tenant_id, c.id FROM integration.connector c WHERE c.kind = $1 AND c.status = 'active'
		AND EXISTS (SELECT FROM tenancy.tenant t WHERE t.id = c.tenant_id AND t.status = 'active') ORDER BY c.tenant_id, c.id`, EInvoiceProvider)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[TenantConnector])
	if err != nil {
		return nil, fmt.Errorf("integration: list the e-invoice providers to fetch from: %w", err)
	}
	return list, nil
}
