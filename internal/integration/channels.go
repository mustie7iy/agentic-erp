package integration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	c, err := getConnector(ctx, tx, id, false)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	switch {
	case c.Kind != BankSFTP:
		return sftpclient.Endpoint{}, problem.Unprocessable("integration.not_a_bank_channel", "Connector "+c.Name+" is a "+c.Kind+" connector, not a bank channel.")
	case c.Status != "active":
		return sftpclient.Endpoint{}, problem.Conflict("integration.connector_disabled", "The bank's connector "+c.Name+" is disabled; enable it to transmit.")
	case c.SecretRef == nil:
		return sftpclient.Endpoint{}, problem.Unprocessable("integration.secret_required",
			"Connector "+c.Name+" names no secret_ref: a bank channel signs in with a private key only.")
	}
	ep, err := sftpEndpoint(c.Settings) // a connector stored before T44a checked its settings is refused here
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	if ep.Signer, err = b.signer(ctx, *c.SecretRef); err != nil {
		return sftpclient.Endpoint{}, err
	}
	return ep, nil
}

// signer resolves the channel's private key.
func (b BankChannels) signer(ctx context.Context, ref string) (ssh.Signer, error) {
	key, err := b.Secrets.Resolve(ctx, ref)
	if err != nil {
		return nil, problem.Unprocessable("integration.secret_unavailable", "The bank channel's private key ("+ref+") cannot be resolved.").WithCause(err)
	}
	s, err := sftpclient.ParseSigner([]byte(key))
	if err != nil {
		return nil, problem.Unprocessable("integration.secret_unavailable", "The bank channel's secret ("+ref+") is not a private key.").WithCause(err)
	}
	return s, nil
}
