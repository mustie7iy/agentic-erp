package processmining

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/audit"
	"github.com/mustie7iy/agentic-erp/internal/payables"
)

// page is what a page's events are read from: each case's documents and
// every document's number.
type page struct {
	docs    map[uuid.UUID][]payables.LoggedInvoice // by case
	runs    map[uuid.UUID][]uuid.UUID              // the runs that paid each journal entry
	numbers map[uuid.UUID]string                   // every document's number by id
}

// events gives each case its events: the documents on its order (or its
// own document) and the runs that paid them join its objects, and every
// audit event of an activity about one of them is an event of the case,
// by time and then sequence number. A document on two orders is in both
// cases, and so is a run that paid documents of two.
func (e *Endpoints) events(ctx context.Context, cases []Case) error {
	p := page{docs: map[uuid.UUID][]payables.LoggedInvoice{}, runs: map[uuid.UUID][]uuid.UUID{}, numbers: map[uuid.UUID]string{}}
	if err := e.documents(ctx, cases, p); err != nil {
		return err
	}
	if err := e.payments(ctx, cases, p); err != nil {
		return err
	}
	var objects []audit.ObjectRef
	for i := range cases {
		c := &cases[i]
		for _, v := range p.docs[c.ID] {
			c.objects = append(c.objects, audit.ObjectRef{Type: invoiceType, ID: v.ID})
			if v.EntryID != nil {
				for _, run := range p.runs[*v.EntryID] {
					c.objects = append(c.objects, audit.ObjectRef{Type: runType, ID: run})
				}
			}
		}
		c.objects = dedupe(c.objects)
		objects = append(objects, c.objects...)
	}
	trail, err := e.Trail.About(ctx, dedupe(objects), actions())
	if err != nil {
		return fmt.Errorf("processmining: %w", err)
	}
	byObject := map[audit.ObjectRef][]audit.ObjectEvent{}
	for _, ev := range trail {
		byObject[ev.Object] = append(byObject[ev.Object], ev)
	}
	for i := range cases {
		c := &cases[i]
		c.Events = []Event{}
		for _, o := range c.objects {
			for _, ev := range byObject[o] {
				c.Events = append(c.Events, Event{Activity: activities[ev.Action], ActorKind: actorKind(ev.ActorKind), OccurredAt: ev.OccurredAt,
					Object: o, DocumentNumber: p.numbers[o.ID], Seq: ev.Seq})
			}
		}
		slices.SortFunc(c.Events, func(a, b Event) int { return cmp.Or(a.OccurredAt.Compare(b.OccurredAt), cmp.Compare(a.Seq, b.Seq)) })
	}
	return nil
}

// documents notes each case's documents and every number: an order
// case's documents are those on its order, a document case's its own.
func (e *Endpoints) documents(ctx context.Context, cases []Case, p page) error {
	var orders []uuid.UUID
	for _, c := range cases {
		p.numbers[c.ID] = c.DocumentNumber
		for _, d := range c.refs {
			p.numbers[d.ID] = d.DocumentNumber
		}
		if c.own != nil {
			p.docs[c.ID] = []payables.LoggedInvoice{*c.own}
		} else {
			orders = append(orders, c.ID)
		}
	}
	if len(orders) == 0 {
		return nil
	}
	invoices, err := e.Invoices.OfOrders(ctx, orders)
	if err != nil {
		return fmt.Errorf("processmining: %w", err)
	}
	for _, v := range invoices {
		p.numbers[v.ID] = v.DocumentNumber
		for _, o := range v.Orders {
			p.docs[o] = append(p.docs[o], v)
		}
	}
	return nil
}

// payments notes the released runs that paid the cases' posted
// documents, and their numbers.
func (e *Endpoints) payments(ctx context.Context, cases []Case, p page) error {
	var years []int16
	var entries []uuid.UUID
	for _, c := range cases {
		for _, v := range p.docs[c.ID] {
			if v.EntryID != nil && !slices.Contains(entries, *v.EntryID) {
				years, entries = append(years, *v.EntryYear), append(entries, *v.EntryID)
			}
		}
	}
	if len(entries) == 0 {
		return nil
	}
	paid, err := e.Payments.Paying(ctx, years, entries)
	if err != nil {
		return fmt.Errorf("processmining: %w", err)
	}
	for _, x := range paid {
		p.runs[x.EntryID] = append(p.runs[x.EntryID], x.RunID)
		p.numbers[x.RunID] = x.DocumentNumber
	}
	return nil
}

// dedupe are the objects without repeats, in their first order.
func dedupe(objects []audit.ObjectRef) []audit.ObjectRef {
	seen := map[audit.ObjectRef]bool{}
	var out []audit.ObjectRef
	for _, o := range objects {
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}
