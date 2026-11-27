package sales

import (
	"context"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
	"github.com/mustie7iy/agentic-erp/internal/tenancy"
)

// Checking with the replenishment lead time (S2j2; ADR-0082 As built
// S2j2). A material's planning data at a plant may name its replenishment
// lead time, the calendar days the plant takes to have more of it (the
// mrp context's, SAP MARC-WZEIT, from memory). An order asked for on or
// after the company's date today plus that lead time is in time for the
// plant to replenish what it lacks, so the availability check confirms
// such an item in full: what the stock covers first, and the rest on the
// lead time, which the item names with the day the lead time ends. An
// order asked for sooner, or for no date, is checked against the stock
// alone, and so is a material without a lead time at the plant.

// LeadTimes is what the availability check reads of materials'
// replenishment lead times at a plant, in days: the mrp context's
// planning data, through a port the composition root wires, so neither
// context imports the other.
type LeadTimes interface {
	LeadTimes(ctx context.Context, plant uuid.UUID, materials []uuid.UUID) (map[uuid.UUID]int32, error)
}

// LeadPart is the part of an item's confirmed quantity that rests on its
// material's replenishment lead time: what the plant's stock did not
// cover when the check confirmed it, in the item's unit, and the day the
// lead time ends.
type LeadPart struct {
	Quantity string    `json:"quantity"`
	Date     time.Time `json:"date"`
}

// leadTime is a material's lead time at the plant as a check counts it:
// its days, the day it ends from the company's date today, and whether
// the order is asked for on or after that day (reached), so its items of
// the material are confirmed in full.
type leadTime struct {
	days    int32
	ends    time.Time
	reached bool
}

// judged is a lead time of days judged on today for an order asked for
// on requested; both are dates at midnight UTC, as every date of the
// context is.
func judged(today, requested time.Time, days int32) leadTime {
	ends := today.AddDate(0, 0, int(days))
	return leadTime{days: days, ends: ends, reached: !requested.Before(ends)}
}

// leadTimesOf are the lead times of the materials at the order's plant,
// judged on the company's date today; none for an order asked for no
// delivery date, which is checked against the stock alone, as for today.
func (e *Endpoints) leadTimesOf(ctx context.Context, o Order, le tenancy.Entity, ids []uuid.UUID) (map[uuid.UUID]leadTime, error) {
	if o.DeliveryDate == nil {
		return nil, nil
	}
	days, err := e.LeadTimes.LeadTimes(ctx, *o.PlantID, ids)
	if err != nil || len(days) == 0 {
		return nil, errs.Wrapf(err, "sales: lead times at plant %s", *o.PlantID) // nil for none
	}
	today, err := e.today(ctx, le.Code)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]leadTime, len(days))
	for m, d := range days {
		out[m] = judged(today.Date, *o.DeliveryDate, d)
	}
	return out, nil
}

// onLead is the part of what an item confirms that rests on the lead
// time, nil for none. beyond is what the stock does not reach of what it
// confirms. An item confirmed in full on its lead time (full) rests all
// of that on it, until lt ends; any other keeps as much of that as rested
// on the lead time before, until the day that did: a check of stock
// alone never moves a promise onto the lead time, and stock that arrived
// since takes its place.
func onLead(it OrderItem, beyond *big.Rat, full bool, lt leadTime) *LeadPart {
	var part *big.Rat
	var ends time.Time
	switch {
	case full:
		part, ends = beyond, lt.ends
	case it.LeadTime != nil:
		part, ends = money.MinRat(beyond, money.ParseRat(it.LeadTime.Quantity)), it.LeadTime.Date
	default:
		return nil
	}
	if part.Sign() <= 0 {
		return nil
	}
	return &LeadPart{Quantity: money.PlainQuantity(part), Date: ends}
}
