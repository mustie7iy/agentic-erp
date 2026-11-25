package sales

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/materials"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// TestConfirm: an item is confirmed as far as its material is available,
// in its own unit rounded down to six decimals, never below what it had
// confirmed (up to its quantity) nor above its quantity, and what it
// promises and has not delivered is counted out of what is available. A
// material of which a promise cannot be valued, its unit no longer one of
// the material's, has nothing available; an item in such a unit keeps
// what it had.
func TestConfirm(t *testing.T) {
	m := materials.Material{ID: uuid.New(), BaseUnit: "EA", Units: []materials.Unit{{Code: "PAK", Numerator: 3, Denominator: 1}}}
	confirmed := func(q string) *string { return &q }
	for _, c := range []struct {
		name                 string
		free                 string
		unknown              bool
		item                 OrderItem
		want, wantLeft       string
		wantUnknownAfterTake bool
	}{
		{"all of it", "10", false, OrderItem{Item: Item{Quantity: "4", Unit: "EA"}, Delivered: "0"}, "4", "6", false},
		{"part, rounded down in the item's unit", "1", false, OrderItem{Item: Item{Quantity: "2", Unit: "PAK"}, Delivered: "0"}, "0.333333", "0.000001", false},
		{"nothing available", "-5", false, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "0"}, "0", "-5", false},
		{"an increase keeps what was confirmed", "0", false, OrderItem{Item: Item{Quantity: "9", Unit: "EA"}, Delivered: "1", Confirmed: confirmed("3")},
			"3", "-2", false},
		{"a decrease to below what was confirmed", "0", false, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "1", Confirmed: confirmed("3")},
			"2", "-1", false},
		{"delivered counts as reached", "2", false, OrderItem{Item: Item{Quantity: "9", Unit: "EA"}, Delivered: "4", Confirmed: confirmed("4")}, "6", "0", false},
		{"a promise of the material could not be valued", "10", true, OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "0"}, "0", "10", true},
		{"its own unit no longer the material's", "10", false, OrderItem{Item: Item{Quantity: "5", Unit: "BOX"}, Delivered: "0", Confirmed: confirmed("1")},
			"1", "10", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stockLeft{left: map[uuid.UUID]*big.Rat{m.ID: money.ParseRat(c.free)}, unknown: map[uuid.UUID]bool{m.ID: c.unknown}}
			c.item.MaterialID = &m.ID
			p := s.confirm(c.item, m, leadTime{}, false)
			if p.confirmed != c.want {
				t.Errorf("confirmed: got %s, want %s", p.confirmed, c.want)
			}
			if left := money.PlainQuantity(s.left[m.ID]); left != c.wantLeft {
				t.Errorf("left: got %s, want %s", left, c.wantLeft)
			}
			if p.unvalued != (c.item.Unit == "BOX") {
				t.Errorf("unvalued: got %v, want it only for a unit the material lacks", p.unvalued)
			}
			if s.unknown[m.ID] != c.wantUnknownAfterTake {
				t.Errorf("unknown: got %v, want %v", s.unknown[m.ID], c.wantUnknownAfterTake)
			}
		})
	}
}

