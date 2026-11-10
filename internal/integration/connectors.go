package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
	"github.com/mustie7iy/agentic-erp/internal/platform/sftpclient"
)

func init() {
	problem.RegisterConstraint("connector_tenant_id_name_key", func() *problem.Problem {
		return problem.Conflict("integration.duplicate_name", "Another connector has this name.")
	})
}

// Connector kinds that are sources of migration runs.
const (
	SAPFile  = "sap_file"
	SAPOData = "sap_odata"
)

// BankSFTP is the kind of a house bank's SFTP channel (ADR-0038).
const BankSFTP = "bank_sftp"

// TreasuryExport is the kind of a treasury system's SFTP directory, which
// the cash context exports its companies' snapshots to (ADR-0051 §14).
const TreasuryExport = "treasury_export"

var kinds = []string{SAPOData, SAPFile, BankSFTP, BankEBICS, "edi_as2", "email", "tax_engine", "ticketing", TreasuryExport, EInvoiceProvider}

// Connector is a row of integration.connector.
type Connector struct {
	ID        uuid.UUID
	Kind      string
	Name      string
	Settings  map[string]any
	SecretRef *string
	Status    string
	Version   int32
	CreatedAt time.Time
	CreatedBy uuid.UUID
	UpdatedAt time.Time
	UpdatedBy uuid.UUID
}

const connectorColumns = `id, kind, name, settings, secret_ref, status, version, created_at, created_by, updated_at, updated_by`

func scanConnector(row pgx.Row) (Connector, error) {
	var c Connector
	err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.Settings, &c.SecretRef, &c.Status, &c.Version, &c.CreatedAt, &c.CreatedBy, &c.UpdatedAt, &c.UpdatedBy)
	return c, errs.Wrapf(err, "scan connector")
}

