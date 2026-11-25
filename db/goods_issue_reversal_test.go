package db

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// reverse reverses the scene's delivery's goods issue as shipping's
// ReverseGoodsIssue does (S2b3, 000471): its status, the reversal's day
// and material document.
func (s shippingScene) reverse(status, day, document, number any) stmt {
	return step(`UPDATE shipping.outbound_delivery SET status = $2, goods_issue_reversal_date = $3, reversal_material_document_id = $4,
		reversal_material_document_number = $5, version = version + 1 WHERE id = $1`, s.delivery, status, day, document, number)
}

// issued is the scene's delivery, goods-issued on 2026-11-20.
func (s shippingScene) issued() []stmt {
	return append(s.made(), step(shippingIssue, s.delivery, s.document))
}

// TestGoodsIssueReversalDeliveryRules (S2b3, migrations 000471 and
// 000472; ADR-0083 As built S2b3): a goods-issued delivery's goods issue
// is reversed by its status, goods_issue_reversed, and the reversal's
// day, on or after the goods issue's, and material document, its number
// of 1 to 40 characters, all three together, its goods issue kept; it
// changes by nothing else, moving its version, and a reversed delivery
// changes no more but for its shipment. A delivery not issued is not
// reversed.
func TestGoodsIssueReversalDeliveryRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s shippingScene) []stmt
		want string // empty: allowed
	}{
		{"reversed", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"))
		}, ""},
		{"reversed on its goods issue's day", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-20", id(), "MD2"))
		}, ""},
		{"its shipment recorded after", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"), s.shipmentSet("SH1", "NWT", 100, "EUR"))
		}, ""},
		{"reversed before its goods issue", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-19", id(), "MD2"))
		}, "outbound_delivery_goods_issue_reversal"},
		{"reversed without a day", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", nil, id(), "MD2"))
		},
			"outbound_delivery_goods_issue_reversal"},
		{"reversed without a document", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", nil, "MD2"))
		}, "outbound_delivery_goods_issue_reversal"},
		{"reversed without a number", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), nil))
		}, "outbound_delivery_goods_issue_reversal"},
		{"a number of 41 characters", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), strings.Repeat("M", 41)))
		}, "outbound_delivery_reversal_material_document_number_check"},
		{"an empty number", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), ""))
		},
			"outbound_delivery_reversal_material_document_number_check"},
		{"a reversal named while goods-issued", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issued", "2026-11-21", id(), "MD2"))
		}, "its goods issue is posted, so it changes no more"},
		{"a delivery not issued", func(s shippingScene) []stmt {
			return append(s.made(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"))
		}, "outbound_delivery_goods_issue"},
		{"its goods issue changed with it", func(s shippingScene) []stmt {
			return append(s.issued(), step(`UPDATE shipping.outbound_delivery SET status = 'goods_issue_reversed', goods_issue_reversal_date = '2026-11-21',
				reversal_material_document_id = gen_random_uuid(), reversal_material_document_number = 'MD2', material_document_number = 'MD9',
				version = version + 1 WHERE id = $1`, s.delivery))
		}, "its goods issue is posted, so it changes no more"},
		{"changed after", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"), s.reverse("goods_issue_reversed", "2026-11-22", id(), "MD3"))
		}, "its goods issue is posted, so it changes no more"},
		{"back to goods-issued", func(s shippingScene) []stmt {
			return append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"), s.reverse("goods_issued", nil, nil, nil))
		}, "its goods issue is posted, so it changes no more"},
		{"without moving the version", func(s shippingScene) []stmt {
			return append(s.issued(), step(`UPDATE shipping.outbound_delivery SET status = 'goods_issue_reversed', goods_issue_reversal_date = '2026-11-21',
				reversal_material_document_id = gen_random_uuid(), reversal_material_document_number = 'MD2' WHERE id = $1`, s.delivery))
		}, "optimistic lock"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error { return all(tx, c.run(s)...) })
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestGoodsIssueReversalDeliveryAsOwner (S2b3, migration 000471): past the
// grants, as the owner, a reversed delivery's text changes no more, nor
// its items.
func TestGoodsIssueReversalDeliveryAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	s := shippingScene{tenant: id(), company: id(), delivery: id(), document: id()}
	for name, c := range map[string]struct {
		st   stmt
		want string
	}{
		"its number": {step(`UPDATE shipping.outbound_delivery SET number = 'OD9', version = version + 1 WHERE id = $1`, s.delivery),
			"outbound_delivery_issued_fixed"},
		"an item's picked quantity": {step(`UPDATE shipping.outbound_delivery_item SET picked_quantity = 1 WHERE outbound_delivery_id = $1`, s.delivery),
			"outbound_delivery_item_issued_fixed"},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, append(s.issued(), s.reverse("goods_issue_reversed", "2026-11-21", id(), "MD2"))...); err != nil {
				t.Fatal(err)
			}
			failsOn(t, exec(tx, c.st.sql, c.st.args...), c.want)
		})
	}
}

