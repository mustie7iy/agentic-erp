package integration

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/go-faster/jx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/idref"
	"github.com/mustie7iy/agentic-erp/internal/platform/pagination"
	"github.com/mustie7iy/agentic-erp/internal/platform/reqctx"
)

// WMSMessage is a row of integration.wms_message.
type WMSMessage struct {
	ID, ConnectorID                uuid.UUID
	Direction, MessageID, Kind     string
	LegalEntityID, DocumentID      *uuid.UUID
	DocumentNumber, PlantCode      *string
	Body                           []byte
	Status                         string
	Attempts                       int32
	NextAttemptAt, SentAt, AckedAt *time.Time
	LastError, ProblemCode, Detail *string
	MaterialDocumentID             *uuid.UUID
	MaterialDocumentNumber         *string
	CreatedAt, UpdatedAt           time.Time
}

const wmsMessageColumns = `id, connector_id, direction, message_id, kind, legal_entity_id, document_id, document_number, plant_code, body::text, status,
	attempts, next_attempt_at, sent_at, acknowledged_at, last_error, problem_code, problem_detail, material_document_id, material_document_number,
	created_at, updated_at`

func scanWMSMessage(row pgx.Row) (WMSMessage, error) {
	var m WMSMessage
	err := row.Scan(&m.ID, &m.ConnectorID, &m.Direction, &m.MessageID, &m.Kind, &m.LegalEntityID, &m.DocumentID, &m.DocumentNumber, &m.PlantCode,
		&m.Body, &m.Status, &m.Attempts, &m.NextAttemptAt, &m.SentAt, &m.AckedAt, &m.LastError, &m.ProblemCode, &m.Detail, &m.MaterialDocumentID,
		&m.MaterialDocumentNumber, &m.CreatedAt, &m.UpdatedAt)
	return m, errs.Wrapf(err, "scan wms message")
}

var wmsMessageSort = pagination.Sort{Default: "created_at", DefaultDesc: true,
	Fields: []pagination.Field{{Name: "created_at", Column: "created_at", Type: "timestamptz"}}}

// ListWMSMessages implements GET /v1/wms-messages: the messages exchanged
// with warehouse systems of the companies the caller may act in, and, to
// a caller who may act in every company, those of no known company,
// newest first.
func (e *Endpoints) ListWMSMessages(ctx context.Context, params gen.ListWMSMessagesParams) (gen.ListWMSMessagesRes, error) {
	// An unset id filter is the nil UUID in the scope, which no row has.
	ms, next, n, err := e.messages(ctx, messageQuery{kinds: wmsKinds, connector: params.ConnectorID, direction: params.Direction.Value,
		status: params.Status.Value, delivery: params.OutboundDeliveryID, order: params.PurchaseOrderID, size: params.PageSize, token: params.PageToken,
		scope: fmt.Sprintf("listWMSMessages|%s|%s|%s|%s|%s", params.ConnectorID.Value, params.Direction.Value, params.Status.Value,
			params.OutboundDeliveryID.Value, params.PurchaseOrderID.Value)})
	if err != nil {
		return nil, err
	}
	page := &gen.WMSMessagePage{Items: make([]gen.WMSMessage, len(ms)), NextPageToken: next}
	for i, m := range ms {
		page.Items[i] = n.toAPI(m)
	}
	return page, nil
}

// ListGTSMessages implements GET /v1/gts-messages (S2s): the messages
// exchanged with trade compliance services, as ListTMSMessages lists
// those of transport systems; a GTS message's fields are a TMS message's.
func (e *Endpoints) ListGTSMessages(ctx context.Context, params gen.ListGTSMessagesParams) (gen.ListGTSMessagesRes, error) {
	ms, next, n, err := e.messages(ctx, messageQuery{kinds: gtsKinds, connector: params.ConnectorID, direction: params.Direction.Value,
		status: params.Status.Value, delivery: params.OutboundDeliveryID, size: params.PageSize, token: params.PageToken,
		scope: fmt.Sprintf("listGTSMessages|%s|%s|%s|%s", params.ConnectorID.Value, params.Direction.Value, params.Status.Value,
			params.OutboundDeliveryID.Value)})
	if err != nil {
		return nil, err
	}
	page := &gen.GTSMessagePage{Items: make([]gen.GTSMessage, len(ms)), NextPageToken: next}
	for i, m := range ms {
		page.Items[i] = gen.GTSMessage(n.tmsToAPI(m))
	}
	return page, nil
}