// TestConfirmedRuleAnswered: the database's refusal of a delivery beyond
// what an item confirmed (sales_order_item_confirmed) is answered as the
// application's own refusal, 409 sales.exceeds_open_quantity.
func TestConfirmedRuleAnswered(t *testing.T) {
	p := problem.From(fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23514", ConstraintName: "sales_order_item_confirmed"}))
	if p == nil || p.Status != 409 || p.Code != "sales.exceeds_open_quantity" {
		t.Errorf("got %+v, want 409 sales.exceeds_open_quantity", p)
	}
}

// TestConfirmOnTheLeadTime (S2j2): an item of a material whose lead time
// the order's date reaches is confirmed in full, the stock first and the
// rest on the lead time until it ends, and all of it counts as promised;
// a check of stock alone keeps no more on the lead time than rested on it
// before, less the stock that arrived since, and moves nothing onto it;
// an item in a unit the material lacks keeps what it had, with what of
// it rested on the lead time.
func TestConfirmOnTheLeadTime(t *testing.T) {
	m := materials.Material{ID: uuid.New(), BaseUnit: "EA", Units: []materials.Unit{{Code: "PAK", Numerator: 3, Denominator: 1}}}
	ends, before := time.Date(2099, 1, 11, 0, 0, 0, 0, time.UTC), time.Date(2099, 1, 4, 0, 0, 0, 0, time.UTC)
	reached, short := leadTime{days: 10, ends: ends, reached: true}, leadTime{days: 10, ends: ends}
	q := func(s string) *string { return &s }
	rested := func(s string) *LeadPart { return &LeadPart{Quantity: s, Date: before} }
	for _, c := range []struct {
		name          string
		free          string
		item          OrderItem
		lt            leadTime
		hasLead       bool
		want, wantFor string // what it confirms, and what of it rests on the lead time ("-": nothing) "until" the day
		wantLeft      string
	}{
		{"reached: the stock first, the rest on the lead time", "4", OrderItem{Item: Item{Quantity: "10", Unit: "EA"}, Delivered: "0"}, reached, true,
			"10", "6 until 2099-01-11", "-6"},
		{"reached, all of it in stock", "20", OrderItem{Item: Item{Quantity: "10", Unit: "EA"}, Delivered: "0"}, reached, true, "10", "-", "10"},
		{"reached with nothing in stock", "-3", OrderItem{Item: Item{Quantity: "2", Unit: "EA"}, Delivered: "0"}, reached, true,
			"2", "2 until 2099-01-11", "-5"},
		{"not reached: the stock alone", "4", OrderItem{Item: Item{Quantity: "10", Unit: "EA"}, Delivered: "0"}, short, true, "4", "-", "0"},
		{"no lead time", "4", OrderItem{Item: Item{Quantity: "10", Unit: "EA"}, Delivered: "0"}, leadTime{}, false, "4", "-", "0"},
		{"reached, in packs rounded down", "4", OrderItem{Item: Item{Quantity: "2", Unit: "PAK"}, Delivered: "0"}, reached, true,
			"2", "0.666667 until 2099-01-11", "-2"},
		{"reached, part of it delivered", "0", OrderItem{Item: Item{Quantity: "8", Unit: "EA"}, Delivered: "5", Confirmed: q("5")}, reached, true,
			"8", "3 until 2099-01-11", "-3"},
		{"checked again short: what rested on it stays", "4", OrderItem{Item: Item{Quantity: "12", Unit: "EA"}, Delivered: "0", Confirmed: q("10"),
			LeadTime: rested("6")}, short, true, "10", "6 until 2099-01-04", "-6"},
		{"checked again short, stock arrived: less rests on it", "7", OrderItem{Item: Item{Quantity: "12", Unit: "EA"}, Delivered: "0", Confirmed: q("10"),
			LeadTime: rested("6")}, short, true, "10", "3 until 2099-01-04", "-3"},
		{"checked again short, all in stock now", "12", OrderItem{Item: Item{Quantity: "12", Unit: "EA"}, Delivered: "0", Confirmed: q("10"),
			LeadTime: rested("6")}, short, true, "12", "-", "0"},
		{"checked again with stock alone: nothing moves onto it", "4", OrderItem{Item: Item{Quantity: "12", Unit: "EA"}, Delivered: "0", Confirmed: q("10")},
			short, true, "10", "-", "-6"},
		{"checked again reached: the new end", "4", OrderItem{Item: Item{Quantity: "12", Unit: "EA"}, Delivered: "0", Confirmed: q("10"),
			LeadTime: rested("6")}, reached, true, "12", "8 until 2099-01-11", "-8"},
		{"a unit the material lacks keeps what rested on it", "10", OrderItem{Item: Item{Quantity: "5", Unit: "BOX"}, Delivered: "0", Confirmed: q("2"),
			LeadTime: rested("1")}, reached, true, "2", "1 until 2099-01-04", "10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := stockLeft{left: map[uuid.UUID]*big.Rat{m.ID: money.ParseRat(c.free)}, unknown: map[uuid.UUID]bool{}}
			c.item.MaterialID = &m.ID
			p := s.confirm(c.item, m, c.lt, c.hasLead)
			if p.confirmed != c.want {
				t.Errorf("confirmed: got %s, want %s", p.confirmed, c.want)
			}
			lead := "-"
			if p.lead != nil {
				lead = p.lead.Quantity + " until " + p.lead.Date.Format(time.DateOnly)
			}
			if lead != c.wantFor {
				t.Errorf("on the lead time: got %s, want %s", lead, c.wantFor)
			}
			if left := money.PlainQuantity(s.left[m.ID]); left != c.wantLeft {
				t.Errorf("left: got %s, want %s", left, c.wantLeft)
			}
			if (p.leadDays != nil) != c.hasLead {
				t.Errorf("the lead time judged on: got %v, want it only for a material with one", p.leadDays)
			}
		})
	}
}

// TestJudged (S2j2): a lead time is reached by an order asked for on or
// after the company's date today plus its days, not by one a day sooner.
func TestJudged(t *testing.T) {
	today := time.Date(2099, 12, 25, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		requested time.Time
		want      bool
	}{
		{"on the day it ends", today.AddDate(0, 0, 10), true},
		{"after it", today.AddDate(1, 0, 0), true},
		{"a day sooner", today.AddDate(0, 0, 9), false},
		{"today", today, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			lt := judged(today, c.requested, 10)
			if lt.reached != c.want || lt.ends.Format(time.DateOnly) != "2100-01-04" || lt.days != 10 {
				t.Errorf("got %+v, want reached %v, ending 2100-01-04", lt, c.want)
			}
		})
	}
}
