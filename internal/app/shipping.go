package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/configuration"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/partner"
	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/sales"
	"github.com/mustie7iy/agentic-erp/internal/shipping"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Shipping is the shipping context as the API wires it (ADR-0083 §1): it
// reads sales orders through sales' published read and has sales hold
// what its deliveries take, reads materials through materials' published
// API and stock through inventory's, and tenancy's companies and org
// units, the ledger's fiscal calendar and the configuration's number
// ranges and company dates through its ports, and whether a trade
// compliance service screens a plant through integration's (S2s).
func Shipping(cfg Config) *shipping.Endpoints {
	return &shipping.Endpoints{Pages: cfg.Pages, Orders: sales.Orders{}, Shipments: Sales(cfg), Stock: Inventory(cfg), Materials: materials.Materials{},
		Partners: partner.Payees{}, LegalEntities: tenancy.Directory{}, Numbers: configuration.Numbers{}, Calendar: calendar(), Settings: configuration.Settings{},
		Screening: integration.TradeScreening{}, People: LedgerPeople{}, Now: cfg.Now}
}

// WMS is the exchange with warehouse and transport systems and trade
// compliance services (ADR-0082 As built S2q, S2r, S2s) as the server runs it: its messages sent and its
// systems polled through client (integration.NewWMSClient), their
// credentials resolved by cfg.Secrets, the goods issues they report
// posted through shipping's published API (wmsDeliveries), the goods
// receipts through procurement's (WMSOrders, S2q2) and the shipments
// recorded through shipping's (tmsDeliveries, S2r), as are the screenings
// (gtsDeliveries, S2s), so integration imports neither. The server sets
// the pool and roles.
func WMS(cfg Config, client *egress.Client) *integration.WMSExchange {
	ship := Shipping(cfg)
	return &integration.WMSExchange{Secrets: resolver(cfg.Secrets), Adapter: integration.WMSAdapters(client), Deliveries: wmsDeliveries{ship},
		Orders: WMSOrders(cfg), Shipments: tmsDeliveries{ship}, Screenings: gtsDeliveries{ship}, RefuseStandIn: cfg.RefuseEBICS, Now: cfg.Now}
}

// gtsDeliveries records a trade compliance service's answer for a
// delivery (shipping's RecordScreening, S2s).
type gtsDeliveries struct{ e *shipping.Endpoints }

func (a gtsDeliveries) ScreeningReported(ctx context.Context, delivery uuid.UUID, s integration.GTSScreening) error {
	_, err := a.e.RecordScreening(ctx, delivery, shipping.ScreeningInput{Result: s.Result, ReasonCode: s.ReasonCode, ScreenedAt: s.ScreenedAt})
	return err
}

// tmsDeliveries records the shipment a transport system reports of a
// delivery (shipping's RecordShipment, S2r).
type tmsDeliveries struct{ e *shipping.Endpoints }

func (a tmsDeliveries) ShipmentReported(ctx context.Context, delivery uuid.UUID, s integration.TMSShipmentReported) error {
	_, err := a.e.RecordShipment(ctx, delivery, shipping.ShipmentInput{Number: s.Number, Carrier: s.Carrier, FreightAmount: s.FreightAmount,
		Currency: s.Currency})
	return err
}

// wmsDeliveries posts what a warehouse system reports of a delivery: its
// picked quantities and then its goods issue, at the delivery's version
// as it is (the exchange holds no ETag; both take the delivery's row
// lock).
type wmsDeliveries struct{ e *shipping.Endpoints }

func (a wmsDeliveries) IssueReported(ctx context.Context, delivery uuid.UUID, picked []integration.WMSPicked) (integration.WMSPosted, error) {
	anyVersion := func(int32) error { return nil }
	in := make([]shipping.PickInput, len(picked))
	for i, p := range picked {
		in[i] = shipping.PickInput{ItemNo: p.ItemNo, Quantity: p.Quantity, Unit: p.Unit}
	}
	if _, err := a.e.Pick(ctx, delivery, anyVersion, in); err != nil {
		return integration.WMSPosted{}, err
	}
	d, err := a.e.PostGoodsIssue(ctx, delivery, anyVersion)
	if err != nil {
		return integration.WMSPosted{}, err
	}
	// A posted goods issue names its material document (shipping's
	// outbound_delivery check).
	return integration.WMSPosted{MaterialDocumentID: *d.MaterialDocumentID, MaterialDocumentNumber: *d.MaterialDocumentNumber}, nil
}