// ListTMSMessages implements GET /v1/tms-messages (S2r): the messages
// exchanged with transport systems, as ListWMSMessages lists those of
// warehouse systems.
func (e *Endpoints) ListTMSMessages(ctx context.Context, params gen.ListTMSMessagesParams) (gen.ListTMSMessagesRes, error) {
	ms, next, n, err := e.messages(ctx, messageQuery{kinds: tmsKinds, connector: params.ConnectorID, direction: params.Direction.Value,
		status: params.Status.Value, delivery: params.OutboundDeliveryID, size: params.PageSize, token: params.PageToken,
		scope: fmt.Sprintf("listTMSMessages|%s|%s|%s|%s", params.ConnectorID.Value, params.Direction.Value, params.Status.Value,
			params.OutboundDeliveryID.Value)})
	if err != nil {
		return nil, err
	}
	page := &gen.TMSMessagePage{Items: make([]gen.TMSMessage, len(ms)), NextPageToken: next}
	for i, m := range ms {
		page.Items[i] = n.tmsToAPI(m)
	}
	return page, nil
}

// messageQuery is a page of the messages of some kinds, filtered.
type messageQuery struct {
	kinds             []string
	connector         gen.OptUUID
	delivery, order   gen.OptUUID
	direction, status string
	size              gen.OptInt32
	token             gen.OptString
	scope             string // the page token's: the operation and its filters
}

// messages reads a page of the messages of the query's kinds, of the
// companies the caller may act in, with what names their connectors and
// companies, and the next page's token.
func (e *Endpoints) messages(ctx context.Context, mq messageQuery) ([]WMSMessage, gen.OptString, wmsNames, error) {
	tx, _, err := reqctx.Request(ctx)
	if err != nil {
		return nil, gen.OptString{}, wmsNames{}, err
	}
	access, err := authz.Companies(ctx)
	if err != nil {
		return nil, gen.OptString{}, wmsNames{}, err
	}
	ks, err := e.Pages.Keyset(wmsMessageSort, mq.scope, apitypes.Page(gen.OptString{}, mq.size, mq.token))
	if err != nil {
		return nil, gen.OptString{}, wmsNames{}, err
	}
	q, args := ks.SQL(`SELECT `+wmsMessageColumns+` FROM integration.wms_message
		WHERE kind = ANY($10) AND ($1::uuid IS NULL OR connector_id = $1) AND ($2 = '' OR direction = $2) AND ($3 = '' OR status = $3)
		  AND ($4::uuid IS NULL OR document_id = $4 AND kind = ANY($7)) AND ($8::uuid IS NULL OR document_id = $8 AND kind = ANY($9))
		  AND ($5 OR legal_entity_id = ANY($6))`,
		[]any{apitypes.Ptr[uuid.UUID](mq.connector), mq.direction, mq.status, apitypes.Ptr[uuid.UUID](mq.delivery),
			access.All, access.IDs, deliveryKinds, apitypes.Ptr[uuid.UUID](mq.order), orderKinds, mq.kinds})
	rows, _ := tx.Query(ctx, q, args...)
	ms, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (WMSMessage, error) { return scanWMSMessage(r) })
	if err != nil {
		return nil, gen.OptString{}, wmsNames{}, fmt.Errorf("integration: list %v messages: %w", mq.kinds, err)
	}
	ms, next := pagination.Page(ks, ms, func(m WMSMessage) (string, uuid.UUID) { return pagination.Instant(m.CreatedAt), m.ID })
	n, err := e.wmsNames(ctx, tx, ms)
	if err != nil {
		return nil, gen.OptString{}, wmsNames{}, err
	}
	var token gen.OptString
	if next != "" {
		token.SetTo(next)
	}
	return ms, token, n, nil
}

// wmsNames are the connectors' names and the companies' codes of a page.
type wmsNames struct {
	connectors map[uuid.UUID]string
	companies  map[uuid.UUID]string
}

var companyRef = idref.Kind{Name: "legal entity", Context: "tenancy"}

