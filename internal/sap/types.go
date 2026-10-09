// Package sap reads data out of SAP ECC / S/4HANA. Structs mirror SAP tables
// (field names unchanged) so mapping specs can be reviewed by SAP functional
// consultants. Amounts and quantities are decimal strings, dates YYYYMMDD.
package sap

// T001 - company codes.
type T001 struct {
	BUKRS string `json:"BUKRS"`
	BUTXT string `json:"BUTXT"`
	WAERS string `json:"WAERS"`
}

// SKB1 - G/L account master per company code, flattened with SKA1/SKAT.
type SKB1 struct {
	BUKRS string `json:"BUKRS"`
	SAKNR string `json:"SAKNR"`
	TXT50 string `json:"TXT50"`
	// XBILK "X" = balance sheet account, "" = P&L account.
	XBILK string `json:"XBILK"`
	// MITKZ reconciliation account type: "K" = vendors, "D" = customers.
	MITKZ string `json:"MITKZ"`
}

// LFA1 - vendor general data.
type LFA1 struct {
	LIFNR string `json:"LIFNR"`
	NAME1 string `json:"NAME1"`
	LAND1 string `json:"LAND1"`
	STCD1 string `json:"STCD1"`
	// SPERR "X" = central posting block.
	SPERR string `json:"SPERR"`
}

// LFB1 - vendor company code data.
type LFB1 struct {
	LIFNR string `json:"LIFNR"`
	BUKRS string `json:"BUKRS"`
	AKONT string `json:"AKONT"` // reconciliation account
	ZTERM string `json:"ZTERM"` // payment terms key
}

// LFBK - vendor bank details.
type LFBK struct {
	LIFNR string `json:"LIFNR"`
	BANKS string `json:"BANKS"`
	BANKN string `json:"BANKN"`
}

// EKKO - purchasing document header.
type EKKO struct {
	EBELN string `json:"EBELN"`
	BUKRS string `json:"BUKRS"`
	LIFNR string `json:"LIFNR"`
	WAERS string `json:"WAERS"`
	BEDAT string `json:"BEDAT"`
}

// EKPO - purchasing document item, with the G/L account from EKKN joined in as SAKTO.
type EKPO struct {
	EBELN string `json:"EBELN"`
	EBELP string `json:"EBELP"`
	MATNR string `json:"MATNR"`
	TXZ01 string `json:"TXZ01"`
	MENGE string `json:"MENGE"`
	NETPR string `json:"NETPR"`
	PEINH string `json:"PEINH"` // price unit
	SAKTO string `json:"SAKTO"`
}

// MSEG - goods movements (BWART 101 = goods receipt for PO, 102 = reversal).
type MSEG struct {
	MBLNR string `json:"MBLNR"`
	MJAHR string `json:"MJAHR"`
	ZEILE string `json:"ZEILE"`
	BWART string `json:"BWART"`
	EBELN string `json:"EBELN"`
	EBELP string `json:"EBELP"`
	MENGE string `json:"MENGE"`
	BUDAT string `json:"BUDAT"`
}

// RBKP - supplier invoice header (logistics invoice verification).
type RBKP struct {
	BELNR  string `json:"BELNR"`
	GJAHR  string `json:"GJAHR"`
	BUKRS  string `json:"BUKRS"`
	LIFNR  string `json:"LIFNR"`
	XBLNR  string `json:"XBLNR"` // vendor's invoice reference
	BLDAT  string `json:"BLDAT"`
	WAERS  string `json:"WAERS"`
	RMWWR  string `json:"RMWWR"`  // gross amount
	WMWST1 string `json:"WMWST1"` // tax amount
	// RBSTAT "A" = parked, "5" = posted.
	RBSTAT string `json:"RBSTAT"`
}

// RSEG - supplier invoice item referencing a PO item.
type RSEG struct {
	BELNR string `json:"BELNR"`
	GJAHR string `json:"GJAHR"`
	BUZEI string `json:"BUZEI"`
	EBELN string `json:"EBELN"`
	EBELP string `json:"EBELP"`
	MENGE string `json:"MENGE"`
	WRBTR string `json:"WRBTR"`
}

// ACDOCA - Universal Journal line item.
type ACDOCA struct {
	RLDNR  string `json:"RLDNR"` // ledger, "0L" = leading
	RBUKRS string `json:"RBUKRS"`
	GJAHR  string `json:"GJAHR"`
	BELNR  string `json:"BELNR"`
	DOCLN  string `json:"DOCLN"`
	RACCT  string `json:"RACCT"`
	HSL    string `json:"HSL"` // company code currency, signed: debit positive
	BUDAT  string `json:"BUDAT"`
	BLART  string `json:"BLART"`
	LIFNR  string `json:"LIFNR"`
	XBLNR  string `json:"XBLNR"`
	AUGBL  string `json:"AUGBL"` // clearing document, empty if open
}

// Extract is one consistent snapshot of the tables needed for migration.
type Extract struct {
	T001   []T001   `json:"T001"`
	SKB1   []SKB1   `json:"SKB1"`
	LFA1   []LFA1   `json:"LFA1"`
	LFB1   []LFB1   `json:"LFB1"`
	LFBK   []LFBK   `json:"LFBK"`
	EKKO   []EKKO   `json:"EKKO"`
	EKPO   []EKPO   `json:"EKPO"`
	MSEG   []MSEG   `json:"MSEG"`
	RBKP   []RBKP   `json:"RBKP"`
	RSEG   []RSEG   `json:"RSEG"`
	ACDOCA []ACDOCA `json:"ACDOCA"`
}

// Counts returns row counts per table, for logs and reconciliation.
func (e *Extract) Counts() map[string]int {
	return map[string]int{
		"T001": len(e.T001), "SKB1": len(e.SKB1), "LFA1": len(e.LFA1), "LFB1": len(e.LFB1),
		"LFBK": len(e.LFBK), "EKKO": len(e.EKKO), "EKPO": len(e.EKPO), "MSEG": len(e.MSEG),
		"RBKP": len(e.RBKP), "RSEG": len(e.RSEG), "ACDOCA": len(e.ACDOCA),
	}
}
