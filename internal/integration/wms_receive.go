package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
// A goods receipt (S2q2) is posted the same way, for an order sent to
// that connector, through procurement's published API (wms_receipts.go).
// After that commit the WMS is told it is recorded, and acknowledged_at
// is set; a confirmation recorded already is acknowledged again.

// WMSDeliveries posts what a warehouse system reports of a delivery
// (shipping's Endpoints.Pick and PostGoodsIssue, which the composition
// root wires, so integration does not import shipping, a leaf).
type WMSDeliveries interface {
	// IssueReported records the picked quantities of the delivery's items
	// and posts its goods issue, in the caller's transaction.
	IssueReported(ctx context.Context, delivery uuid.UUID, picked []WMSPicked) (WMSPosted, error)
}

// WMSPicked is a quantity a WMS picked of a delivery's item, in its unit.
type WMSPicked struct {
	ItemNo         int32
	Quantity, Unit string
}

// WMSPosted is what a reported movement posted, a goods issue or a goods
// receipt: its material document.
type WMSPosted struct {
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

// pollTenants returns the tenants with active wms or tms connectors.
func (w *WMSExchange) pollTenants(ctx context.Context) ([]uuid.UUID, error) {
	tenants, err := database.Tenants(ctx, w.Pool, w.Begin, w.WorkerRole,
		`SELECT DISTINCT tenant_id FROM integration.connector WHERE kind = ANY($1) AND status = 'active' AND tenant_id IN `+database.ActiveTenants,
		[]string{WMS, TMS})
	if err != nil {
		return nil, fmt.Errorf("integration: list the tenants with wms connectors: %w", err)
	}
	return tenants, nil
}

// PollOnce polls every active wms and tms connector of every active tenant and
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

// pollTenant polls the tenant's active wms and tms connectors, by name.
func (w *WMSExchange) pollTenant(ctx context.Context, tenant uuid.UUID) (int, error) {
	var conns []usable
	if err := w.job(tenant, "read the wms connectors").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		cs, err := activeLogistics(ctx, tx, WMS, TMS)
		for _, c := range cs {
			if c.SecretRef != nil && (!w.RefuseStandIn || c.Adapter != StandInAdapter) {
				conns = append(conns, usable{WMSConnection: WMSConnection{Name: c.Name, Kind: c.Kind, WMSSettings: c.WMSSettings}, secretRef: *c.SecretRef, id: c.ID})
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
// acknowledges it. One that cannot be read, recorded or acknowledged now
// is reported and left for the next poll, and the others are taken still;
// an id the table cannot hold (empty, above 200 characters, holding a NUL
// or invalid UTF-8) is reported
// and never taken, as no record of it can be kept.
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
	var failed []error
	for _, id := range ids {
		recorded, err := w.take(ctx, tenant, wh, c, id)
		if recorded {
			n++
		}
		if err != nil {
			failed = append(failed, fmt.Errorf("confirmation %q: %w", id, err))
		}
	}
	return n, errors.Join(failed...)
}

// maxWMSMessageID is the longest message id integration.wms_message holds.
const maxWMSMessageID = 200

// take reads, records and acknowledges one confirmation and reports
// whether it recorded it.
func (w *WMSExchange) take(ctx context.Context, tenant uuid.UUID, wh Warehouse, c usable, id string) (bool, error) {
	if id == "" || utf8.RuneCountInString(id) > maxWMSMessageID || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
		return false, fmt.Errorf("integration: a confirmation id of %d characters, or holding a NUL or invalid UTF-8, is not one a message can be recorded under",
			utf8.RuneCountInString(id))
	}
	raw, readErr := wh.Confirmation(ctx, id)
	if readErr != nil && problem.From(readErr).Status >= 500 {
		return false, readErr
	}
	var recorded bool
	if err := w.job(tenant, "record a wms confirmation").Tx(ctx, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		var err error
		recorded, err = w.record(ctx, tx, tenant, c, id, raw, readErr)
		return err == nil, err
	}); err != nil {
		return false, err
	}
	return recorded, w.acknowledge(ctx, tenant, wh, c.id, id)
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
	legalEntity, document *uuid.UUID // its company and the delivery or purchase order it reports on
	number                *string
	body                  []byte
	status                string
	code, detail          *string // refused: the problem's, its detail at most 1000 characters
	kind                  string  // WMSGoodsIssue, or WMSGoodsReceipt for a message read as one; a tms connector's TMSShipment
	posted                *WMSPosted
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
	body, asReceived := keptBody(raw)
	if !asReceived && readErr == nil {
		readErr = unkept(id)
	}
	in := inbound{id: uuid.Must(uuid.NewV7()), connector: c.id, messageID: id, kind: WMSGoodsIssue, body: body}
	if c.Kind == TMS {
		in.kind = TMSShipment
	}
	err := uow.Act(ctx, tx, wmsActor(tenant), w.Now, func(ctx context.Context) error {
		if err := w.post(ctx, tx, &in, raw, readErr); err != nil {
			return err
		}
		return in.insert(ctx, tx, c.Name)
	})
	return err == nil, err
}

// keptBody is what is kept of a confirmation and whether it is the
// confirmation as received: its JSON object; or, when it is not one or
// holds a NUL (\u0000), which jsonb cannot store, its text as
// {"text": ...}, NULs and invalid UTF-8 dropped, at most 1000 characters
// of it.
func keptBody(raw []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	// The check also refuses an escaped backslash before the text u0000
	// ("\\u0000"), which jsonb could hold: rare, and refused visibly
	// rather than lost.
	if json.Unmarshal(raw, &fields) == nil && fields != nil && !bytes.Contains(raw, []byte(`\u0000`)) {
		return raw, true
	}
	text := []rune(strings.ReplaceAll(strings.ToValidUTF8(string(raw), ""), "\x00", ""))
	b, _ := json.Marshal(map[string]string{"text": string(text[:min(len(text), 1000)])}) // a map of strings always marshals
	return b, false
}

// unkept refuses a confirmation that could not be kept as received.
func unkept(id string) error {
	return problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON object the ERP can keep as received.")
}

// post reads the confirmation and posts it, in a savepoint: a refusal
// rolls the posting back (and the audit records it buffered) and is kept
// on in; a failure of the machinery (5xx, or one asking to be retried) is
// returned, so nothing is recorded and the next poll takes it again.
func (w *WMSExchange) post(ctx context.Context, tx pgx.Tx, in *inbound, raw []byte, readErr error) error {
	err := w.postAs(ctx, tx, in, raw, readErr)
	if err == nil {
		in.status = "posted"
		if in.kind == TMSShipment {
			in.status = "recorded" // a shipment posts nothing
		}
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

// postAs reads the confirmation by its type and posts it: a goods issue
// or a goods receipt (S2q2); a tms connector's, a shipment (S2r).
func (w *WMSExchange) postAs(ctx context.Context, tx pgx.Tx, in *inbound, raw []byte, readErr error) error {
	if in.kind == TMSShipment {
		sh, err := readShipment(in.messageID, raw, readErr)
		if err != nil {
			return err
		}
		return w.postShipment(ctx, tx, in, sh)
	}
	typ, err := readType(in.messageID, raw, readErr)
	if err != nil {
		return err
	}
	if typ == WMSGoodsReceipt {
		in.kind = WMSGoodsReceipt
		gr, err := readGoodsReceipt(in.messageID, raw)
		if err != nil {
			return err
		}
		return w.postReceipt(ctx, tx, in, gr)
	}
	gi, err := readGoodsIssue(in.messageID, raw)
	if err != nil {
		return err
	}
	return w.postIssue(ctx, tx, in, gi)
}

// readType reads a confirmation's message type, goods_issue or
// goods_receipt, checking it names the message id the WMS listed it
// under.
func readType(id string, raw []byte, readErr error) (string, error) {
	if readErr != nil {
		return "", readErr
	}
	var head struct {
		MessageID string `json:"message_id"`
		Type      string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "", problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON message of the WMS protocol.")
	}
	switch {
	case head.Type != WMSGoodsIssue && head.Type != WMSGoodsReceipt:
		return "", problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is of type "+fmt.Sprintf("%q", head.Type)+
			": only goods_issue and goods_receipt are read.")
	case head.MessageID != id:
		return "", problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names another message id, "+fmt.Sprintf("%q", head.MessageID)+".")
	}
	return head.Type, nil
}

// readGoodsIssue reads a confirmation of type goods_issue.
func readGoodsIssue(id string, raw []byte) (WMSIssueReport, error) {
	var gi WMSIssueReport
	if err := json.Unmarshal(raw, &gi); err != nil {
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" is not a JSON goods_issue message of the WMS protocol.")
	}
	if gi.DeliveryID == uuid.Nil {
		return gi, problem.Unprocessable("integration.wms_message_unreadable", "Confirmation "+id+" names no outbound_delivery_id.")
	}
	return gi, nil
}

// sentTo refuses a document the connector was not sent of the outbound
// kind (unknown, integration.wms_unknown_delivery or
// integration.wms_unknown_order): a WMS reports only what it was given,
// so it cannot post another plant's. It notes the document on in and
// reads the message as sent into sent.
func (w *WMSExchange) sentTo(ctx context.Context, tx pgx.Tx, in *inbound, kind string, doc uuid.UUID, sent any, unknown error) error {
	var le uuid.UUID
	var number string
	err := tx.QueryRow(ctx, `SELECT legal_entity_id, document_number, body FROM integration.wms_message
		WHERE connector_id = $1 AND direction = 'outbound' AND kind = $2 AND document_id = $3`, in.connector, kind, doc).Scan(&le, &number, sent)
	if noRows(err) {
		return unknown
	}
	if err != nil {
		return fmt.Errorf("integration: the document of wms confirmation %s: %w", in.messageID, err)
	}
	in.legalEntity, in.document, in.number = &le, &doc, &number
	return nil
}

// pickedOf is what the report picked of each item, and nothing of each
// item it was sent and does not name, so a quantity picked before (by a
// person) is not issued with it.
func pickedOf(gi WMSIssueReport, sent []WMSDeliveryItem) []WMSPicked {
	picked := make([]WMSPicked, 0, len(gi.Items)+len(sent))
	named := map[int32]bool{}
	for _, it := range gi.Items {
		picked = append(picked, WMSPicked{ItemNo: it.ItemNo, Quantity: it.Quantity, Unit: it.Unit})
		named[it.ItemNo] = true
	}
	for _, it := range sent {
		if !named[it.ItemNo] {
			picked = append(picked, WMSPicked{ItemNo: it.ItemNo, Quantity: "0", Unit: it.Unit})
		}
	}
	return picked
}

// postIssue posts a goods issue the WMS reports, of a delivery it was
// sent, in a savepoint, acting for the delivery's company alone.
func (w *WMSExchange) postIssue(ctx context.Context, tx pgx.Tx, in *inbound, gi WMSIssueReport) error {
	var sent WMSDelivery
	if err := w.sentTo(ctx, tx, in, WMSOutboundDelivery, gi.DeliveryID, &sent, problem.Unprocessable("integration.wms_unknown_delivery",
		"Confirmation "+in.messageID+" names an outbound delivery this warehouse system was not sent.")); err != nil {
		return err
	}
	picked := pickedOf(gi, sent.Items)
	return w.inSavepoint(ctx, tx, in, func(ctx context.Context) (*WMSPosted, error) {
		p, err := w.Deliveries.IssueReported(authz.ActingFor(ctx, *in.legalEntity), gi.DeliveryID, picked)
		return &p, err
	})
}

// inSavepoint posts in a savepoint and notes what it posted on in (a
// material document; nil for a shipment, which posts nothing); a refusal
// rolls it back, with the audit records it buffered, and is returned, as
// is a failure of the machinery.
func (w *WMSExchange) inSavepoint(ctx context.Context, tx pgx.Tx, in *inbound, post func(context.Context) (*WMSPosted, error)) error {
	mark := audit.Mark(ctx)
	if _, err := tx.Exec(ctx, `SAVEPOINT wms_posting`); err != nil {
		return fmt.Errorf("integration: open the savepoint of wms confirmation %s: %w", in.messageID, err)
	}
	posted, err := post(ctx)
	if err == nil {
		in.posted = posted
		_, err := tx.Exec(ctx, `RELEASE SAVEPOINT wms_posting`)
		return errs.Wrapf(err, "integration: release the savepoint of wms confirmation %s", in.messageID)
	}
	if p := problem.From(err); p.Status >= 500 || p.RetryAfter > 0 {
		return err
	}
	if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT wms_posting`); rerr != nil {
		return fmt.Errorf("integration: roll back the refused posting of wms confirmation %s: %w", in.messageID, rerr)
	}
	audit.DiscardFrom(ctx, mark)
	return err
}

// insert writes the recorded confirmation with its audit record.
func (in inbound) insert(ctx context.Context, tx pgx.Tx, connectorName string) error {
	var docNumber *string
	var docID *uuid.UUID
	if in.posted != nil {
		docID, docNumber = &in.posted.MaterialDocumentID, &in.posted.MaterialDocumentNumber
	}
	if _, err := tx.Exec(ctx, `INSERT INTO integration.wms_message (tenant_id, id, connector_id, direction, message_id, kind, legal_entity_id, document_id,
			document_number, body, status, problem_code, problem_detail, material_document_id, material_document_number)
		VALUES (platform.require_tenant(), $1, $2, 'inbound', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		in.id, in.connector, in.messageID, in.kind, in.legalEntity, in.document, in.number, in.body, in.status, in.code, in.detail, docID, docNumber); err != nil {
		return fmt.Errorf("integration: record wms confirmation %s: %w", in.messageID, err)
	}
	action := map[string]string{"posted": "integration.wms_message.post", "recorded": "integration.wms_message.record",
		"refused": "integration.wms_message.refuse"}[in.status]
	details := map[string]any{"connector_id": in.connector, "connector_name": connectorName, "message_id": in.messageID, "kind": in.kind,
		"material_document_number": docNumber, "code": in.code}
	if in.kind == WMSGoodsReceipt {
		details["purchase_order_id"], details["purchase_order_number"] = in.document, in.number
	} else {
		details["outbound_delivery_id"], details["outbound_delivery_number"] = in.document, in.number
	}
	return audit.Record(ctx, action, "integration.wms_message", in.id, details)
}