// wmsNames reads what names the page's connectors and companies, each
// once (REVIEW.md: an identifier with the name its consumer needs).
func (e *Endpoints) wmsNames(ctx context.Context, tx pgx.Tx, ms []WMSMessage) (wmsNames, error) {
	var n wmsNames
	var err error
	if n.connectors, err = idref.Resolve(ctx, connectorNames(tx), idref.Of(ms, func(m WMSMessage) *uuid.UUID { return &m.ConnectorID }), connectorRef); err != nil {
		return n, err
	}
	codes := func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
		out := make(map[uuid.UUID]string, len(ids))
		for _, id := range ids {
			le, ok, err := e.LegalEntities.Entity(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("integration: company %s of a wms message: %w", id, err)
			}
			if ok {
				out[id] = le.Code
			}
		}
		return out, nil
	}
	n.companies, err = idref.Resolve(ctx, codes, idref.Of(ms, func(m WMSMessage) *uuid.UUID { return m.LegalEntityID }), companyRef)
	return n, err
}

// The kinds whose document is an outbound delivery, and those whose
// document is a purchase order (S2q2); the kinds a warehouse system's
// messages are of, a transport system's (S2r) and a trade compliance
// service's (S2s).
var (
	deliveryKinds = []string{WMSOutboundDelivery, WMSGoodsIssue, TMSTransportRequest, TMSShipment, GTSScreeningRequest, GTSScreeningResult}
	orderKinds    = []string{WMSInboundReceipt, WMSGoodsReceipt}
	wmsKinds      = []string{WMSOutboundDelivery, WMSGoodsIssue, WMSInboundReceipt, WMSGoodsReceipt}
	tmsKinds      = []string{TMSTransportRequest, TMSShipment}
	gtsKinds      = []string{GTSScreeningRequest, GTSScreeningResult}
)

func (n wmsNames) toAPI(m WMSMessage) gen.WMSMessage {
	out := gen.WMSMessage{ID: m.ID, ConnectorID: m.ConnectorID, ConnectorName: n.connectors[m.ConnectorID], Direction: m.Direction, MessageID: m.MessageID,
		Kind: m.Kind, Body: wmsBody(m.Body), Status: m.Status, Attempts: m.Attempts, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		LegalEntityID: apitypes.OptUUID(m.LegalEntityID), PlantCode: apitypes.OptString(m.PlantCode),
		NextAttemptAt: apitypes.OptDateTime(m.NextAttemptAt), SentAt: apitypes.OptDateTime(m.SentAt),
		AcknowledgedAt: apitypes.OptDateTime(m.AckedAt), LastErrorCode: apitypes.OptString(m.LastError),
		ProblemCode: apitypes.OptString(m.ProblemCode), ProblemDetail: apitypes.OptString(m.Detail),
		MaterialDocumentID: apitypes.OptUUID(m.MaterialDocumentID), MaterialDocumentNumber: apitypes.OptString(m.MaterialDocumentNumber)}
	if m.LegalEntityID != nil {
		out.CompanyCode.SetTo(n.companies[*m.LegalEntityID])
	}
	if slices.Contains(orderKinds, m.Kind) {
		out.PurchaseOrderID, out.PurchaseOrderNumber = apitypes.OptUUID(m.DocumentID), apitypes.OptString(m.DocumentNumber)
	} else {
		out.OutboundDeliveryID, out.OutboundDeliveryNumber = apitypes.OptUUID(m.DocumentID), apitypes.OptString(m.DocumentNumber)
	}
	return out
}

// tmsToAPI is a transport system's message as the API gives it: a
// warehouse system's but for the purchase order and material document,
// which a transport system's never names.
func (n wmsNames) tmsToAPI(m WMSMessage) gen.TMSMessage {
	w := n.toAPI(m)
	return gen.TMSMessage{ID: w.ID, ConnectorID: w.ConnectorID, ConnectorName: w.ConnectorName, Direction: w.Direction, MessageID: w.MessageID,
		Kind: w.Kind, LegalEntityID: w.LegalEntityID, CompanyCode: w.CompanyCode, OutboundDeliveryID: w.OutboundDeliveryID,
		OutboundDeliveryNumber: w.OutboundDeliveryNumber, PlantCode: w.PlantCode, Body: w.Body, Status: w.Status, Attempts: w.Attempts,
		NextAttemptAt: w.NextAttemptAt, LastErrorCode: w.LastErrorCode, SentAt: w.SentAt, ProblemCode: w.ProblemCode, ProblemDetail: w.ProblemDetail,
		AcknowledgedAt: w.AcknowledgedAt, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}

// wmsBody is a kept body, a JSON object, passed through as stored.
func wmsBody(raw []byte) jx.Raw { return jx.Raw(raw) }
