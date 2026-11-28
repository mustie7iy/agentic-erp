package integration

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// Stock values materials in plants and takes their stock over, and reads
// back what runs took over (inventory's published API, ADR-0082 As built
// S2p).
type Stock interface {
	TakeOver(ctx context.Context, in inventory.StockTakeOver) (inventory.TakenOverStock, error)
	LockNumbers(ctx context.Context, company uuid.UUID) (time.Time, error)
	TakenOver(ctx context.Context, valuations, documents []uuid.UUID) (map[uuid.UUID]inventory.HeldStock, map[uuid.UUID]uuid.UUID, error)
}

// lockStock plans the stock take-over of the materials the run loads
// (stocksOf), each once: a valuation an earlier run took over is kept as
// it is, as SAP's stock is taken over at the cutover. It then takes the
// material_document range of every company whose stock it posts
// (Stock.LockNumbers, in company code order), before the GR/IR locks
// (lockGRIR) and anything that posts to the ledger, so the run takes the
// order items, goods_receipt, material_document, the GR/IR locks and
// journal_we in the order a goods receipt into stock takes them
// (ADR-0008, ADR-0082 As built S2p, Locks).
func (l *loader) lockStock(ctx context.Context) error {
	plants, _ := plantsOf(l.ext)                    // the quality gate refused their errors
	mats, _ := materialsOf(l.ext, l.t, plants)      // and reported the materials held back
	stocks, _ := stocksOf(l.ext, l.t, plants, mats) // and refused the stock it cannot take over
	posts := map[string]bool{}
	l.stocks = nil
	for _, s := range stocks {
		if _, ok := l.refs["MBEW/"+s.key()]; ok {
			continue
		}
		l.stocks = append(l.stocks, s)
		if len(s.locations) > 0 {
			posts[s.company] = true
		}
	}
	l.stockDays = map[string]time.Time{}
	for _, company := range sortedKeys(posts) {
		// A refusal (the company has no material_document range) locks
		// nothing: each of its take-overs is refused for it (takeOver).
		day, err := l.e.Stock.LockNumbers(ctx, l.t.entities[company].ID)
		if err != nil && problem.From(err).Status >= 500 {
			return err
		}
		l.stockDays[company] = day
	}
	return nil
}

// stock takes over what lockStock planned, under the locks it and lockGRIR
// took. The receipts before it post no entry (PostLoadedReceipt; SAP's is
// in the journal), so the run holds no journal range until its first
// initial entry.
func (l *loader) stock(ctx context.Context) error {
	for _, s := range l.stocks {
		if err := l.takeOver(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// takeOverInput is what TakeOver is given for a stock lockStock planned:
// the valuation, the stock at each storage location and the day lockStock
// locked the company's range for; false, with the issue reported, when
// the material, the plant or a storage location did not load.
func (l *loader) takeOverInput(s sapStock) (inventory.StockTakeOver, bool) {
	material, plant := l.refs["MARA/"+s.matnr], l.refs["T001W/"+s.werks]
	if material == uuid.Nil || plant == uuid.Nil {
		l.issue("MBEW", s.key(), "material "+s.matnr+" or plant "+s.werks+" was not loaded")
		return inventory.StockTakeOver{}, false
	}
	cur := l.t.entities[s.company].LocalCurrency
	in := inventory.StockTakeOver{Valuation: inventory.ValuationInput{MaterialID: material, PlantID: plant, ValuationClass: s.class,
		PriceControl: s.control, Price: l.plain(s.price, cur), Currency: cur, PriceUnit: s.priceUnit},
		HeaderText: "Migration run " + l.run.ID.String() + ": SAP stock of material " + s.matnr + " at plant " + s.werks, Day: l.stockDays[s.company]}
	for _, at := range s.locations {
		id, ok := l.refs["T001L/"+s.werks+"/"+at.lgort]
		if !ok {
			l.issue("MARD", s.matnr+"/"+s.werks+"/"+at.lgort, "storage location "+at.lgort+" of plant "+s.werks+" was not loaded")
			return inventory.StockTakeOver{}, false
		}
		it := inventory.TakeOverItem{StorageLocationID: id, Quantity: at.quantity}
		if s.control == inventory.MovingAverage {
			it.Amount = &inventory.Amount{Value: l.plain(at.amount, cur), Currency: cur}
		}
		in.Items = append(in.Items, it)
	}
	return in, true
}

// takeOver values a material at a plant and posts its stock as one
// initial entry, keeping the valuation as MBEW/<BWKEY>/<MATNR> and the
// material document as MKPF/<BWKEY>/<MATNR>. A take-over the services
// refuse is a load refusal, and so is one whose entry falls in another
// fiscal year than the run's: the entry is dated the day lockStock locked
// the company's range for (its date today then), and the run's trial
// balance and stock controls are of its fiscal year.
func (l *loader) takeOver(ctx context.Context, s sapStock) error {
	in, ok := l.takeOverInput(s)
	if !ok {
		return nil
	}
	var taken inventory.TakenOverStock
	done, err := l.step(ctx, "MBEW", s.key(), func() error {
		var err error
		if taken, err = l.e.Stock.TakeOver(ctx, in); err != nil {
			return err
		}
		if err := l.addRef(ctx, "inventory.valuation", taken.Valuation.ID, "MBEW", s.key()); err != nil {
			return err
		}
		if taken.Document == nil {
			return nil
		}
		return l.addRef(ctx, "inventory.material_document", taken.Document.ID, "MKPF", s.key())
	})
	if !done {
		return err
	}
	l.counts.MaterialValuations++
	if d := taken.Document; d != nil {
		l.counts.InitialEntries++
		if d.EntryYear != nil && int32(*d.EntryYear) != l.t.year {
			l.issue("MBEW", s.key(), "the stock is taken over on the day the run locked the company's range (its date today then), in fiscal year "+strconv.Itoa(int(*d.EntryYear))+
				"; take it over in a run of that fiscal year")
		}
	}
	return nil
}