func getConnector(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (Connector, error) {
	q := `SELECT ` + connectorColumns + ` FROM integration.connector WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	c, err := scanConnector(tx.QueryRow(ctx, q, id))
	if noRows(err) {
		return c, problem.NotFoundOf("connector")
	}
	if err != nil {
		return c, fmt.Errorf("integration: read connector %s: %w", id, err)
	}
	return c, nil
}

// checkSettings validates the settings of the source kinds: sap_file needs
// a relative path below the server's extract directory and optionally the
// form of its amounts (amount_format, amounts.go), sap_odata the
// base URL of the SAP system and optionally its client and page size.
// Settings never hold secrets; credentials are named by secret_ref.
func checkSettings(kind string, settings map[string]any) error {
	str := func(key string) (string, bool) {
		s, ok := settings[key].(string)
		return s, ok
	}
	if sys, ok := settings["system"]; ok && (kind == SAPFile || kind == SAPOData) {
		if code, isString := sys.(string); !isString || !systemCode.MatchString(code) {
			return invalid("integration.invalid_settings", "settings.system", "The source system's code in external references: 2-20 upper-case letters, digits or _.")
		}
	}
	switch kind {
	case SAPFile:
		if p, ok := str("path"); !ok || !filepath.IsLocal(p) {
			return invalid("integration.invalid_settings", "settings.path", "An extract file path relative to the server's extract directory.")
		}
		return checkAmountFormat(settings)
	case SAPOData:
		return checkODataSettings(settings)
	case BankSFTP:
		return checkSFTPSettings(settings)
	case BankEBICS:
		return checkEBICSSettings(settings)
	case TreasuryExport:
		_, err := treasurySettings(settings)
		return err
	case EInvoiceProvider:
		_, err := einvoiceSettings(settings)
		return err
	}
	return nil
}

// checkSFTPSettings validates a bank_sftp channel (ADR-0038): the server's
// host and optional port, the account, the server's host key, pinned, and
// the absolute directory the bank collects from. The client's private key
// is the secret_ref's, required where the channel is resolved (T44b): there
// is no password, and no other setting.
func checkSFTPSettings(settings map[string]any) error {
	_, err := sftpEndpoint(settings)
	return err
}

// sftpKeys are the settings of every SFTP channel: the server, the account,
// its pinned key and the directory files are delivered to.
var sftpKeys = []string{"host", "port", "user", "host_key", "directory"}

// sftpEndpoint is the endpoint valid bank_sftp settings name, without the
// client's key.
func sftpEndpoint(settings map[string]any) (sftpclient.Endpoint, error) {
	ep, err := sftpServer(settings, BankSFTP, append([]string{"statements_directory", "statements_archive"}, sftpKeys...), "the bank collects files from")
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	inbound, archive, err := statementDirs(settings)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	ep.Inbound, ep.Archive = inbound, archive
	return ep, nil
}

// sftpServer is the server, account, key and directory of the settings of
// an SFTP channel of the kind, which may name only the keys given; collects
// says who takes the files from the directory.
func sftpServer(settings map[string]any, kind string, keys []string, collects string) (sftpclient.Endpoint, error) {
	for k := range settings {
		if !slices.Contains(keys, k) {
			return sftpclient.Endpoint{}, invalid("integration.invalid_settings", "settings."+k, "Not a "+kind+" setting; credentials go in secret_ref.")
		}
	}
	host, _ := settings["host"].(string) // not a string: "", refused below
	user, _ := settings["user"].(string) // as host
	for _, k := range []string{"host", "user"} {
		if v := map[string]string{"host": host, "user": user}[k]; v == "" || strings.ContainsAny(v, ":@/ ") {
			return sftpclient.Endpoint{}, invalid("integration.invalid_settings", "settings."+k, "The SFTP server's "+k+", without port, path or spaces.")
		}
	}
	port, err := sftpPort(settings)
	if err != nil {
		return sftpclient.Endpoint{}, err
	}
	line, _ := settings["host_key"].(string) // not a string: "", which ParseHostKey refuses
	key, err := sftpclient.ParseHostKey(line)
	if err != nil {
		return sftpclient.Endpoint{}, invalid("integration.invalid_settings", "settings.host_key", "The SFTP server's public key in authorized_keys form, as the bank gave it.")
	}
	dir, _ := settings["directory"].(string) // not a string: "", which is not absolute
	if !path.IsAbs(dir) {
		return sftpclient.Endpoint{}, invalid("integration.invalid_settings", "settings.directory", "The absolute directory "+collects+".")
	}
	return sftpclient.Endpoint{Addr: net.JoinHostPort(host, strconv.Itoa(port)), User: user, HostKey: key, Dir: dir}, nil
}

// treasuryFields are the format and companies treasury_export settings
// name, ok when both are valid: jsonl or csv ("" otherwise), and company
// codes, at least one, each once.
func treasuryFields(settings map[string]any) (format string, companies []string, ok bool) {
	if format, _ = settings["format"].(string); format != "jsonl" && format != "csv" { // not a string: "", refused
		return "", nil, false
	}
	list, _ := settings["companies"].([]any) // not a list: none, refused below
	for _, c := range list {
		code, _ := c.(string) // not a string: "", refused below
		if !companyCode.MatchString(code) || slices.Contains(companies, code) {
			return format, nil, false
		}
		companies = append(companies, code)
	}
	return format, companies, len(companies) > 0
}

// Treasury is what valid treasury_export settings name besides the SFTP
// server: the file format and the companies exported (ADR-0051 §14).
type Treasury struct {
	Endpoint  sftpclient.Endpoint // without the client's key
	Format    string              // jsonl or csv
	Companies []string            // company codes, at least one
}

// companyCode is a legal entity's company code (tenancy.legal_entity).
var companyCode = regexp.MustCompile(`^[A-Z0-9]{1,10}$`)

// treasurySettings validates a treasury_export channel: an SFTP server as
// a bank_sftp channel names one, without statement directories; the format
// of its files, jsonl or csv (the cash export schema v1); and the codes of
// the companies whose snapshots it takes, named one by one, so a company
// is exported only to a channel set up for it.
func treasurySettings(settings map[string]any) (Treasury, error) {
	ep, err := sftpServer(settings, TreasuryExport, append([]string{"format", "companies"}, sftpKeys...), "the treasury system collects files from")
	if err != nil {
		return Treasury{}, err
	}
	format, codes, ok := treasuryFields(settings)
	switch {
	case format == "":
		return Treasury{}, invalid("integration.invalid_settings", "settings.format", "The format of the files, jsonl or csv (the cash export schema v1).")
	case !ok:
		return Treasury{}, invalid("integration.invalid_settings", "settings.companies", "The company codes whose snapshots are exported, at least one, each once.")
	}
	return Treasury{Endpoint: ep, Format: format, Companies: codes}, nil
}

// statementDirs are the directories the bank leaves statements in and
// they are moved to once imported (T49d, ADR-0047): both or neither, each
// absolute, and not the same, so an archived file is never fetched again.
func statementDirs(settings map[string]any) (inbound, archive string, err error) {
	_, hasIn := settings["statements_directory"]
	_, hasArchive := settings["statements_archive"]
	if !hasIn && !hasArchive {
		return "", "", nil
	}
	inbound, _ = settings["statements_directory"].(string) // not a string: "", which is not absolute
	archive, _ = settings["statements_archive"].(string)   // as inbound
	if !path.IsAbs(inbound) {
		return "", "", invalid("integration.invalid_settings", "settings.statements_directory", "The absolute directory the bank leaves statements in, with statements_archive.")
	}
	if !path.IsAbs(archive) || path.Clean(archive) == path.Clean(inbound) {
		return "", "", invalid("integration.invalid_settings", "settings.statements_archive",
			"The absolute directory imported statements are moved to, not statements_directory.")
	}
	return path.Clean(inbound), path.Clean(archive), nil
}

// sftpPort is the settings' port, 22 when left out.
func sftpPort(settings map[string]any) (int, error) {
	n, ok := settings["port"]
	if !ok {
		return 22, nil
	}
	//nolint:forbidigo // encoding/json decodes every number in map[string]any as float64; a port is not an amount
	if f, isNumber := n.(float64); isNumber && f >= 1 && f <= 65535 && f == float64(int(f)) {
		return int(f), nil
	}
	return 0, invalid("integration.invalid_settings", "settings.port", "The SFTP server's port, from 1 to 65535; 22 when left out.")
}

// checkODataSettings validates the settings of a sap_odata source.
func checkODataSettings(settings map[string]any) error {
	if _, ok := settings["amount_format"]; ok {
		return invalid("integration.invalid_settings", "settings.amount_format", "OData delivers amounts in external form; only a sap_file connector takes amount_format.")
	}
	b, ok := settings["base_url"].(string)
	u, err := url.Parse(b)
	if !ok || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return invalid("integration.invalid_settings", "settings.base_url", "The SAP system's base URL, for example https://sap.example.com.")
	}
	if c, ok := settings["client"]; ok {
		if _, isString := c.(string); !isString {
			return invalid("integration.invalid_settings", "settings.client", "The SAP client as a string, for example \"100\".")
		}
	}
	if n, ok := settings["page_size"]; ok {
		//nolint:forbidigo // encoding/json decodes every number in map[string]any as float64; a page size is not an amount
		if f, isNumber := n.(float64); !isNumber || f < 1 || f > 5000 || f != float64(int(f)) {
			return invalid("integration.invalid_settings", "settings.page_size", "A page size from 1 to 5000.")
		}
	}
	return nil
}

func statusEvent(c Connector, name string, previous *string) outbox.Event {
	return outbox.Event{Context: "integration", Name: name, AggregateType: "integration.connector", AggregateID: c.ID,
		Payload: map[string]any{"connector_id": c.ID, "kind": c.Kind, "name": c.Name, "previous_status": previous, "status": c.Status}}
}

// NewConnector is a connector as a client creates it.
type NewConnector struct {
	Kind, Name string
	Settings   map[string]any
	SecretRef  *string
}

// CreateConnector creates an active connector.
func CreateConnector(ctx context.Context, in NewConnector) (Connector, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Connector{}, err
	}
	if !slices.Contains(kinds, in.Kind) {
		return Connector{}, invalid("integration.unknown_kind", "kind", "A connector kind of the contract.")
	}
	if err := checkSettings(in.Kind, in.Settings); err != nil {
		return Connector{}, err
	}
	c, err := scanConnector(tx.QueryRow(ctx, `INSERT INTO integration.connector (tenant_id, id, kind, name, settings, secret_ref, status, created_by, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $7) RETURNING `+connectorColumns,
		p.TenantID, uuid.Must(uuid.NewV7()), in.Kind, in.Name, in.Settings, in.SecretRef, p.ID))
	if err != nil {
		return Connector{}, fmt.Errorf("integration: create connector %q: %w", in.Name, err)
	}
	return c, record(ctx, "integration.connector.create", c.ID, map[string]any{"kind": c.Kind, "name": c.Name}, statusEvent(c, "connector_enabled", nil))
}

// ConnectorChange is a merge patch of a connector.
type ConnectorChange struct {
	Name      *string
	Settings  map[string]any // replaced as a whole when not nil
	SecretRef **string       // nil: unchanged; pointer to nil: clear
}

// UpdateConnector changes a connector's name, settings or secret reference.
func UpdateConnector(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, ch ConnectorChange) (Connector, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Connector{}, err
	}
	c, err := getConnector(ctx, tx, id, true)
	if err != nil {
		return Connector{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Connector{}, err
	}
	if ch.Name != nil {
		c.Name = *ch.Name
	}
	if ch.Settings != nil {
		if err := checkSettings(c.Kind, ch.Settings); err != nil {
			return Connector{}, err
		}
		c.Settings = ch.Settings
	}
	if ch.SecretRef != nil {
		c.SecretRef = *ch.SecretRef
	}
	updated, err := scanConnector(tx.QueryRow(ctx, `UPDATE integration.connector SET name = $2, settings = $3, secret_ref = $4, version = version + 1,
		updated_by = $5 WHERE id = $1 RETURNING `+connectorColumns, id, c.Name, c.Settings, c.SecretRef, p.ID))
	if err != nil {
		return Connector{}, fmt.Errorf("integration: update connector %s: %w", id, err)
	}
	return updated, record(ctx, "integration.connector.update", id, map[string]any{"name": updated.Name, "secret_ref_set": updated.SecretRef != nil})
}

// SetConnectorStatus enables (active) or disables a connector.
func SetConnectorStatus(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, status, reason string) (Connector, error) {
	return setConnectorStatus(ctx, id, ifMatch, status, map[string]any{"reason": reason}, func(Connector) error { return nil })
}

// DisableByOperator disables a connector for an operator acting without the
// API (erp-server disable-connector), whatever its version: the remedy when
// a production cell refuses to start for an active bank_ebics connector
// (ADR-0038), audited with the operator's name and reason like a disable
// through the API, and published.
func DisableByOperator(ctx context.Context, id uuid.UUID, operator, reason string) (Connector, error) {
	return setConnectorStatus(ctx, id, func(int32) error { return nil }, "disabled", map[string]any{"reason": reason, "operator": operator},
		func(Connector) error { return nil })
}

// setConnectorStatus sets the status once allow accepts the connector as
// read, locked, and audits it with detail.
func setConnectorStatus(ctx context.Context, id uuid.UUID, ifMatch func(int32) error, status string, detail map[string]any, allow func(Connector) error) (Connector, error) {
	tx, p, err := reqctx.Request(ctx)
	if err != nil {
		return Connector{}, err
	}
	c, err := getConnector(ctx, tx, id, true)
	if err != nil {
		return Connector{}, err
	}
	if err := ifMatch(c.Version); err != nil {
		return Connector{}, err
	}
	if c.Status == status {
		return Connector{}, problem.Conflict("integration.invalid_status", "The connector is already "+status+".")
	}
	if err := allow(c); err != nil {
		return Connector{}, err
	}
	updated, err := scanConnector(tx.QueryRow(ctx, `UPDATE integration.connector SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+connectorColumns, id, status, p.ID))
	if err != nil {
		return Connector{}, fmt.Errorf("integration: set connector %s to %s: %w", id, status, err)
	}
	name, action := "connector_enabled", "integration.connector.enable"
	if status == "disabled" {
		name, action = "connector_disabled", "integration.connector.disable"
	}
	return updated, record(ctx, action, id, detail, statusEvent(updated, name, &c.Status))
}