// reversalScene is a tenant's confirmed sales order of 5 EA with a goods
// issue's delivery of 2 EA on 2026-03-14, by id.
type reversalScene struct{ tenant, order, delivery uuid.UUID }

const (
	reversalOrder = `INSERT INTO sales.sales_order (tenant_id, id, document_number, legal_entity_id, customer_id, order_date,
		currency, payment_term_code, net_amount, status, created_by, updated_by)
		VALUES ($1, $2, 'SO1', gen_random_uuid(), gen_random_uuid(), '2026-03-12', 'EUR', 'NT30', 100, 'draft', $1, $1)`
	reversalOrderItem = `INSERT INTO sales.sales_order_item (tenant_id, sales_order_id, item_no, description, quantity, uom_code, unit_price, price_unit,
		net_amount, account_id) VALUES ($1, $2, 1, 'Bracket', 5, 'EA', 100, 1, 100, gen_random_uuid())`
	reversalConfirm  = `UPDATE sales.sales_order SET status = 'confirmed', version = version + 1, updated_by = tenant_id WHERE id = $1`
	reversalDelivery = `INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, outbound_delivery_id, outbound_delivery_number,
		material_document_id, material_document_number, created_by) VALUES ($1, $2, $3, 1, '2026-03-14', $4, 'OD1', gen_random_uuid(), 'MD1', $1)`
	reversalRow = `INSERT INTO sales.delivery_reversal (tenant_id, delivery_id, sales_order_id, reversal_date, material_document_id, material_document_number,
		created_by) VALUES ($1, $2, $3, $4, gen_random_uuid(), $5, $1)`
)

// delivered is the scene's order with its goods issue's delivery, which
// names an outbound delivery unless byHand.
func (s reversalScene) delivered(byHand bool) []stmt {
	var outbound any = id()
	if byHand {
		outbound = nil
	}
	delivery := step(reversalDelivery, s.tenant, s.delivery, s.order, outbound)
	if byHand {
		delivery = step(`INSERT INTO sales.delivery (tenant_id, id, sales_order_id, delivery_no, delivery_date, created_by)
			VALUES ($1, $2, $3, 1, '2026-03-14', $1)`, s.tenant, s.delivery, s.order)
	}
	return []stmt{step(reversalOrder, s.tenant, s.order), step(reversalOrderItem, s.tenant, s.order), step(reversalConfirm, s.order), delivery,
		step(`INSERT INTO sales.delivery_item (tenant_id, delivery_id, sales_order_id, order_item_no, quantity) VALUES ($1, $2, $3, 1, 2)`,
			s.tenant, s.delivery, s.order)}
}

// reversal reverses the scene's delivery on the day, naming the number.
func (s reversalScene) reversal(day, number string) stmt {
	return step(reversalRow, s.tenant, s.delivery, s.order, day, number)
}

