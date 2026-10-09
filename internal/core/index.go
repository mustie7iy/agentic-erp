package core

import "slices"

// index holds lookups derived from the store's maps so matching and payment
// runs avoid full scans. It indexes by fields that never change after a
// record is created (an invoice's vendor and PO, a receipt's PO), and is
// rebuilt whenever the number of invoices or receipts changes. Statuses
// and amounts are always read from the live records.
type index struct {
	invoices, receipts int
	byVendor           map[string][]string // vendor -> invoice ids
	byPO               map[string][]string // PO -> invoice ids
	grByPO             map[string][]string // PO -> receipt ids, sorted
}

func (s *Store) idx() *index {
	if s.index != nil && s.index.invoices == len(s.Invoices) && s.index.receipts == len(s.GoodsReceipts) {
		return s.index
	}
	ix := &index{invoices: len(s.Invoices), receipts: len(s.GoodsReceipts),
		byVendor: map[string][]string{}, byPO: map[string][]string{}, grByPO: map[string][]string{}}
	for id, inv := range s.Invoices {
		ix.byVendor[inv.VendorID] = append(ix.byVendor[inv.VendorID], id)
		if inv.POID != "" {
			ix.byPO[inv.POID] = append(ix.byPO[inv.POID], id)
		}
	}
	for id, g := range s.GoodsReceipts {
		ix.grByPO[g.POID] = append(ix.grByPO[g.POID], id)
	}
	for _, m := range []map[string][]string{ix.byVendor, ix.byPO, ix.grByPO} {
		for k := range m {
			slices.Sort(m[k])
		}
	}
	s.index = ix
	return ix
}
