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
	// ZWELS is the list of the payment methods to be considered (data
	// element DZWELS, CHAR 10): one character per method (ZLSCH). The
	// partner migration loads it, in its order, as the supplier's methods
	// in the company (ADR-0038). The field and its data element are the
	// ABAP dictionary's as a published mirror shows them
	// (https://leanx.eu/en/sap/table/lfb1.html); that the order of its
	// characters is SAP's order of preference is to be confirmed by an SAP
	// specialist.
	ZWELS string `json:"ZWELS,omitempty"`
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
	// XRECH is the document's direction (data element XRECH, "Indicator:
	// post invoice"): "X" an invoice, "" a credit memo. It is a pointer
	// because an absent indicator and a credit memo's blank one differ: an
	// extract without it (the OData mapping does not yet carry it) cannot
	// tell an invoice from a credit memo, and the quality gate refuses its
	// parked documents rather than guess (T72b). The field and its values
	// are from the ABAP dictionary as published mirrors show it, still to be
	// confirmed by an SAP specialist on the pilot's release.
	XRECH *string `json:"XRECH,omitempty"`
}

// Indicator is an SAP one-character indicator as the extract holds an
// optional one: XRECH: sap.Indicator("X").
func Indicator(v string) *string { return &v }

// RSEG - supplier invoice item referencing a PO item.
type RSEG struct {
	BELNR string `json:"BELNR"`
	GJAHR string `json:"GJAHR"`
	BUZEI string `json:"BUZEI"`
	EBELN string `json:"EBELN"`
	EBELP string `json:"EBELP"`
	MENGE string `json:"MENGE"`
	WRBTR string `json:"WRBTR"`
	// TBTKZ "X" marks a subsequent debit/credit item: a revaluation of what
	// was invoiced that moves no quantity (SAP Help, "Subsequent
	// Debits/Credits"); "" an ordinary item. SAP keeps the indicator per
	// item, and a document whose items all carry it is a subsequent debit
	// (XRECH "X") or credit (XRECH ""), as SAP Help, "Posting Subsequent
	// Debit/Credits", describes entering one (T72b).
	TBTKZ string `json:"TBTKZ,omitempty"`
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
	// RWCUR is the line's transaction (document) currency and WSL its
	// amount in it, signed as HSL. SAP names WSL "Amount in Transaction
	// Currency" (SAP KBA 3788925,
	// https://userapps.support.sap.com/sap/support/knowledge/en/3788925);
	// that RWCUR is its currency key, the original transaction currency, is
	// from the ABAP dictionary as published mirrors show it, still to be
	// confirmed by an SAP specialist on the pilot's release. The load posts
	// HSL only, so the quality gate refuses an open item whose RWCUR is not
	// the company's currency, or is absent (the OData mapping does not
	// carry it yet), rather than load it in local currency. WSL is carried
	// for the migration of such items and is not read yet.
	RWCUR string `json:"RWCUR,omitempty"`
	WSL   string `json:"WSL,omitempty"`
	BUDAT string `json:"BUDAT"`
	BLART string `json:"BLART"`
	LIFNR string `json:"LIFNR"`
	XBLNR string `json:"XBLNR"`
	AUGBL string `json:"AUGBL"` // clearing document, empty if open
	// UMSKZ is a vendor line's special G/L indicator, "" on an ordinary
	// line; in the standard system "A" marks a general down payment (SAP
	// Help, "Posting Keys and Special G/L Indicators",
	// https://help.sap.com/saphelp_470/helpdata/en/01/a9c6e6455711d182b40000e829fbfe/content.htm),
	// posted to the vendor's alternative reconciliation account
	// (T074). Only A is migrated (T72c, ADR-0037). That ACDOCA carries the
	// vendor line's indicator under this name, as BSEG and BSIK do, is from
	// the ABAP dictionary as published mirrors show it, still to be
	// confirmed by an SAP specialist on the pilot's release.
	UMSKZ string `json:"UMSKZ,omitempty"`
	// MWSKZ is the line's tax code. The load expects a down payment with
	// tax to carry it on its vendor line and on its tax line (T72c), which
	// an SAP specialist is still to confirm. With net display SAP posts
	// the down payment without the tax and the tax to the input tax
	// account; with gross display the down payment includes the tax and a
	// tax clearing account offsets it (SAP Help, "Net or Gross Display of
	// Down Payments",
	// https://help.sap.com/saphelp_46c/helpdata/en/01/a9c838455711d182b40000e829fbfe/content.htm).
	// The platform keeps a down payment net (ADR-0037), so only net
	// display is migrated (the quality gate). The field name is the
	// ABAP dictionary's as published mirrors show it, to be confirmed.
	MWSKZ string `json:"MWSKZ,omitempty"`
	// EBELN and EBELP are the purchase order and item a line belongs to.
	// Goods receipts and invoice receipts of an order valued through GR/IR
	// are offset to SAP's GR/IR clearing account (T030 WRX), which the
	// invoice clears (SAP Help, "Postings at Goods Receipt and Invoice
	// Receipt",
	// https://help.sap.com/saphelp_46C/helpdata/EN/47/60ff6f49f011d1894c0000e829fbbd/content.htm);
	// the load expects every line there to name its order item and takes
	// each item's open position over onto the platform's (T72f). That
	// ACDOCA carries them under these names, as MSEG and RSEG do, and on
	// every GR/IR line, is from the ABAP dictionary as published mirrors
	// show it, still to be confirmed by an SAP specialist on the pilot's
	// release.
	EBELN string `json:"EBELN,omitempty"`
	EBELP string `json:"EBELP,omitempty"`
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
	// WAERS is the account's currency key (check table TCURC), which the
	// house bank is generated in: a foreign-currency account in its own
	// currency, not the company's (ADR-0038, T53a). The field, its data
	// element and check table are the ABAP dictionary's as a published
	// mirror shows them (https://leanx.eu/en/sap/table/t012k.html), still to
	// be confirmed by an SAP specialist, as is what an empty WAERS means
	// there; the configuration draft refuses an account without one rather
	// than guess the company's currency.
	WAERS string `json:"WAERS,omitempty"`
}

