package integration

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// BankEBICS is the kind of a house bank's EBICS channel (ADR-0038). EBICS is
// refused in production until it has been certified with a bank (T44d): the
// API refuses to create or enable one there, and the server refuses to start
// while one is active (ActiveEBICSConnectors).
const BankEBICS = "bank_ebics"

// ebicsID is an EBICS 3.0 (H005) HostID, PartnerID or UserID.
var ebicsID = regexp.MustCompile(`^[A-Za-z0-9,=]{1,35}$`)

// ebicsKeys are the settings of a bank_ebics channel.
var ebicsKeys = []string{"url", "host_id", "partner_id", "user_id"}

// checkEBICSSettings validates a bank_ebics channel: the bank's EBICS URL,
// over HTTPS only, and the subscriber's host, partner and user ids. The
// subscriber's keys are the secret_ref's; settings hold no other value.
func checkEBICSSettings(settings map[string]any) error {
	for k := range settings {
		if !slices.Contains(ebicsKeys, k) {
			return invalid("integration.invalid_settings", "settings."+k, "Not a bank_ebics setting; keys go in secret_ref.")
		}
	}
	raw, _ := settings["url"].(string) // not a string: "", refused below
	if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return invalid("integration.invalid_settings", "settings.url", "The bank's EBICS URL: https, without credentials, query or fragment.")
	}
	for _, k := range []string{"host_id", "partner_id", "user_id"} {
		if v, _ := settings[k].(string); !ebicsID.MatchString(v) { // not a string: "", which does not match
			return invalid("integration.invalid_settings", "settings."+k, "The EBICS "+k+" the bank assigned: 1-35 letters, digits, ',' or '='.")
		}
	}
	return nil
}

// ebicsRefused is the refusal of an EBICS channel in production.
func ebicsRefused() error {
	return problem.Unprocessable("integration.ebics_not_certified",
		"EBICS channels are refused in production until EBICS has been certified with a bank; use a bank_sftp channel.")
}

// RefuseActiveEBICS refuses a tenant with an active bank_ebics connector:
// a production import runs it before its commit (offboarding.Importer),
// as the cell would otherwise refuse to start at its next deploy.
func RefuseActiveEBICS(ctx context.Context, tx pgx.Tx, tenant uuid.UUID) error {
	var has bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM integration.connector WHERE tenant_id = $1 AND kind = $2 AND status = 'active')`, tenant, BankEBICS).Scan(&has); err != nil {
		return fmt.Errorf("integration: read the EBICS connectors of tenant %s: %w", tenant, err)
	}
	if has {
		return ebicsRefused()
	}
	return nil
}

// EBICSConnector names an active bank_ebics connector of some tenant.
type EBICSConnector struct {
	TenantID, ID uuid.UUID
}

// ActiveEBICSConnectors lists every tenant's active bank_ebics connectors,
// read by the control role across tenants (ADR-0003, 000101): in
// production the server refuses to start while there is one (T44d). The
// API refuses to create or enable one there and a production import refuses
// a tenant with one (RefuseActiveEBICS), so a connector from before or a
// change by hand gets here; disabling it is the remedy.
func ActiveEBICSConnectors(ctx context.Context, tx pgx.Tx) ([]EBICSConnector, error) {
	rows, _ := tx.Query(ctx, `SELECT tenant_id, id FROM integration.connector WHERE kind = $1 AND status = 'active' ORDER BY tenant_id, id`, BankEBICS)
	cs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[EBICSConnector])
	if err != nil {
		return nil, fmt.Errorf("integration: list active EBICS connectors: %w", err)
	}
	return cs, nil
}
