package integration

import (
	"bytes"
	"context"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/sap"
)

// lockOrderItems locks the platform's order items that the take-over of
// invoiced quantities and the receipts not yet loaded name, every one
// before either step takes a lock of its own: order by order in id order,
// each order's items in item order (Orders.LockItems), the order a goods
// movement locks them in (procurement's lockItems). The receipts and the
// take-over then lock only items the load already holds, after it holds
// the goods_receipt range too. A document either step leaves out (refused
// or waiting for an order) only keeps its items locked until the load
// commits.
func (l *loader) lockOrderItems(ctx context.Context) error {
	nos := itemNos(l.ext)
	byOrder := map[uuid.UUID][]int32{}
	add := func(ebeln, ebelp string) {
		if po, ok := l.refs["EKKO/"+ebeln]; ok {
			byOrder[po] = append(byOrder[po], nos[ebeln+"/"+ebelp])
		}
	}
	docs, _ := postedDocuments(l.ext, "") // the issues are the quality gate's, which stopped the load on any
	for _, d := range docs {
		if _, loaded := l.refs["RBKP/"+d.key]; !loaded {
			for _, it := range d.items {
				add(it.EBELN, it.EBELP)
			}
		}
	}
	receipts := receiptDocs(l.ext)
	for key, lines := range receipts {
		if _, loaded := l.refs["MSEG/"+key]; !loaded {
			for _, m := range lines {
				add(m.EBELN, m.EBELP)
			}
		}
	}
	orders := slices.Collect(maps.Keys(byOrder))
	slices.SortFunc(orders, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, o := range orders {
		if err := l.e.OrderLocks.LockItems(ctx, o, byOrder[o]); err != nil {
			return err
		}
	}
	return nil
}

// receiptDocs are the extract's goods receipts (movement type 101), the
// lines of each by MBLNR/MJAHR.
func receiptDocs(e *sap.Extract) map[string][]sap.MSEG {
	docs := map[string][]sap.MSEG{}
	for _, m := range e.MSEG {
		if m.BWART == "101" {
			docs[m.MBLNR+"/"+m.MJAHR] = append(docs[m.MBLNR+"/"+m.MJAHR], m)
		}
	}
	return docs
}
