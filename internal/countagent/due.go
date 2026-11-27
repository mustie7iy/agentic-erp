package countagent

import (
	"cmp"
	"context"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/money"
)

// The rule of a count due (decided by default, ADR-0082 As built S2e,
// from SAP's ABC analysis for cycle counting, MIBC, and its cycle
// counting indicator's intervals, from memory): the year read is the 365
// days to the run's day; a material's class is A while the materials of
// greater consumption value make up less than ShareA percent of the
// plant's, B while less than ShareB, else C, as is one with none; it is
// due at a storage location never counted there, or last counted there
// Interval[class] days or more before the day: its cycle counting
// indicator's interval instead when it has one at the plant (ADR-0082 As
// built S2c2), which the controller keeps on the material at the plant.
const (
	YearDays = 365
	ShareA   = 80
	ShareB   = 95
)

// Interval is the days between counts of each class: four counts a year
// of an A material, two of a B, one of a C.
var Interval = map[string]int{"A": 90, "B": 180, "C": 365}

// consumes are the movement types whose value is consumption at the plant
// (an issue to a cost center, a scrapping, a delivery's goods issue) and
// their reversals, which give it back: their lines' value, negated.
// Transfers, receipts and count differences are no consumption.
var consumes = map[string]bool{"201": true, "202": true, "551": true, "552": true, "601": true, "602": true}

// openCount reports whether a count document's status leaves it open, its
// posting block on: counting, counted, pending_approval, or one this
// version does not know. posted, rejected and withdrawn are closed.
func openCount(status string) bool {
	return !slices.Contains([]string{"posted", "rejected", "withdrawn"}, status)
}

// stockRow is a material's stock at a storage location of the plant.
type stockRow struct {
	MaterialID          string `json:"material_id"`
	MaterialNumber      string `json:"material_number"`
	StorageLocationID   string `json:"storage_location_id"`
	StorageLocationCode string `json:"storage_location_code"`
}

// material is what a count of a material needs to know: whether a count
// document takes it (active or blocked, not managed in batches or by
// serial number, which counts refuse, As built S2f and S2g).
type material struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	BatchManaged  bool   `json:"batch_managed"`
	SerialManaged bool   `json:"serial_managed"`
}

func (m material) countable() bool {
	return (m.Status == "active" || m.Status == "blocked") && !m.BatchManaged && !m.SerialManaged
}

// amount is a Money as an answer gives it.
type amount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// document is a material document as the run reads it.
type document struct {
	PostingDate  string `json:"posting_date"`
	MovementType string `json:"movement_type"`
	Lines        []struct {
		MaterialID        string  `json:"material_id"`
		PlantID           string  `json:"plant_id"`
		StorageLocationID string  `json:"storage_location_id"`
		InTransit         bool    `json:"in_transit"`
		Amount            *amount `json:"amount"`
	} `json:"lines"`
}

// countDocument is a count document as the run reads it.
type countDocument struct {
	Number            string `json:"number"`
	DocumentDate      string `json:"document_date"`
	Status            string `json:"status"`
	StorageLocationID string `json:"storage_location_id"`
	Items             []struct {
		MaterialID string `json:"material_id"`
	} `json:"items"`
}

// indicator is a material's cycle counting indicator at the plant, as
// listCycleCountMaterials gives it.
type indicator struct {
	MaterialID    string `json:"material_id"`
	IndicatorCode string `json:"indicator_code"`
	IntervalDays  int    `json:"interval_days"`
}

// place is a material at a storage location.
type place struct{ material, location string }

// lastCount is a place's latest posted count.
type lastCount struct{ date, number string }

// candidate is a material at a storage location due for a count, with
// what ranks it and its line.
type candidate struct {
	class          string
	indicator      *indicator // nil: due by its class's interval
	movements      int
	last           lastCount
	number, code   string // the material's number, the storage location's code
	materialID, sl string
	line           string
}

// survey is what the run found: the year's first day and the due
// materials in their rank.
type survey struct {
	from string
	due  []candidate
}

// lines are the due materials' lines the run lists, the first MaxLines.
func (s *survey) lines() []string {
	var out []string
	for _, c := range s.due[:min(len(s.due), MaxLines)] {
		out = append(out, c.line)
	}
	return out
}

