package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/internal/platform/audit"
	"github.com/mustie7iy/agentic-erp/internal/platform/authz"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
	"github.com/mustie7iy/agentic-erp/internal/platform/uow"
)

// The movements a warehouse system reports back (S2q1): a poll lists, for
// each active wms connector, the confirmations its WMS holds and has not
// had acknowledged, reads each, and records it once per connector and
// message id, in the transaction that posts it: an inbound row of
// integration.wms_message, the inbox of the WMS's messages, so a
// confirmation read twice (a lost acknowledgement, a second replica) is
// posted once. A goods issue is posted for a delivery sent to that
// connector alone: its picked quantities, then its goods issue, through
// shipping's published API as the exchange's principal acting for the
// delivery's company. A refusal (shipping's, or the confirmation not
// readable, of another type or of a delivery the connector was not sent)
// is recorded as refused with its problem, nothing of the posting kept.
// After that commit the WMS is told it is recorded, and acknowledged_at
// is set; a confirmation recorded already is acknowledged again.

// WMSDeliveries posts what a warehouse system reports of a delivery
// (shipping's Endpoints.Pick and PostGoodsIssue, which the composition
// root wires, so integration does not import shipping, a leaf).
type WMSDeliveries interface {
	// IssueReported records the picked quantities of the delivery's items
	// and posts its goods issue, in the caller's transaction.
	IssueReported(ctx context.Context, delivery uuid.UUID, picked []WMSPicked) (WMSIssued, error)
}

// WMSPicked is a quantity a WMS picked of a delivery's item, in its unit.
type WMSPicked struct {
	ItemNo         int32
	Quantity, Unit string
}

// WMSIssued is the goods issue a reported movement posted: its material
// document.
type WMSIssued struct {
	MaterialDocumentID     uuid.UUID
	MaterialDocumentNumber string
}

// WMSIssueReport is the message a warehouse system reports a delivery's
// goods issue with (message type goods_issue, version 1): its own message
// id, the delivery it was sent by id, and what it picked of each item, in
// the item's unit; the items it does not name are picked at nothing.
type WMSIssueReport struct {
	MessageID  string    `json:"message_id"`
	Type       string    `json:"type"`
	DeliveryID uuid.UUID `json:"outbound_delivery_id"`
	Items      []struct {
		ItemNo   int32  `json:"item_no"`
		Quantity string `json:"quantity"`
		Unit     string `json:"unit"`
	} `json:"items"`
}

// pollTenants returns the tenants with active wms connectors.
func (w *WMSExchange) pollTenants(ctx context.Context) ([]uuid.UUID, error) {
	tenants, err := database.Tenants(ctx, w.Pool, w.Begin, w.WorkerRole,
		`SELECT DISTINCT tenant_id FROM integration.connector WHERE kind = $1 AND status = 'active' AND tenant_id IN `+database.ActiveTenants, WMS)
	if err != nil {
		return nil, fmt.Errorf("integration: list the tenants with wms connectors: %w", err)
	}
	return tenants, nil
}

// PollOnce polls every active wms connector of every active tenant and
// returns how many confirmations it recorded. A connector whose WMS
// cannot be read is reported and the others are polled still.
func (w *WMSExchange) PollOnce(ctx context.Context) (int, error) {
	tenants, err := w.pollTenants(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	var failed []error
	for _, t := range tenants {
		got, err := w.pollTenant(ctx, t)
		n += got
		if err != nil {
			failed = append(failed, fmt.Errorf("integration: wms poll of tenant %s: %w", t, err))
		}
	}
	return n, errors.Join(failed...)
}

// pollTenant polls the tenant's active wms connectors, by name.
func (w *WMSExchange) pollTenant(ctx context.Context, tenant uuid.UUID) (int, error) {
	var conns []usable
	if err := w.job(tenant, "read the wms connectors").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		cs, err := activeWMS(ctx, tx)
		for _, c := range cs {
			if c.SecretRef != nil && (!w.RefuseStandIn || c.Adapter != StandInAdapter) {
				conns = append(conns, usable{WMSConnection: WMSConnection{Name: c.Name, WMSSettings: c.WMSSettings}, secretRef: *c.SecretRef, id: c.ID})
			}
		}
		return false, err
	}); err != nil {
		return 0, err
	}
	n := 0
	var failed []error
	for _, c := range conns {
		got, err := w.pollConnector(ctx, tenant, c)
		n += got
		if err != nil {
			failed = append(failed, fmt.Errorf("connector %s: %w", c.Name, err))
		}
	}
	return n, errors.Join(failed...)
}