// TestDeliveryReversalRules (S2b3, migration 000471; ADR-0083 As built
// S2b3): a goods issue's delivery of an order is reversed once, on or
// after its day, naming the 602 by a number of 1 to 40 characters; the
// reversal takes its quantity off the order item's delivered quantity,
// never below what is billed, and moves the order's version. A delivery
// confirmed by hand is not reversed, nor one of another order. The
// application neither changes nor deletes a reversal.
func TestDeliveryReversalRules(t *testing.T) {
	need(t)
	cases := []struct {
		name string
		run  func(s reversalScene) []stmt
		want string // empty: allowed
	}{
		{"reversed", func(s reversalScene) []stmt { return append(s.delivered(false), s.reversal("2026-03-20", "MD2")) }, ""},
		{"reversed on its day", func(s reversalScene) []stmt { return append(s.delivered(false), s.reversal("2026-03-14", "MD2")) }, ""},
		{"reversed before its day", func(s reversalScene) []stmt { return append(s.delivered(false), s.reversal("2026-03-13", "MD2")) },
			"dated before its delivery"},
		{"a delivery by hand", func(s reversalScene) []stmt { return append(s.delivered(true), s.reversal("2026-03-20", "MD2")) },
			"only a goods issue's delivery is reversed"},
		{"reversed twice", func(s reversalScene) []stmt {
			return append(s.delivered(false), s.reversal("2026-03-20", "MD2"), s.reversal("2026-03-21", "MD3"))
		}, "delivery_reversal_pkey"},
		{"a number of 41 characters", func(s reversalScene) []stmt {
			return append(s.delivered(false), s.reversal("2026-03-20", strings.Repeat("M", 41)))
		}, "delivery_reversal_material_document_number_check"},
		{"an empty number", func(s reversalScene) []stmt { return append(s.delivered(false), s.reversal("2026-03-20", "")) },
			"delivery_reversal_material_document_number_check"},
		{"of another order", func(s reversalScene) []stmt {
			return append(s.delivered(false), step(reversalRow, s.tenant, s.delivery, id(), "2026-03-20", "MD2"))
		}, "delivery_reversal_delivery_fkey"},
		{"changed", func(s reversalScene) []stmt {
			return append(s.delivered(false), s.reversal("2026-03-20", "MD2"), step(`UPDATE sales.delivery_reversal SET reversal_date = '2026-03-21'`))
		}, "permission denied"},
		{"deleted", func(s reversalScene) []stmt {
			return append(s.delivered(false), s.reversal("2026-03-20", "MD2"), step(`DELETE FROM sales.delivery_reversal`))
		}, "permission denied"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := reversalScene{tenant: id(), order: id(), delivery: id()}
			err := asApp(t, s.tenant, func(tx pgx.Tx) error {
				if err := all(tx, c.run(s)...); err != nil {
					return err
				}
				if c.want != "" {
					return nil
				}
				var delivered, version string
				if err := tx.QueryRow(context.Background(), `SELECT trim_scale(i.delivered_quantity)::text, o.version::text FROM sales.sales_order o
					JOIN sales.sales_order_item i ON i.tenant_id = o.tenant_id AND i.sales_order_id = o.id WHERE o.id = $1`, s.order).Scan(&delivered, &version); err != nil {
					return err
				}
				if delivered != "0" || version != "4" {
					t.Errorf("the order after the reversal: got %s delivered, version %s; want 0, version 4 (created, confirmed, delivered, reversed)",
						delivered, version)
				}
				return nil
			})
			if c.want != "" {
				failsWith(t, err, c.want)
				return
			}
			if err != nil {
				t.Fatalf("got %v, want no error", err)
			}
		})
	}
}

// TestDeliveryReversalAsOwner (S2b3, migration 000471): past the
// application's checks, as the owner, a reversal that would leave the
// order item billed beyond what is delivered is refused, and one of an
// order no longer confirmed; a reversal is insert-only.
func TestDeliveryReversalAsOwner(t *testing.T) {
	need(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		set  string
		want string
	}{
		"billed":        {`UPDATE sales.sales_order_item SET billed_quantity = 1 WHERE sales_order_id = $1`, "sales_order_item_quantities"},
		"not confirmed": {`UPDATE sales.sales_order SET status = 'draft' WHERE id = $1`, "delivery_order_confirmed"},
	} {
		t.Run(name, func(t *testing.T) {
			s := reversalScene{tenant: id(), order: id(), delivery: id()}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, s.delivered(false)...); err != nil {
				t.Fatal(err)
			}
			// The rule under test is the reversal's, so the set-up
			// skips the order's own triggers to reach the state.
			if err := all(tx, step(`SET LOCAL session_replication_role = replica`), step(c.set, s.order),
				step(`SET LOCAL session_replication_role = origin`)); err != nil {
				t.Fatal(err)
			}
			st := s.reversal("2026-03-20", "MD2")
			failsOn(t, exec(tx, st.sql, st.args...), c.want)
		})
	}
	for name, change := range map[string]string{
		"changed": `UPDATE sales.delivery_reversal SET reversal_date = '2026-03-21'`,
		"deleted": `DELETE FROM sales.delivery_reversal`,
	} {
		t.Run(name, func(t *testing.T) {
			s := reversalScene{tenant: id(), order: id(), delivery: id()}
			tx, err := testDB.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", s.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := all(tx, append(s.delivered(false), s.reversal("2026-03-20", "MD2"))...); err != nil {
				t.Fatal(err)
			}
			failsWith(t, exec(tx, change+` WHERE delivery_id = $1`, s.delivery), "insert-only")
		})
	}
}