// reading is what the reads before the session gathered.
type reading struct {
	plantID     string
	from, day   string // the year's first and last day
	stock       []stockRow
	materials   map[string]material
	indicators  map[string]indicator   // by material
	consumption map[string]money.Minor // by material
	movements   map[place]int
	last        map[place]lastCount
	open        map[place]bool
	currency    string
	decimals    int
}

// survey reads the plant's stock, materials, movements of the year to the
// day and count documents, and ranks what is due.
func (a *Agent) survey(ctx context.Context, pl plant, day string) (*survey, error) {
	d, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return nil, fmt.Errorf("countagent: the day %q: %w", day, err)
	}
	r := &reading{plantID: pl.ID, from: d.AddDate(0, 0, 1-YearDays).Format(time.DateOnly), day: day, materials: map[string]material{},
		indicators: map[string]indicator{}, consumption: map[string]money.Minor{}, movements: map[place]int{}, last: map[place]lastCount{},
		open: map[place]bool{}}
	q := url.Values{"plant_id": {pl.ID}}
	if r.stock, err = apitools.List[stockRow](ctx, a.API, "/v1/stock", q); err != nil {
		return nil, fmt.Errorf("countagent: the stock of plant %s: %w", pl.Code, err)
	}
	materials, err := apitools.List[material](ctx, a.API, "/v1/materials", q)
	if err != nil {
		return nil, fmt.Errorf("countagent: the materials of plant %s: %w", pl.Code, err)
	}
	for _, m := range materials {
		r.materials[m.ID] = m
	}
	// Only the indicators materials are counted by: one taken off (S2c3)
	// leaves its material to its class's interval.
	indicators, err := apitools.List[indicator](ctx, a.API, "/v1/cycle-count-materials", url.Values{"plant_id": {pl.ID}, "status": {"active"}})
	if err != nil {
		return nil, fmt.Errorf("countagent: the cycle counting indicators of plant %s: %w", pl.Code, err)
	}
	for _, i := range indicators {
		r.indicators[i.MaterialID] = i
	}
	if err := r.documents(ctx, a.API, q); err != nil {
		return nil, fmt.Errorf("countagent: the material documents of plant %s: %w", pl.Code, err)
	}
	counts, err := apitools.List[countDocument](ctx, a.API, "/v1/physical-inventory-documents", q)
	if err != nil {
		return nil, fmt.Errorf("countagent: the count documents of plant %s: %w", pl.Code, err)
	}
	r.counts(counts)
	return r.rank(pl, d)
}

// documents reads the plant's material documents and keeps those of the
// year. Every page is read: the list is in the order documents were
// created, and a goods receipt or a stock take-over is posted on the day
// its caller names, so a document created today may be dated before the
// year and one dated in it may follow it.
func (r *reading) documents(ctx context.Context, c *apitools.Client, q url.Values) error {
	var bad error
	err := apitools.Each(ctx, c, "/v1/material-documents", q, func(doc document) bool {
		if doc.PostingDate < r.from || doc.PostingDate > r.day {
			return true
		}
		bad = r.document(doc)
		return bad == nil
	})
	if err != nil {
		return err
	}
	return bad
}

// document counts a document's lines at the plant as movements of their
// material at their storage location, and adds a consumption's value to
// its material's.
func (r *reading) document(doc document) error {
	for _, l := range doc.Lines {
		if l.PlantID != r.plantID {
			continue
		}
		if !l.InTransit {
			r.movements[place{l.MaterialID, l.StorageLocationID}]++
		}
		if !consumes[doc.MovementType] || l.Amount == nil {
			continue
		}
		v, err := r.value(*l.Amount)
		if err != nil {
			return err
		}
		if r.consumption[l.MaterialID], err = money.Diff(r.consumption[l.MaterialID], v); err != nil {
			return fmt.Errorf("countagent: the consumption of material %s: %w", l.MaterialID, err)
		}
	}
	return nil
}

// value is an amount in minor units of the plant's currency: every amount
// of the plant's valuation is in its company's, which the first fixes,
// with its decimals; another is refused, never added (ADR-0005).
func (r *reading) value(a amount) (money.Minor, error) {
	_, frac, _ := strings.Cut(a.Amount, ".")
	if r.currency == "" {
		r.currency, r.decimals = a.Currency, len(frac)
	}
	if a.Currency != r.currency {
		return 0, fmt.Errorf("countagent: an amount in %s beside %s at one plant", a.Currency, r.currency)
	}
	v, err := money.ParseAmount(a.Amount, r.decimals)
	if err != nil {
		return 0, fmt.Errorf("countagent: %w", err)
	}
	return v, nil
}