// pollConnector records each confirmation the connector's WMS holds, then
// acknowledges it; it stops at the first that cannot be read or recorded,
// which the next poll takes again.
func (w *WMSExchange) pollConnector(ctx context.Context, tenant uuid.UUID, c usable) (int, error) {
	wh, err := w.warehouse(ctx, tenant, c)
	if err != nil {
		return 0, err
	}
	ids, err := wh.Confirmations(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		raw, readErr := wh.Confirmation(ctx, id)
		if readErr != nil && problem.From(readErr).Status >= 500 {
			return n, readErr
		}
		var recorded bool
		if err := w.job(tenant, "record a wms confirmation").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
			var err error
			recorded, err = w.record(ctx, tx, tenant, c, id, raw, readErr)
			return err == nil, err
		}); err != nil {
			return n, err
		}
		if recorded {
			n++
		}
		if err := w.acknowledge(ctx, tenant, wh, c.id, id); err != nil {
			return n, err
		}
	}
	return n, nil
}

// acknowledge tells the WMS the confirmation is recorded and sets its
// acknowledged_at once that call succeeded.
func (w *WMSExchange) acknowledge(ctx context.Context, tenant uuid.UUID, wh Warehouse, connector uuid.UUID, id string) error {
	if err := wh.Acknowledge(ctx, id); err != nil {
		return err
	}
	return w.job(tenant, "acknowledge a wms confirmation").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		_, err := tx.Exec(ctx, `UPDATE integration.wms_message SET acknowledged_at = $3, version = version + 1
			WHERE connector_id = $1 AND direction = 'inbound' AND message_id = $2 AND acknowledged_at IS NULL`, connector, id, clock.Now(w.Now))
		if err != nil {
			return false, fmt.Errorf("integration: mark wms confirmation %s acknowledged: %w", id, err)
		}
		return true, nil
	})
}

// inbound is a confirmation as recorded: what it is and its outcome.
type inbound struct {
	id, connector         uuid.UUID
	messageID             string
	legalEntity, delivery *uuid.UUID
	number                *string
	body                  []byte
	status                string
	code, detail          *string // refused: the problem's, its detail at most 1000 characters
	issued                *WMSIssued
}

// record records the confirmation once, under a lock of its connector and
// message id, so two pollers cannot both post it; it reports whether this
// call recorded it. readErr is a refusal of its reading (too large).
func (w *WMSExchange) record(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, c usable, id string, raw []byte, readErr error) (bool, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('integration.wms_message:' || $1::text || ':' || $2, 0))`, c.id, id); err != nil {
		return false, fmt.Errorf("integration: lock wms confirmation %s: %w", id, err)
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM integration.wms_message WHERE connector_id = $1 AND direction = 'inbound' AND message_id = $2)`,
		c.id, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("integration: read wms confirmation %s: %w", id, err)
	}
	if exists {
		return false, nil
	}
	in := inbound{id: uuid.Must(uuid.NewV7()), connector: c.id, messageID: id, body: keptBody(raw)}
	err := uow.Act(ctx, tx, wmsActor(tenant), w.Now, func(ctx context.Context) error {
		if err := w.post(ctx, tx, &in, raw, readErr); err != nil {
			return err
		}
		return in.insert(ctx, tx, c.Name)
	})
	return err == nil, err
}

// keptBody is what is kept of a confirmation: its JSON object, or, when it
// is not one, its text as {"text": ...}, at most 1000 characters of it.
func keptBody(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields != nil {
		return raw
	}
	text := []rune(string(raw))
	if !utf8.Valid(raw) {
		text = []rune("(not text)")
	}
	b, _ := json.Marshal(map[string]string{"text": string(text[:min(len(text), 1000)])}) // a map of strings always marshals
	return b
}

// post reads the confirmation and posts it, in a savepoint: a refusal
// rolls the posting back (and the audit records it buffered) and is kept
// on in; a failure of the machinery (5xx, or one asking to be retried) is
// returned, so nothing is recorded and the next poll takes it again.
func (w *WMSExchange) post(ctx context.Context, tx pgx.Tx, in *inbound, raw []byte, readErr error) error {
	gi, err := readGoodsIssue(in.messageID, raw, readErr)
	if err == nil {
		err = w.sentTo(ctx, tx, in, gi.DeliveryID)
	}
	if err == nil {
		err = w.issue(ctx, tx, in, gi)
	}
	if err == nil {
		in.status = "posted"
		return nil
	}
	p := problem.From(err)
	if p.Status >= 500 || p.RetryAfter > 0 {
		return err
	}
	code, detail := p.Code, []rune(p.Detail)
	text := string(detail[:min(len(detail), 1000)])
	in.status, in.code, in.detail = "refused", &code, &text
	return nil
}