func fromSettings(raw map[string]jx.Raw) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		var x any
		_ = json.Unmarshal(v, &x) // the generated decoder only passes valid JSON
		out[k] = x
	}
	return out
}

func connectorToAPI(c Connector) gen.Connector {
	out := gen.Connector{ID: c.ID, Version: c.Version, CreatedAt: c.CreatedAt, CreatedBy: c.CreatedBy, UpdatedAt: c.UpdatedAt,
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Kind: c.Kind, Name: c.Name, Settings: apitypes.EncodeObject(c.Settings), Status: c.Status}
	if c.SecretRef != nil {
		out.SecretRef.SetTo(*c.SecretRef)
	}
	return out
}

func connectorOK(c Connector) *gen.UpdateConnectorOKHeaders {
	return &gen.UpdateConnectorOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: connectorToAPI(c)}
}

// CreateConnector implements POST /v1/connectors.
func (e *Endpoints) CreateConnector(ctx context.Context, req *gen.ConnectorCreate, _ gen.CreateConnectorParams) (gen.CreateConnectorRes, error) {
	in := NewConnector{Kind: req.Kind, Name: req.Name, Settings: fromSettings(req.Settings)}
	if s, ok := req.SecretRef.Get(); ok {
		in.SecretRef = &s
	}
	if err := e.refuseUncertified(in.Kind, in.Settings); err != nil {
		return nil, err
	}
	c, err := CreateConnector(ctx, in)
	if err != nil {
		return nil, err
	}
	return &gen.ConnectorHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Location: gen.NewOptString("/v1/connectors/" + c.ID.String()),
		Response: connectorToAPI(c)}, nil
}

