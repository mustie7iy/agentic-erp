package sap

// mockMaterial is a material of the mock: its MARA row, its descriptions
// by language, its alternative units (MEINH, UMREZ, UMREN) beside the
// base unit's own 1/1 row, its storage locations by plant, its
// unrestricted stock by WERKS/LGORT (MARD LABST) and its valuations
// (MBEW, S2p), MATNR left to the material's.
type mockMaterial struct {
	mara       MARA
	texts      [][2]string
	units      [][3]string
	locations  map[string][]string
	stock      map[string]string
	valuations []MBEW
}

// mockMaterials are company 3000's materials (S2o) with their stock and
// valuations (S2p), all of valuation class 3100: steel sheet bought by
// the kilogram and the tonne, with a German description beside its
// English one; a shelving unit the company makes, kept at both plants
// and sold by the box of 4; a hex bolt it trades by the piece, kept in
// Laval's main and raw materials stores; a washer
// whose base unit is SAP's internal key for a piece (ST), which is no
// unit of the platform, and a spacer sold by the carton (CAR), no unit
// of the platform either, both held back with their reason; and a gasket
// flagged for deletion, which is not migrated. Steel sheet and hex bolts
// are valued at a moving price (1,200 KG worth 3,000.00 at Mississauga;
// 6,500 PC at 12.00 per 100 at Laval's two stores), shelving at a
// standard price of 85.00 at both plants; the washer's 300 pieces are
// valued too, but it is held back, so its stock is not taken over.
var mockMaterials = []mockMaterial{
	{MARA{MATNR: "000000000000100001", MTART: MaterialTypeRaw, MATKL: "STEEL", MEINS: "KG"},
		[][2]string{{"D", "Stahlblech 2 mm"}, {"E", "Steel sheet 2 mm"}}, [][3]string{{"T", "1000", "1"}}, map[string][]string{"3100": {"0001"}},
		map[string]string{"3100/0001": "1200.000"},
		[]MBEW{{BWKEY: "3100", VPRSV: PriceControlMoving, VERPR: "2.50", STPRS: "0.00", PEINH: "1", BKLAS: "3100", LBKUM: "1200.000", SALK3: "3000.00"}}},
	{MARA{MATNR: "000000000000200001", MTART: MaterialTypeFinished, MATKL: "SHELVING", MEINS: "EA"},
		[][2]string{{"E", "Shelving unit, 5 tiers"}}, [][3]string{{"BOX", "4", "1"}}, map[string][]string{"3100": {"0002"}, "3200": {"0003"}},
		map[string]string{"3100/0002": "40.000", "3200/0003": "12.000"},
		[]MBEW{{BWKEY: "3100", VPRSV: PriceControlStandard, VERPR: "84.10", STPRS: "85.00", PEINH: "1", BKLAS: "3100", LBKUM: "40.000", SALK3: "3400.00"},
			{BWKEY: "3200", VPRSV: PriceControlStandard, VERPR: "85.00", STPRS: "85.00", PEINH: "1", BKLAS: "3100", LBKUM: "12.000", SALK3: "1020.00"}}},
	{MARA{MATNR: "000000000000300001", MTART: MaterialTypeTrading, MATKL: "FASTENERS", MEINS: "PC"},
		[][2]string{{"E", "Hex bolt M8"}}, nil, map[string][]string{"3200": {"0001", "0003"}},
		map[string]string{"3200/0001": "1500.000", "3200/0003": "5000.000"},
		[]MBEW{{BWKEY: "3200", VPRSV: PriceControlMoving, VERPR: "12.00", STPRS: "0.00", PEINH: "100", BKLAS: "3100", LBKUM: "6500.000", SALK3: "780.00"}}},
	{MARA{MATNR: "000000000000300002", MTART: MaterialTypeTrading, MATKL: "FASTENERS", MEINS: "ST"},
		[][2]string{{"E", "Washer M8"}}, nil, map[string][]string{"3200": {"0003"}}, map[string]string{"3200/0003": "300.000"},
		[]MBEW{{BWKEY: "3200", VPRSV: PriceControlMoving, VERPR: "0.05", STPRS: "0.00", PEINH: "1", BKLAS: "3100", LBKUM: "300.000", SALK3: "15.00"}}},
	{MARA{MATNR: "000000000000300003", MTART: MaterialTypeTrading, MATKL: "FASTENERS", MEINS: "EA"},
		[][2]string{{"E", "Spacer M8"}}, [][3]string{{"CAR", "500", "1"}}, map[string][]string{"3200": {"0003"}}, nil, nil},
	{MARA{MATNR: "000000000000900001", MTART: MaterialTypeRaw, MATKL: "RUBBER", MEINS: "EA", LVORM: Deleted},
		[][2]string{{"E", "Gasket, discontinued"}}, nil, map[string][]string{"3100": {"0001"}}, nil, nil},
}

// materials adds the company's plants and storage locations (T001W,
// T001L) and its material master (MARA, MAKT, MARM, MARC, MARD; S2o): the
// Mississauga plant 3100 with its raw materials and finished goods
// stores, the Laval plant 3200 with its main store and a raw materials
// store of the same code as Mississauga's, 0001 (SAP keys a storage
// location by its plant), and mockMaterials.
// It draws nothing from the generator, so the rest of the extract is the
// same as without it.
func (g *mockGen) materials(cc string) {
	g.e.T001W = append(g.e.T001W, T001W{WERKS: "3100", NAME1: "Mississauga plant", BWKEY: "3100", BUKRS: cc},
		T001W{WERKS: "3200", NAME1: "Laval plant", BWKEY: "3200", BUKRS: cc})
	g.e.T001L = append(g.e.T001L, T001L{WERKS: "3100", LGORT: "0001", LGOBE: "Raw materials"},
		T001L{WERKS: "3100", LGORT: "0002", LGOBE: "Finished goods"}, T001L{WERKS: "3200", LGORT: "0001", LGOBE: "Raw materials"},
		T001L{WERKS: "3200", LGORT: "0003", LGOBE: "Main store"})
	for _, m := range mockMaterials {
		g.e.MARA = append(g.e.MARA, m.mara)
		for _, t := range m.texts {
			g.e.MAKT = append(g.e.MAKT, MAKT{MATNR: m.mara.MATNR, SPRAS: t[0], MAKTX: t[1]})
		}
		g.e.MARM = append(g.e.MARM, MARM{MATNR: m.mara.MATNR, MEINH: m.mara.MEINS, UMREZ: "1", UMREN: "1"})
		for _, u := range m.units {
			g.e.MARM = append(g.e.MARM, MARM{MATNR: m.mara.MATNR, MEINH: u[0], UMREZ: u[1], UMREN: u[2]})
		}
		for _, plant := range []string{"3100", "3200"} {
			locations, ok := m.locations[plant]
			if !ok {
				continue
			}
			g.e.MARC = append(g.e.MARC, MARC{MATNR: m.mara.MATNR, WERKS: plant, LVORM: m.mara.LVORM})
			for _, l := range locations {
				g.e.MARD = append(g.e.MARD, MARD{MATNR: m.mara.MATNR, WERKS: plant, LGORT: l, LVORM: m.mara.LVORM, LABST: m.stock[plant+"/"+l]})
			}
		}
		for _, v := range m.valuations {
			v.MATNR = m.mara.MATNR
			g.e.MBEW = append(g.e.MBEW, v)
		}
	}
}
