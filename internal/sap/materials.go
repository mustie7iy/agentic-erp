package sap

import "cmp"

// --- Material master and plants (ADR-0082 As built S2o) ---
//
// The tables and fields below are those docs/design/plan-supply-chain.md
// §S2o names for the migration of the material master into materials and
// tenancy. Every name, and what each field is taken to mean here, is
// stated from memory and pending the SAP specialist's confirmation on the
// pilot's release (docs/migration.md, "What to check on a real SAP
// system"). There is no OData mapping for them yet (file extract only).

// MARA - general material data: the material number MATNR, its material
// type MTART (ROH raw material, HALB semi-finished, FERT finished good,
// HAWA trading good, NLAG non-stock, DIEN service in the standard
// system), its material group MATKL, its base unit of measure MEINS and
// LVORM, "X" when the material is flagged for deletion at client level.
// MATNR and MEINS are taken as the extract writes them: whether a table
// export writes MATNR with its leading zeros and MEINS in SAP's internal
// form (T006-MSEHI) or its commercial one is the specialist's to confirm.
// The base unit's measures (S1a3): its gross and net weight BRGEW and
// NTGEW in weight unit GEWEI, its volume VOLUM in volume unit VOLEH and
// its EAN/UPC EAN11, plain decimals, "0" or empty for none.
type MARA struct {
	MATNR string `json:"MATNR"`
	MTART string `json:"MTART"`
	MATKL string `json:"MATKL"`
	MEINS string `json:"MEINS"`
	LVORM string `json:"LVORM,omitempty"`
	BRGEW string `json:"BRGEW,omitempty"`
	NTGEW string `json:"NTGEW,omitempty"`
	GEWEI string `json:"GEWEI,omitempty"`
	VOLUM string `json:"VOLUM,omitempty"`
	VOLEH string `json:"VOLEH,omitempty"`
	EAN11 string `json:"EAN11,omitempty"`
}

// MAKT - material descriptions: the description MAKTX of material MATNR
// in language SPRAS. The platform keeps one description (ADR-0082 §4).
type MAKT struct {
	MATNR string `json:"MATNR"`
	SPRAS string `json:"SPRAS"`
	MAKTX string `json:"MAKTX"`
}

// MARM - units of measure of a material: one alternative unit MEINH is
// UMREZ / UMREN base units (numerator and denominator of the conversion
// to the base unit). SAP also holds a row for the base unit itself, 1/1.
// The unit's measures (S1a3): its gross weight BRGEW in GEWEI, its volume
// VOLUM in VOLEH, its length, width and height LAENG, BREIT and HOEHE in
// MEABM, and its EAN/UPC EAN11 (MARM has no net weight).
type MARM struct {
	MATNR string `json:"MATNR"`
	MEINH string `json:"MEINH"`
	UMREZ string `json:"UMREZ"`
	UMREN string `json:"UMREN"`
	BRGEW string `json:"BRGEW,omitempty"`
	GEWEI string `json:"GEWEI,omitempty"`
	VOLUM string `json:"VOLUM,omitempty"`
	VOLEH string `json:"VOLEH,omitempty"`
	LAENG string `json:"LAENG,omitempty"`
	BREIT string `json:"BREIT,omitempty"`
	HOEHE string `json:"HOEHE,omitempty"`
	MEABM string `json:"MEABM,omitempty"`
	EAN11 string `json:"EAN11,omitempty"`
}

// MARC - plant data of a material: material MATNR at plant WERKS, LVORM
// "X" when flagged for deletion at that plant.
type MARC struct {
	MATNR string `json:"MATNR"`
	WERKS string `json:"WERKS"`
	LVORM string `json:"LVORM,omitempty"`
}

// MARD - storage location data of a material: material MATNR at storage
// location LGORT of plant WERKS, LVORM as MARC's, and LABST, its
// unrestricted-use stock in the base unit (MEINS), a plain decimal; empty
// is none. The migration of the material master reads the assignment
// (S2o), the stock migration LABST (S2p). SAP's other stock types
// (quality inspection INSME, blocked SPEME, restricted EINME, in transfer
// UMLME, returns RETME) are not extracted: a valuation whose stock is not
// the sum of its storage locations' LABST refuses the run (S2p).
type MARD struct {
	MATNR string `json:"MATNR"`
	WERKS string `json:"WERKS"`
	LGORT string `json:"LGORT"`
	LVORM string `json:"LVORM,omitempty"`
	LABST string `json:"LABST,omitempty"`
}

