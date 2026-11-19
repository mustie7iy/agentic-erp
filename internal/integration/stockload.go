package integration

import (
	"context"
	"strconv"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/inventory"
)

// Stock values materials in plants and takes their stock over, and reads
// back what runs took over (inventory's published API, ADR-0082 As built
// S2p).
type Stock interface {
	TakeOver(ctx context.Context, in inventory.StockTakeOver) (inventory.TakenOverStock, error)
	TakenOver(ctx context.Context, valuations, documents []uuid.UUID) (map[uuid.UUID]inventory.HeldStock, map[uuid.UUID]uuid.UUID, error)
}

// stock takes over the valuations and stock of the materials the run
// loads (stocksOf), each once: a valuation an earlier run took over is
// kept as it is, as SAP's stock is taken over at the cutover. It runs
// after the order item locks and the receipts and before anything posts
// to the ledger, so the run takes the goods_receipt range, then the
// material document range, then the journal's, as a goods receipt into
// stock does (ADR-0008): it never waits for an order item, goods_receipt
// or material_document while it holds a later one. The receipts before it
// post no entry (PostLoadedReceipt; SAP's is in the journal), so the run
// holds no journal range until its first initial entry, and a goods
// movement (material_document, then journal_we) takes no lock the run
// takes before material_document.
func (l *loader) stock(ctx context.Context) error {
	plants, _ := plantsOf(l.ext)                     // the quality gate refused their errors
	mats, _ := materialsOf(l.ext, l.t.units, plants) // and reported the materials held back
	stocks, _ := stocksOf(l.ext, l.t, plants, mats)  // and refused the stock it cannot take over
	for _, s := range stocks {
		if _, ok := l.refs["MBEW/"+s.key()]; ok {
			continue
		}
		if err := l.takeOver(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// takeOver values a material at a plant and posts its stock as one
// initial entry, keeping the valuation as MBEW/<BWKEY>/<MATNR> and the
// material document as MKPF/<BWKEY>/<MATNR>. A take-over the services
// refuse is a load refusal, and so is one whose entry falls in another
// fiscal year than the run's: the entry is dated the company's date today,
// and the run's trial balance and stock controls are of its fiscal year.
func (l *loader) takeOver(ctx context.Context, s sapStock) error {
	material, plant := l.refs["MARA/"+s.matnr], l.refs["T001W/"+s.werks]
	if material == uuid.Nil || plant == uuid.Nil {
		l.issue("MBEW", s.key(), "material "+s.matnr+" or plant "+s.werks+" was not loaded")
		return nil
	}
	cur := l.t.entities[s.company].LocalCurrency
	in := inventory.StockTakeOver{Valuation: inventory.ValuationInput{MaterialID: material, PlantID: plant, ValuationClass: s.class,
		PriceControl: s.control, Price: l.plain(s.price, cur), Currency: cur, PriceUnit: s.priceUnit},
		HeaderText: "Migration run " + l.run.ID.String() + ": SAP stock of material " + s.matnr + " at plant " + s.werks}
	for _, at := range s.locations {
		id, ok := l.refs["T001L/"+s.werks+"/"+at.lgort]
		if !ok {
			l.issue("MARD", s.matnr+"/"+s.werks+"/"+at.lgort, "storage location "+at.lgort+" of plant "+s.werks+" was not loaded")
			return nil
		}
		it := inventory.TakeOverItem{StorageLocationID: id, Quantity: at.quantity}
		if s.control == inventory.MovingAverage {
			it.Amount = &inventory.Amount{Value: l.plain(at.amount, cur), Currency: cur}
		}
		in.Items = append(in.Items, it)
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
			l.issue("MBEW", s.key(), "the stock is taken over on the company's date today, in fiscal year "+strconv.Itoa(int(*d.EntryYear))+
				"; take it over in a run of that fiscal year")
		}
	}
	return nil
}
