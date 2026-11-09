package einvoicing

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/etag"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// EInvoice is a stored e-invoice.
type EInvoice struct {
	ID, ConnectorID, FetchOperationID uuid.UUID
	MessageID                         string
	Receiver                          *string
	LegalEntityID                     *uuid.UUID
	Status                            string
	SHA256                            []byte
	Size                              int64
	ReceivedAt                        time.Time
	AcknowledgedAt                    *time.Time
	// ChangedHash is the SHA-256 of other bytes the provider delivered
	// later under the message id, the latest, and ChangedAt when a fetch
	// found them; nil while there are none (ADR-0061 §4).
	ChangedHash      []byte
	ChangedAt        *time.Time
	ChangedFetchedBy *uuid.UUID // the fetch that found them
	Version          int32
}

const einvoiceColumns = `id, connector_id, message_id, receiver, legal_entity_id, status, sha256, size, fetch_operation_id, received_at, acknowledged_at,
	changed_hash, changed_at, changed_fetch_operation_id, version`

// fields are the destinations of einvoiceColumns, in order.
func (d *EInvoice) fields() []any {
	return []any{&d.ID, &d.ConnectorID, &d.MessageID, &d.Receiver, &d.LegalEntityID, &d.Status, &d.SHA256, &d.Size, &d.FetchOperationID, &d.ReceivedAt,
		&d.AcknowledgedAt, &d.ChangedHash, &d.ChangedAt, &d.ChangedFetchedBy, &d.Version}
}

func scanEInvoice(r pgx.Row) (EInvoice, error) {
	var d EInvoice
	err := r.Scan(d.fields()...)
	return d, errs.Wrapf(err, "scan e-invoice")
}

// visible says whether a caller of the access may see the document: one
// of a company the caller may act in, or one addressed to none of the
// connector's companies to a caller who may act in every company.
func visible(d EInvoice, access authz.Access) bool {
	if d.LegalEntityID == nil {
		return access.All
	}
	return access.Allows(*d.LegalEntityID)
}

var einvoiceSort = pagination.Sort{Default: "received_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "received_at", Column: "received_at", Type: "timestamptz"}}}

// ListEInvoices implements GET /v1/e-invoices (ADR-0061 §15).
func (e *Endpoints) ListEInvoices(ctx context.Context, params gen.ListEInvoicesParams) (gen.ListEInvoicesRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	scope := fmt.Sprintf("listEInvoices|%s|%s|%s|%s|%s|%s", optUUID(params.LegalEntityID), optUUID(params.ConnectorID), params.Status.Value,
		optUUID(params.FetchOperationID), optTime(params.ReceivedFrom), optTime(params.ReceivedTo))
	ks, err := e.Pages.Keyset(einvoiceSort, scope, apitypes.Page(gen.OptString{}, params.PageSize, params.PageToken))
	if err != nil {
		return nil, err
	}
	q, args := ks.SQL(`SELECT `+einvoiceColumns+` FROM einvoicing.einvoice
		WHERE ($1::uuid IS NULL OR legal_entity_id = $1) AND ($2::uuid IS NULL OR connector_id = $2) AND ($3 = '' OR status = $3)
		  AND ($4::uuid IS NULL OR fetch_operation_id = $4 OR changed_fetch_operation_id = $4) AND ($5::timestamptz IS NULL OR received_at >= $5)
		  AND ($6::timestamptz IS NULL OR received_at < $6) AND ($7 OR legal_entity_id = ANY($8))`,
		[]any{apitypes.Ptr[uuid.UUID](params.LegalEntityID), apitypes.Ptr[uuid.UUID](params.ConnectorID), params.Status.Value,
			apitypes.Ptr[uuid.UUID](params.FetchOperationID), apitypes.Ptr[time.Time](params.ReceivedFrom), apitypes.Ptr[time.Time](params.ReceivedTo),
			access.All, access.IDs})
	rows, _ := tx.Query(ctx, q, args...)
	docs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (EInvoice, error) { return scanEInvoice(r) })
	if err != nil {
		return nil, fmt.Errorf("einvoicing: list e-invoices: %w", err)
	}
	docs, next := pagination.Page(ks, docs, func(d EInvoice) (string, uuid.UUID) { return d.ReceivedAt.UTC().Format(time.RFC3339Nano), d.ID })
	n, err := e.names(ctx, docs)
	if err != nil {
		return nil, err
	}
	items := make([]gen.EInvoice, len(docs))
	for i, d := range docs {
		items[i] = n.toAPI(d)
	}
	page := &gen.EInvoicePage{Items: items}
	if next != "" {
		page.NextPageToken.SetTo(next)
	}
	return page, nil
}