// MBEW - material valuation (S2p): material MATNR in valuation area BWKEY
// (a plant's, T001W-BWKEY, where valuation is at plant level), the
// valuation type BWTAR (empty unless the material is split-valued), its
// price control VPRSV (S standard, V moving average), moving price
// VERPR and standard price STPRS in company code currency per price unit
// PEINH of the base unit, valuation class BKLAS, and the valuated stock
// LBKUM (base unit) and its value SALK3 (company code currency). Amounts
// are written as the extract writes HSL (TCURX applies).
type MBEW struct {
	MATNR string `json:"MATNR"`
	BWKEY string `json:"BWKEY"`
	BWTAR string `json:"BWTAR,omitempty"`
	VPRSV string `json:"VPRSV"`
	VERPR string `json:"VERPR"`
	STPRS string `json:"STPRS"`
	PEINH string `json:"PEINH"`
	BKLAS string `json:"BKLAS"`
	LBKUM string `json:"LBKUM"`
	SALK3 string `json:"SALK3"`
}

// Price controls of a material valuation (MBEW-VPRSV), from memory.
const (
	PriceControlStandard = "S"
	PriceControlMoving   = "V"
)

// T001W - plants: plant WERKS named NAME1, with its valuation area BWKEY
// and the company code BUKRS of that area flattened in from T001K (by
// BWKEY), as EKPO carries EKKN's account: the plant's company code, which
// the platform's plant belongs to.
type T001W struct {
	WERKS string `json:"WERKS"`
	NAME1 string `json:"NAME1"`
	BWKEY string `json:"BWKEY,omitempty"`
	BUKRS string `json:"BUKRS"`
}

// T001L - storage locations: storage location LGORT of plant WERKS, named
// LGOBE. SAP keys a storage location by its plant.
type T001L struct {
	WERKS string `json:"WERKS"`
	LGORT string `json:"LGORT"`
	LGOBE string `json:"LGOBE"`
}

// Material type keys of SAP's standard system (MARA-MTART), from memory.
const (
	MaterialTypeRaw          = "ROH"
	MaterialTypeSemiFinished = "HALB"
	MaterialTypeFinished     = "FERT"
	MaterialTypeTrading      = "HAWA"
	MaterialTypeNonStock     = "NLAG"
	MaterialTypeService      = "DIEN"
)

// Deleted is the value of a deletion flag (LVORM) that is set.
const Deleted = "X"

// filterMaterials keeps the plants of the scope's company codes
// (T001W-BUKRS) with their storage locations, the materials with plant
// data at one of them, and of those their descriptions, units, plant data
// and storage location data at the scope's plants and their valuations
// in the scope's plants' valuation areas (MBEW, S2p).
func filterMaterials(all, out *Extract, inCC func(string) bool) {
	out.T001W = keep(all.T001W, func(r T001W) bool { return inCC(r.BUKRS) })
	plants := map[string]bool{}
	for _, r := range out.T001W {
		plants[r.WERKS] = true
	}
	out.T001L = keep(all.T001L, func(r T001L) bool { return plants[r.WERKS] })
	out.MARC = keep(all.MARC, func(r MARC) bool { return plants[r.WERKS] })
	materials := map[string]bool{}
	for _, r := range out.MARC {
		materials[r.MATNR] = true
	}
	out.MARA = keep(all.MARA, func(r MARA) bool { return materials[r.MATNR] })
	out.MAKT = keep(all.MAKT, func(r MAKT) bool { return materials[r.MATNR] })
	out.MARM = keep(all.MARM, func(r MARM) bool { return materials[r.MATNR] })
	out.MARD = keep(all.MARD, func(r MARD) bool { return plants[r.WERKS] && materials[r.MATNR] })
	areas := map[string]bool{}
	for _, r := range out.T001W {
		areas[cmp.Or(r.BWKEY, r.WERKS)] = true // a plant without BWKEY is its own valuation area, as the load reads it
	}
	out.MBEW = keep(all.MBEW, func(r MBEW) bool { return areas[r.BWKEY] && materials[r.MATNR] })
}