// T042Z - payment methods for automatic payment, per country: the
// country key LAND1, the payment method ZLSCH (data element DZLSCH, CHAR 1)
// and its name TEXT1. The methods are set up per country (SAP Help, "Set
// Up Payment Methods per Country for Payment Transactions",
// https://help.sap.com/saphelp_46c/helpdata/en/51/ca2d28e4ded31184080004aca6e0d1/content.htm);
// the configuration draft names a company's method by the T042Z row of the
// company's country (T001 LAND1), which an SAP specialist is to confirm is
// the paying company code's. Fields as the ABAP dictionary's published
// mirror shows them (https://leanx.eu/en/sap/table/t042z.html).
type T042Z struct {
	LAND1 string `json:"LAND1"`
	ZLSCH string `json:"ZLSCH"`
	TEXT1 string `json:"TEXT1"`
}

// T042E - company code-specific specifications for payment methods: the
// methods the paying company code ZBUKR pays by, with the minimum and
// maximum amount of a payment by the method (VONBT, BISBT; CURR), which
// the configuration has no counterpart for. Fields as the ABAP
// dictionary's published mirror shows them
// (https://leanx.eu/en/sap/table/t042e.html).
type T042E struct {
	ZBUKR string `json:"ZBUKR"`
	ZLSCH string `json:"ZLSCH"`
	VONBT string `json:"VONBT,omitempty"`
	BISBT string `json:"BISBT,omitempty"`
}

// T042A - bank selection for the payment program: for paying company code
// ZBUKR, payment method ZLSCH and currency WAERS, the house bank HBKID at
// ranking order RANGF (NUMC 5, "number that sets the ranking order of the
// entries"). The payment program takes the bank by method and currency,
// and an entry without a currency when there is none for the currency; it
// pays from the house bank of the highest priority in the ranking (SAP
// Help, "Selecting the House Bank",
// https://help.sap.com/saphelp_globext607_10/helpdata/en/01/a9bf27455711d182b40000e829fbfe/content.htm).
// Fields as the ABAP dictionary's published mirror shows them
// (https://leanx.eu/en/sap/table/t042a.html), to be confirmed by an SAP
// specialist, as is that a lower RANGF ranks first.
type T042A struct {
	ZBUKR string `json:"ZBUKR"`
	ZLSCH string `json:"ZLSCH"`
	WAERS string `json:"WAERS,omitempty"`
	RANGF string `json:"RANGF"`
	HBKID string `json:"HBKID"`
}

