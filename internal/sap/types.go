// Package sap reads data out of SAP ECC / S/4HANA, which stays read-only
// during migration (01-requirements §1.4; tables mapped as in 03-data-model
// §3.17). Structs mirror SAP tables (field names unchanged) so mapping specs
// can be reviewed by SAP functional consultants. Amounts and quantities are
// decimal strings, dates YYYYMMDD.
package sap

// T001 - company codes.
type T001 struct {
	BUKRS string `json:"BUKRS"`
	BUTXT string `json:"BUTXT"`
	WAERS string `json:"WAERS"`
	LAND1 string `json:"LAND1,omitempty"`
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

// LFBK - vendor bank details: the bank country, the bank key ("this may
// be the bank number": a country-specific bank identification) and the
// bank account number (SAP Help, "Defining Business Partner Banks",
// https://help.sap.com/saphelp_46c/helpdata/en/9c/944bb81ca911d2954b00a0c9426c73/content.htm).
type LFBK struct {
	LIFNR string `json:"LIFNR"`
	BANKS string `json:"BANKS"`
	BANKN string `json:"BANKN"`
	// BANKL is the bank key. A Canadian account (BANKS "CA") is loaded
	// only with its bank key in the Canadian Payments Association's
	// electronic routing form, 0 + institution + transit (ADR-0038, T72d);
	// which form a given SAP system keeps it in is its customizing, still
	// to be confirmed for the pilot's.
	BANKL string `json:"BANKL,omitempty"`
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

// --- Customizing (configuration) tables, read by the configuration agent ---

// T052 - payment terms.
type T052 struct {
	ZTERM string `json:"ZTERM"`
	// ZTAG1/ZPRZ1 and ZTAG2/ZPRZ2 are the cash discount tiers (days,
	// percent) and ZTAG3 the net days; a position without a percent holds
	// the net days, so a net-only term has them in ZTAG1.
	ZTAG1 string `json:"ZTAG1"`
	ZPRZ1 string `json:"ZPRZ1,omitempty"`
	ZTAG2 string `json:"ZTAG2,omitempty"`
	ZPRZ2 string `json:"ZPRZ2,omitempty"`
	ZTAG3 string `json:"ZTAG3,omitempty"`
	// ZMONA1-3 and ZFAEL1-3 are additional months and a fixed day of each
	// position ("2% by the 15th of next month"), which the configuration's
	// days cannot hold.
	ZMONA1 string `json:"ZMONA1,omitempty"`
	ZFAEL1 string `json:"ZFAEL1,omitempty"`
	ZMONA2 string `json:"ZMONA2,omitempty"`
	ZFAEL2 string `json:"ZFAEL2,omitempty"`
	ZMONA3 string `json:"ZMONA3,omitempty"`
	ZFAEL3 string `json:"ZFAEL3,omitempty"`
	TEXT1  string `json:"TEXT1"`
}

// T007A - tax codes (flattened with the condition rate and G/L account).
type T007A struct {
	BUKRS string `json:"BUKRS"`
	MWSKZ string `json:"MWSKZ"`
	TEXT1 string `json:"TEXT1"`
	KBETR string `json:"KBETR"` // rate in percent
	KONTS string `json:"KONTS"` // tax account
}

// T030 - automatic account determination (KTOSL transaction key, e.g. VST input tax).
type T030 struct {
	BUKRS string `json:"BUKRS"`
	KTOSL string `json:"KTOSL"`
	KONTS string `json:"KONTS"`
}

// T169G - invoice verification tolerance limits (TOLSL PP = price variance).
type T169G struct {
	BUKRS string `json:"BUKRS"`
	TOLSL string `json:"TOLSL"`
	PROZ  string `json:"PROZ"`  // percent
	WERTO string `json:"WERTO"` // absolute upper limit, company currency
}

// T012K - house bank accounts.
type T012K struct {
	BUKRS string `json:"BUKRS"`
	HBKID string `json:"HBKID"`
	HKONT string `json:"HKONT"` // G/L account of the bank
	IBAN  string `json:"IBAN"`
	SWIFT string `json:"SWIFT"`
	// BANKS and BANKL are the house bank's bank country and bank key and
	// BANKN the account's bank account number, flattened in here as IBAN
	// and SWIFT are. That SAP keeps the first two with the house bank (T012)
	// rather than its account is an assumption to confirm. A house bank has a
	// bank country, a bank key (for Germany the bank number), an account ID
	// and a bank account number, which is the company's account with the
	// bank (SAP Help, "Necessary Customizing for the Electronic Bank
	// Statement",
	// https://help.sap.com/saphelp_46c/helpdata/en/64/20b8398a9111d2bc970060087d1bfc/content.htm).
	// A Canadian account has no IBAN and is taken from these (T72d); the
	// table and field names are the ABAP dictionary's as published mirrors
	// show them, still to be confirmed by an SAP specialist.
	BANKS string `json:"BANKS,omitempty"`
	BANKL string `json:"BANKL,omitempty"`
	BANKN string `json:"BANKN,omitempty"`
}

// AGR_DEFINE - roles.
type AGRDefine struct {
	AGR_NAME string `json:"AGR_NAME"`
	TEXT     string `json:"TEXT"`
}

// AGR_TCODES - transactions in a role.
type AGRTcode struct {
	AGR_NAME string `json:"AGR_NAME"`
	TCODE    string `json:"TCODE"`
}

// AGR_USERS - role assignments.
type AGRUser struct {
	AGR_NAME string `json:"AGR_NAME"`
	UNAME    string `json:"UNAME"`
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

	T052       []T052      `json:"T052,omitempty"`
	T007A      []T007A     `json:"T007A,omitempty"`
	T030       []T030      `json:"T030,omitempty"`
	T169G      []T169G     `json:"T169G,omitempty"`
	T012K      []T012K     `json:"T012K,omitempty"`
	AGR_DEFINE []AGRDefine `json:"AGR_DEFINE,omitempty"`
	AGR_TCODES []AGRTcode  `json:"AGR_TCODES,omitempty"`
	AGR_USERS  []AGRUser   `json:"AGR_USERS,omitempty"`
}

// Counts returns row counts per table, for logs and reconciliation.
func (e *Extract) Counts() map[string]int {
	return map[string]int{
		"T001": len(e.T001), "SKB1": len(e.SKB1), "LFA1": len(e.LFA1), "LFB1": len(e.LFB1),
		"LFBK": len(e.LFBK), "EKKO": len(e.EKKO), "EKPO": len(e.EKPO), "MSEG": len(e.MSEG),
		"RBKP": len(e.RBKP), "RSEG": len(e.RSEG), "ACDOCA": len(e.ACDOCA),
		"T052": len(e.T052), "T007A": len(e.T007A), "T030": len(e.T030), "T169G": len(e.T169G),
		"T012K": len(e.T012K), "AGR_DEFINE": len(e.AGR_DEFINE), "AGR_TCODES": len(e.AGR_TCODES), "AGR_USERS": len(e.AGR_USERS),
	}
}