func optUUID(o gen.OptUUID) string {
	if !o.Set {
		return ""
	}
	return o.Value.String()
}

func optTime(o gen.OptDateTime) string {
	if !o.Set {
		return ""
	}
	return o.Value.UTC().Format(time.RFC3339Nano)
}

// GetEInvoice implements GET /v1/e-invoices/{id}.
func (e *Endpoints) GetEInvoice(ctx context.Context, params gen.GetEInvoiceParams) (gen.GetEInvoiceRes, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, err
	}
	d, err := scanEInvoice(tx.QueryRow(ctx, `SELECT `+einvoiceColumns+` FROM einvoicing.einvoice WHERE id = $1`, params.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows) || (err == nil && !visible(d, access)):
		return nil, problem.NotFoundOf("e-invoice")
	case err != nil:
		return nil, fmt.Errorf("einvoicing: read e-invoice %s: %w", params.ID, err)
	}
	n, err := e.names(ctx, []EInvoice{d})
	if err != nil {
		return nil, err
	}
	return &gen.EInvoiceHeaders{ETag: gen.NewOptString(etag.Format(d.Version)), Response: n.toAPI(d)}, nil
}

// names are the connectors' names and the companies of a page.
type names struct {
	connectors map[uuid.UUID]string
	companies  map[uuid.UUID]Company
}

var (
	connectorRef = idref.Kind{Name: "connector", Context: "integration"}
	companyRef   = idref.Kind{Name: "legal entity", Context: "tenancy"}
)

// names reads what names the page's connectors and companies, one call
// each (REVIEW.md: an identifier with the name its consumer needs).
func (e *Endpoints) names(ctx context.Context, docs []EInvoice) (names, error) {
	var n names
	var err error
	if n.connectors, err = idref.Resolve(ctx, e.Connectors.Names, idref.Of(docs, func(d EInvoice) *uuid.UUID { return &d.ConnectorID }), connectorRef); err != nil {
		return n, err
	}
	n.companies, err = idref.Resolve(ctx, e.Companies.Named, idref.Of(docs, func(d EInvoice) *uuid.UUID { return d.LegalEntityID }), companyRef)
	return n, err
}

func (n names) toAPI(d EInvoice) gen.EInvoice {
	out := gen.EInvoice{ID: d.ID, ConnectorID: d.ConnectorID, ConnectorName: n.connectors[d.ConnectorID], MessageID: d.MessageID, Status: d.Status,
		SHA256: hex.EncodeToString(d.SHA256), Size: d.Size, FetchOperationID: d.FetchOperationID, ReceivedAt: d.ReceivedAt, Version: d.Version}
	if d.Receiver != nil {
		out.Receiver.SetTo(*d.Receiver)
	} else {
		out.Receiver.SetToNull()
	}
	if d.LegalEntityID != nil {
		co := n.companies[*d.LegalEntityID]
		out.LegalEntityID.SetTo(*d.LegalEntityID)
		out.CompanyCode.SetTo(co.Code)
		out.CompanyName.SetTo(co.Name)
	} else {
		out.LegalEntityID.SetToNull()
		out.CompanyCode.SetToNull()
		out.CompanyName.SetToNull()
	}
	if d.AcknowledgedAt != nil {
		out.AcknowledgedAt.SetTo(*d.AcknowledgedAt)
	} else {
		out.AcknowledgedAt.SetToNull()
	}
	if d.ChangedAt != nil {
		out.ChangedSHA256.SetTo(hex.EncodeToString(d.ChangedHash))
		out.ChangedAt.SetTo(*d.ChangedAt)
		out.ChangedFetchOperationID.SetTo(*d.ChangedFetchedBy)
	} else {
		out.ChangedSHA256.SetToNull()
		out.ChangedAt.SetToNull()
		out.ChangedFetchOperationID.SetToNull()
	}
	return out
}