// T042I - account determination for the payment program: for company code
// ZBUKR, house bank HBKID, payment method ZLSCH and currency WAERS, the
// house bank's account (HKTID) and the G/L account posted to, the bank
// subaccount UKONT. The platform credits a house bank's one clearing
// account for every method, so the draft notes a subaccount that differs.
// Fields as the ABAP dictionary's published mirror shows them
// (https://leanx.eu/en/sap/table/t042i.html), to be confirmed by an SAP
// specialist.
type T042I struct {
	ZBUKR string `json:"ZBUKR"`
	HBKID string `json:"HBKID"`
	ZLSCH string `json:"ZLSCH"`
	WAERS string `json:"WAERS,omitempty"`
	HKTID string `json:"HKTID,omitempty"`
	UKONT string `json:"UKONT"`
}

// T074 - alternative reconciliation accounts of special G/L transactions:
// for account type KOART ("K" vendors) and special G/L indicator UMSKZ,
// the special G/L account SKONT that replaces the reconciliation account
// HKONT, flattened per company code as T030 is. SAP "requires a special
// G/L account for each special G/L transaction per chart of accounts",
// defined by account type, special G/L indicator and reconciliation
// account (SAP Help, "Special G/L Accounts",
// https://help.sap.com/saphelp_470/helpdata/en/01/a9c70d455711d182b40000e829fbfe/content.htm).
// It gives the configuration its down_payment
// account (UMSKZ "A"; ADR-0037, T72c). SAP keeps T074 per chart of
// accounts; the table and field names are the ABAP dictionary's as
// published mirrors show them, still to be confirmed by an SAP specialist.
type T074 struct {
	BUKRS string `json:"BUKRS"`
	KOART string `json:"KOART"`
	UMSKZ string `json:"UMSKZ"`
	HKONT string `json:"HKONT"`
	SKONT string `json:"SKONT"`
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
	T074       []T074      `json:"T074,omitempty"`
	T042Z      []T042Z     `json:"T042Z,omitempty"`
	T042E      []T042E     `json:"T042E,omitempty"`
	T042A      []T042A     `json:"T042A,omitempty"`
	T042I      []T042I     `json:"T042I,omitempty"`
	AGR_DEFINE []AGRDefine `json:"AGR_DEFINE,omitempty"`
	AGR_TCODES []AGRTcode  `json:"AGR_TCODES,omitempty"`
	AGR_USERS  []AGRUser   `json:"AGR_USERS,omitempty"`
}

// Counts returns row counts per table, for logs and reconciliation: the
// tables a migration run reads and the customizing its report has always
// counted. The payment method tables (T042*) are read only by the
// configuration draft (delivery/sapconfig), so a run's report does not
// count them.
func (e *Extract) Counts() map[string]int {
	return map[string]int{
		"T001": len(e.T001), "SKB1": len(e.SKB1), "LFA1": len(e.LFA1), "LFB1": len(e.LFB1),
		"LFBK": len(e.LFBK), "EKKO": len(e.EKKO), "EKPO": len(e.EKPO), "MSEG": len(e.MSEG),
		"RBKP": len(e.RBKP), "RSEG": len(e.RSEG), "ACDOCA": len(e.ACDOCA),
		"T052": len(e.T052), "T007A": len(e.T007A), "T030": len(e.T030), "T169G": len(e.T169G),
		"T012K": len(e.T012K), "T074": len(e.T074), "AGR_DEFINE": len(e.AGR_DEFINE), "AGR_TCODES": len(e.AGR_TCODES), "AGR_USERS": len(e.AGR_USERS),
	}
}