// counts keeps each place's latest posted count and the places an open
// count holds.
func (r *reading) counts(docs []countDocument) {
	for _, d := range docs {
		for _, it := range d.Items {
			p := place{it.MaterialID, d.StorageLocationID}
			switch {
			case openCount(d.Status):
				r.open[p] = true
			case d.Status == "posted" && d.DocumentDate > r.last[p].date:
				r.last[p] = lastCount{d.DocumentDate, d.Number}
			}
		}
	}
}

// rank gives each countable material in stock its class and finds those
// due on the day, in their rank: class, then movements (the most first),
// then the last count (never first, then the oldest), then number and
// storage location.
func (r *reading) rank(pl plant, day time.Time) (*survey, error) {
	classes, err := classify(r.consumption)
	if err != nil {
		return nil, err
	}
	s := &survey{from: r.from}
	for _, st := range r.stock {
		p := place{st.MaterialID, st.StorageLocationID}
		m, known := r.materials[st.MaterialID]
		if !known || !m.countable() || r.open[p] {
			continue
		}
		c := candidate{class: cmp.Or(classes[m.ID], "C"), movements: r.movements[p], last: r.last[p], number: st.MaterialNumber,
			code: st.StorageLocationCode, materialID: m.ID, sl: st.StorageLocationID}
		interval := Interval[c.class]
		if i, ok := r.indicators[m.ID]; ok {
			c.indicator, interval = &i, i.IntervalDays
		}
		if c.last.date != "" && c.last.date > day.AddDate(0, 0, -interval).Format(time.DateOnly) {
			continue
		}
		c.line = c.text(pl.Code, r.from)
		s.due = append(s.due, c)
	}
	slices.SortFunc(s.due, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.class, b.class), cmp.Compare(b.movements, a.movements), cmp.Compare(a.last.date, b.last.date),
			cmp.Compare(a.number, b.number), cmp.Compare(a.code, b.code))
	})
	return s, nil
}

// text is a candidate's line.
func (c candidate) text(plantCode, from string) string {
	moves := fmt.Sprintf("%d movements since %s", c.movements, from)
	if c.movements == 1 {
		moves = "1 movement since " + from
	}
	last := "never counted"
	if c.last.date != "" {
		last = fmt.Sprintf("last counted on %s (%s)", c.last.date, c.last.number)
	}
	class := "class " + c.class
	if c.indicator != nil {
		class += fmt.Sprintf(", cycle counting indicator %s every %d days", c.indicator.IndicatorCode, c.indicator.IntervalDays)
	}
	return fmt.Sprintf("Count %s at %s/%s (material %s, storage location %s): %s, %s, %s", c.number, plantCode, c.code, c.materialID, c.sl,
		class, moves, last)
}

// classify gives the materials with consumption their ABC class: in the
// order of their consumption value, the greatest first (ties by id), A
// while those before make up less than ShareA percent of the total, B
// while less than ShareB, else C. Shares are compared exactly, in whole
// minor units.
func classify(consumption map[string]money.Minor) (map[string]string, error) {
	var ids []string
	var values []money.Minor
	for id, v := range consumption {
		if v > 0 {
			ids, values = append(ids, id), append(values, v)
		}
	}
	total, err := money.Sum(values...)
	if err != nil {
		return nil, fmt.Errorf("countagent: the plant's consumption: %w", err)
	}
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(cmp.Compare(consumption[b], consumption[a]), cmp.Compare(a, b)) })
	classes, before := map[string]string{}, new(big.Int)
	share := func(pct int64) *big.Int { return new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(pct)) }
	for _, id := range ids {
		scaled := new(big.Int).Mul(before, big.NewInt(100))
		switch {
		case scaled.Cmp(share(ShareA)) < 0:
			classes[id] = "A"
		case scaled.Cmp(share(ShareB)) < 0:
			classes[id] = "B"
		default:
			classes[id] = "C"
		}
		before.Add(before, big.NewInt(int64(consumption[id])))
	}
	return classes, nil
}
