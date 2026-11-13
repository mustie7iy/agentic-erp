package grc

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The states of a control's links in the matrix (ADR-0078 §8).
const (
	LinksHolding = "holding"
	LinksBroken  = "broken"
	LinksNone    = "none"
)

// MatrixFilter selects the controls of the matrix: nil fields select
// all.
type MatrixFilter struct {
	Process   *string
	Reference *string // one framework reference, by its exact text
	LinkState *string
}

// Totals count what a matrix's filters select, over every page.
type Totals struct {
	Controls, Holding, Broken, WithoutLinks int32
	Links, LinksHolding, LinksBroken        int32
}

// selection is what the matrix's filters select: the controls' ids,
// each one's judged links and state, and the totals.
type selection struct {
	asOf   time.Time
	ids    []uuid.UUID
	links  map[uuid.UUID][]Link
	states map[uuid.UUID]string
	totals Totals
}

// matrixWhere selects the active controls of a process and framework
// reference under the alias c ($1, $2).
const matrixWhere = ` c.status = 'active' AND ($1::text IS NULL OR c.process = $1) AND ($2::text IS NULL OR $2 = ANY(c.framework_references))`

// state is the state of a control's links.
func state(links []Link) string {
	if len(links) == 0 {
		return LinksNone
	}
	for _, l := range links {
		if !l.Holds {
			return LinksBroken
		}
	}
	return LinksHolding
}

// selected reads the active controls the filter selects with their
// active links, judges the links and counts what is selected. The
// link state is known only once the links are judged, so it is applied
// here, and a page is then read among the ids selected.
func (e *Endpoints) selected(ctx context.Context, tx pgx.Tx, f MatrixFilter) (selection, error) {
	rows, _ := tx.Query(ctx, `SELECT c.id FROM grc.control c WHERE`+matrixWhere+` ORDER BY c.code, c.id`, f.Process, f.Reference)
	controls, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return selection{}, fmt.Errorf("grc: controls of the matrix: %w", err)
	}
	// By the order the links were made (a link's id is a UUIDv7), the
	// order a control's links are shown in.
	rows, _ = tx.Query(ctx, linkSelect+` WHERE l.status = 'active' AND`+matrixWhere+` ORDER BY l.id`, f.Process, f.Reference)
	links, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Link, error) { return scanLink(r) })
	if err != nil {
		return selection{}, fmt.Errorf("grc: links of the matrix: %w", err)
	}
	links, today, err := e.judged(ctx, tx, links)
	if err != nil {
		return selection{}, err
	}
	s := selection{asOf: today, ids: []uuid.UUID{}, links: map[uuid.UUID][]Link{}, states: map[uuid.UUID]string{}}
	for _, l := range links {
		s.links[l.ControlID] = append(s.links[l.ControlID], l)
	}
	for _, id := range controls {
		st := state(s.links[id])
		if f.LinkState != nil && *f.LinkState != st {
			continue
		}
		s.ids, s.states[id] = append(s.ids, id), st
		s.totals.add(st, s.links[id])
	}
	return s, nil
}

// add counts a control selected, in the state of its links.
func (t *Totals) add(state string, links []Link) {
	t.Controls++
	switch state {
	case LinksNone:
		t.WithoutLinks++
	case LinksBroken:
		t.Broken++
	default:
		t.Holding++
	}
	for _, l := range links {
		t.Links++
		if l.Holds {
			t.LinksHolding++
		} else {
			t.LinksBroken++
		}
	}
}