// GetConnector implements GET /v1/connectors/{id}.
func (e *Endpoints) GetConnector(ctx context.Context, params gen.GetConnectorParams) (gen.GetConnectorRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	c, err := getConnector(ctx, tx, params.ID, false)
	if err != nil {
		return nil, err
	}
	return &gen.GetConnectorOKHeaders{ETag: gen.NewOptString(etag.Format(c.Version)), Response: connectorToAPI(c)}, nil
}

var connectorSort = pagination.Sort{Default: "name", Fields: []pagination.Field{{Name: "name", Column: "name", Type: "text"}}}

// ListConnectors implements GET /v1/connectors.
func (e *Endpoints) ListConnectors(ctx context.Context, params gen.ListConnectorsParams) (gen.ListConnectorsRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	ks, err := e.Pages.Keyset(connectorSort, "listConnectors|"+params.Kind.Value, apitypes.Page(params.OrderBy, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+connectorColumns+` FROM integration.connector WHERE ($1 = '' OR kind = $1)`, []any{params.Kind.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) { return scanConnector(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: list connectors: %w", err)
	}
	items, next := apitypes.PageOf(ks, cs, func(c Connector) (string, uuid.UUID) { return c.Name, c.ID }, connectorToAPI)
	return &gen.ConnectorPage{Items: items, NextPageToken: next}, nil
}

// UpdateConnector implements PATCH /v1/connectors/{id}.
func (e *Endpoints) UpdateConnector(ctx context.Context, req *gen.ConnectorUpdate, params gen.UpdateConnectorParams) (gen.UpdateConnectorRes, error) {
	ch := ConnectorChange{}
	if n, ok := req.Name.Get(); ok {
		ch.Name = &n
	}
	if s, ok := req.Settings.Get(); ok {
		ch.Settings = fromSettings(s)
	}
	if req.SecretRef.Set {
		var ref *string
		if !req.SecretRef.Null {
			ref = &req.SecretRef.Value
		}
		ch.SecretRef = &ref
	}
	c, err := UpdateConnector(ctx, params.ID, etag.IfMatch(params.IfMatch), ch)
	if err != nil {
		return nil, err
	}
	return connectorOK(c), nil
}

// EnableConnector implements POST /v1/connectors/{id}:enable.
func (e *Endpoints) EnableConnector(ctx context.Context, params gen.EnableConnectorParams) (gen.EnableConnectorRes, error) {
	c, err := setConnectorStatus(ctx, params.ID, etag.IfMatch(params.IfMatch), "active", map[string]any{"reason": ""}, func(c Connector) error { return e.refuseUncertified(c.Kind, c.Settings) })
	if err != nil {
		return nil, err
	}
	return connectorOK(c), nil
}

// DisableConnector implements POST /v1/connectors/{id}:disable.
func (e *Endpoints) DisableConnector(ctx context.Context, req *gen.ReasonInput, params gen.DisableConnectorParams) (gen.DisableConnectorRes, error) {
	c, err := SetConnectorStatus(ctx, params.ID, etag.IfMatch(params.IfMatch), "disabled", req.Reason)
	if err != nil {
		return nil, err
	}
	return connectorOK(c), nil
}