// readGoodsIssue reads a confirmation as a goods issue of the message id
// the WMS listed it under.
func readGoodsIssue(id string, raw []byte, readErr error) (WMSIssueReport, error) {
	if readErr != nil {
		return WMSIssueReport{}, readErr
	}
	var gi WMSIssueReport
	if err := json.Unmarshal(raw, &gi); err != nil {
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON message of the WMS protocol.")
	}
	switch {
	case gi.Type != WMSGoodsIssue:
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is of type "+fmt.Sprintf("%q", gi.Type)+": only goods_issue is read.")
	case gi.MessageID != id:
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names another message id, "+fmt.Sprintf("%q", gi.MessageID)+".")
	case gi.DeliveryID == uuid.Nil:
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names no outbound_delivery_id.")
	}
	return gi, nil
}

// sentTo refuses a delivery the connector was not sent
// (integration.wms_unknown_delivery): a WMS reports only what it was given
// to pick, so it cannot post another plant's delivery.
func (w *WMSExchange) sentTo(ctx context.Context, tx pgx.Tx, in *inbound, delivery uuid.UUID) error {
	var le uuid.UUID
	var number string
	err := tx.QueryRow(ctx, `SELECT legal_entity_id, document_number FROM integration.wms_message
		WHERE connector_id = $1 AND direction = 'outbound' AND kind = $2 AND document_id = $3`, in.connector, WMSOutboundDelivery, delivery).Scan(&le, &number)
	if noRows(err) {
		return problem.Unprocessable("integration.wms_unknown_delivery",
			"Confirmation "+in.messageID+" names an outbound delivery this warehouse system was not sent.")
	}
	if err != nil {
		return fmt.Errorf("integration: the delivery of wms confirmation %s: %w", in.messageID, err)
	}
	in.legalEntity, in.delivery, in.number = &le, &delivery, &number
	return nil
}

// issue posts the goods issue in a savepoint, acting for the delivery's
// company alone.
func (w *WMSExchange) issue(ctx context.Context, tx pgx.Tx, in *inbound, gi WMSIssueReport) error {
	picked := make([]WMSPicked, len(gi.Items))
	for i, it := range gi.Items {
		picked[i] = WMSPicked{ItemNo: it.ItemNo, Quantity: it.Quantity, Unit: it.Unit}
	}
	mark := audit.Mark(ctx)
	if _, err := tx.Exec(ctx, `SAVEPOINT wms_goods_issue`); err != nil {
		return fmt.Errorf("integration: open the savepoint of wms confirmation %s: %w", in.messageID, err)
	}
	issued, err := w.Deliveries.IssueReported(authz.ActingFor(ctx, *in.legalEntity), gi.DeliveryID, picked)
	if err == nil {
		in.issued = &issued
		_, err := tx.Exec(ctx, `RELEASE SAVEPOINT wms_goods_issue`)
		return errs.Wrapf(err, "integration: release the savepoint of wms confirmation %s", in.messageID)
	}
	if p := problem.From(err); p.Status >= 500 || p.RetryAfter > 0 {
		return err
	}
	if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT wms_goods_issue`); rerr != nil {
		return fmt.Errorf("integration: roll back the refused goods issue of wms confirmation %s: %w", in.messageID, rerr)
	}
	audit.DiscardFrom(ctx, mark)
	return err
}

// insert writes the recorded confirmation with its audit record.
func (in inbound) insert(ctx context.Context, tx pgx.Tx, connectorName string) error {
	var docNumber *string
	var docID *uuid.UUID
	if in.issued != nil {
		docID, docNumber = &in.issued.MaterialDocumentID, &in.issued.MaterialDocumentNumber
	}
	if _, err := tx.Exec(ctx, `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, body, status, problem_code, problem_detail, material_document_id, material_document_number)
		VALUES (platform.require_tenant(), $1, $2, 'inbound', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		in.id, in.connector, in.messageID, WMSGoodsIssue, in.legalEntity, in.delivery, in.number, in.body, in.status, in.code, in.detail, docID, docNumber); err != nil {
		return fmt.Errorf("integration: record wms confirmation %s: %w", in.messageID, err)
	}
	action := "integration.wms_message.post"
	if in.status == "refused" {
		action = "integration.wms_message.refuse"
	}
	return audit.Record(ctx, action, "integration.wms_message", in.id, map[string]any{"connector_id": in.connector, "connector_name": connectorName,
		"message_id": in.messageID, "outbound_delivery_id": in.delivery, "outbound_delivery_number": in.number, "material_document_number": docNumber,
		"code": in.code})
}
