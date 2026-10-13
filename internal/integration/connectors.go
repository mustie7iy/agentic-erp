package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/outbox"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
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

var kinds = []string{SAPOData, SAPFile, "bank_sftp", "bank_ebics", "edi_as2", "email", "tax_engine", "ticketing"}

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
		return c, notFound("connector")
	}
	if err != nil {
		return c, fmt.Errorf("integration: read connector %s: %w", id, err)
	}
	return c, nil
}

// checkSettings validates the settings of the source kinds: sap_file needs
// a relative path below the server's extract directory, sap_odata the
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
	case SAPOData:
		return checkODataSettings(settings)
	}
	return nil
}

// checkODataSettings validates the settings of a sap_odata source.
func checkODataSettings(settings map[string]any) error {
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
	updated, err := scanConnector(tx.QueryRow(ctx, `UPDATE integration.connector SET status = $2, version = version + 1, updated_by = $3
		WHERE id = $1 RETURNING `+connectorColumns, id, status, p.ID))
	if err != nil {
		return Connector{}, fmt.Errorf("integration: set connector %s to %s: %w", id, status, err)
	}
	name, action := "connector_enabled", "integration.connector.enable"
	if status == "disabled" {
		name, action = "connector_disabled", "integration.connector.disable"
	}
	return updated, record(ctx, action, id, map[string]any{"reason": reason}, statusEvent(updated, name, &c.Status))
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
		UpdatedBy: gen.NewOptUUID(c.UpdatedBy), Kind: c.Kind, Name: c.Name, Settings: gen.ConnectorSettings{}, Status: c.Status}
	for k, v := range c.Settings {
		b, _ := json.Marshal(v) // values decoded from JSON always encode
		out.Settings[k] = jx.Raw(b)
	}
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
	ks, err := e.Pages.Keyset(connectorSort, "listConnectors|"+params.Kind.Value, pagination.Request{OrderBy: params.OrderBy.Value,
		PageSize: params.PageSize.Value, PageSizeSet: params.PageSize.Set, PageToken: params.PageToken.Value, PageTokenSet: params.PageToken.Set})
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+connectorColumns+` FROM integration.connector WHERE ($1 = '' OR kind = $1)`, []any{params.Kind.Value})
	rows, _ := tx.Query(ctx, q, args...)
	cs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Connector, error) { return scanConnector(r) })
	if err != nil {
		return nil, fmt.Errorf("integration: list connectors: %w", err)
	}
	cs, next := pagination.Page(ks, cs, func(c Connector) (string, uuid.UUID) { return c.Name, c.ID })
	page := gen.ConnectorPage{Items: make([]gen.Connector, len(cs))}
	for i, c := range cs {
		page.Items[i] = connectorToAPI(c)
	}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return &gen.ConnectorPageHeaders{Response: page}, nil
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
	c, err := SetConnectorStatus(ctx, params.ID, etag.IfMatch(params.IfMatch), "active", "")
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
