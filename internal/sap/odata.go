package sap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ODataSource extracts from a live S/4HANA system through its standard
// OData V2 APIs (SAP API Business Hub). Entity sets and field names below
// follow the published APIs; verify them against your system's $metadata
// and adjust Entities, since releases and activated APIs differ.
//
// Every request is a read. Agents never write to SAP through this source.
type ODataSource struct {
	BaseURL  string // e.g. https://my-s4.example.com
	User     string
	Password string
	Client   string // sap-client, optional
	PageSize int
	HTTP     *http.Client
	Entities []EntityMapping
}

// EntityMapping maps one OData entity set onto one SAP table struct.
type EntityMapping struct {
	Table   string // target table, e.g. "LFA1"
	Service string // e.g. "API_BUSINESS_PARTNER"
	Entity  string // e.g. "A_Supplier"
	// Fields maps SAP table field -> OData property.
	Fields map[string]string
	// CompanyField / YearField are OData properties used for scope filters.
	CompanyField string
	YearField    string
	// Static sets constant SAP fields (e.g. RLDNR) not exposed by the API.
	Static map[string]string
}

// DefaultEntities covers the tables in Extract with standard S/4HANA APIs.
var DefaultEntities = []EntityMapping{
	{Table: "T001", Service: "API_COMPANYCODE_SRV", Entity: "A_CompanyCode", CompanyField: "CompanyCode",
		Fields: map[string]string{"BUKRS": "CompanyCode", "BUTXT": "CompanyCodeName", "WAERS": "Currency", "LAND1": "Country"}},
	{Table: "SKB1", Service: "API_GLACCOUNTINCOMPANYCODE_SRV", Entity: "A_GLAccountInCompanyCode", CompanyField: "CompanyCode",
		Fields: map[string]string{"BUKRS": "CompanyCode", "SAKNR": "GLAccount", "TXT50": "GLAccountLongName",
			"XBILK": "IsBalanceSheetAccount", "MITKZ": "ReconciliationAccountType"}},
	{Table: "LFA1", Service: "API_BUSINESS_PARTNER", Entity: "A_Supplier",
		Fields: map[string]string{"LIFNR": "Supplier", "NAME1": "SupplierName", "LAND1": "Country",
			"STCD1": "TaxNumber1", "SPERR": "PostingIsBlocked"}},
	// LFB1-ZWELS is A_SupplierCompany's PaymentMethodsList, a string, as
	// the SAP Cloud SDK's type of the entity has it
	// (https://help.sap.com/doc/505cd8d2cbd340a38eeae8b63b5df753/1.0/en-US/interfaces/_sap_cloud_sdk_vdm_business_partner_service.suppliercompanytype.html).
	{Table: "LFB1", Service: "API_BUSINESS_PARTNER", Entity: "A_SupplierCompany", CompanyField: "CompanyCode",
		Fields: map[string]string{"LIFNR": "Supplier", "BUKRS": "CompanyCode", "AKONT": "ReconciliationAccount",
			"ZTERM": "PaymentTerms", "ZWELS": "PaymentMethodsList"}},
	// A_BusinessPartnerBank keys a bank account by its business partner,
	// whose number need not be its supplier's: the rows read as PARTNER and
	// take their vendor (LIFNR) from the partner link below (attachBanks).
	{Table: "LFBK", Service: "API_BUSINESS_PARTNER", Entity: "A_BusinessPartnerBank",
		Fields: map[string]string{"PARTNER": "BusinessPartner", "BANKS": "BankCountryKey", "BANKN": "BankAccount", "BANKL": "BankNumber"}},
	// The business partner -> supplier link, A_BusinessPartner's Supplier.
	// Stated from memory, not yet from a primary source (ADR-0034 As built,
	// review:T72d): confirm it against the system's $metadata.
	{Table: PartnerLink, Service: "API_BUSINESS_PARTNER", Entity: "A_BusinessPartner",
		Fields: map[string]string{"PARTNER": "BusinessPartner", "LIFNR": "Supplier"}},
	{Table: "EKKO", Service: "API_PURCHASEORDER_PROCESS_SRV", Entity: "A_PurchaseOrder", CompanyField: "CompanyCode",
		Fields: map[string]string{"EBELN": "PurchaseOrder", "BUKRS": "CompanyCode", "LIFNR": "Supplier",
			"WAERS": "DocumentCurrency", "BEDAT": "PurchaseOrderDate"}},
	{Table: "EKPO", Service: "API_PURCHASEORDER_PROCESS_SRV", Entity: "A_PurchaseOrderItem", CompanyField: "CompanyCode",
		Fields: map[string]string{"EBELN": "PurchaseOrder", "EBELP": "PurchaseOrderItem", "MATNR": "Material",
			"TXZ01": "PurchaseOrderItemText", "MENGE": "OrderQuantity", "NETPR": "NetPriceAmount",
			"PEINH": "NetPriceQuantity", "SAKTO": "GLAccount"}},
	{Table: "MSEG", Service: "API_MATERIAL_DOCUMENT_SRV", Entity: "A_MaterialDocumentItem", YearField: "MaterialDocumentYear",
		Fields: map[string]string{"MBLNR": "MaterialDocument", "MJAHR": "MaterialDocumentYear", "ZEILE": "MaterialDocumentItem",
			"BWART": "GoodsMovementType", "EBELN": "PurchaseOrder", "EBELP": "PurchaseOrderItem",
			"MENGE": "QuantityInEntryUnit", "BUDAT": "PostingDate"}},
	// RBKP-XRECH and RSEG-TBTKZ (the document's kind, T72b) are not mapped:
	// the API's properties for them are still to be confirmed against a
	// system's $metadata. Until they are, the quality gate refuses the
	// parked invoices of an OData extract, whose XRECH is absent, rather
	// than load a credit memo as an invoice, and its posted ones, whose
	// invoiced quantities the load takes over by direction (T72b2).
	{Table: "RBKP", Service: "API_SUPPLIERINVOICE_PROCESS_SRV", Entity: "A_SupplierInvoice", CompanyField: "CompanyCode", YearField: "FiscalYear",
		Fields: map[string]string{"BELNR": "SupplierInvoice", "GJAHR": "FiscalYear", "BUKRS": "CompanyCode",
			"LIFNR": "InvoicingParty", "XBLNR": "SupplierInvoiceIDByInvcgParty", "BLDAT": "DocumentDate",
			"WAERS": "DocumentCurrency", "RMWWR": "InvoiceGrossAmount", "WMWST1": "TaxAmount", "RBSTAT": "SupplierInvoiceStatus"}},
	{Table: "RSEG", Service: "API_SUPPLIERINVOICE_PROCESS_SRV", Entity: "A_SuplrInvcItemPurOrdRef", YearField: "FiscalYear",
		Fields: map[string]string{"BELNR": "SupplierInvoice", "GJAHR": "FiscalYear", "BUZEI": "SupplierInvoiceItem",
			"EBELN": "PurchaseOrder", "EBELP": "PurchaseOrderItem", "MENGE": "QuantityInPurchaseOrderUnit",
			"WRBTR": "SupplierInvoiceItemAmount"}},
	// ACDOCA-UMSKZ and MWSKZ (a down payment's special G/L indicator and
	// tax code, T72c) are not mapped either, for the same reason: until
	// they are, the quality gate refuses an OData extract's lines on the
	// down payment account, whose indicator is absent, rather than infer it.
	// Nor are ACDOCA-EBELN and EBELP (a GR/IR line's order item, T72f), nor
	// T030: the gate refuses an OData extract's lines on the legal entity's
	// gr_ir account, which come without them, and warns that SAP's GR/IR
	// account cannot be told by T030 (WRX) for a company with receipts, so
	// lines on it are refused only when it is the legal entity's gr_ir.
	// Nor are ACDOCA-RWCUR and WSL (a line's document currency and its
	// amount in it): until the API's properties for them are confirmed, the
	// gate refuses an OData extract's open items, which come without a
	// document currency, rather than assume the company's; once they are
	// mapped (the pilot system's $metadata), such items load in their
	// document currency as a file extract's do.
	// Nor are ACDOCA-RCNTR, PRCTR and SEGMENT (a line's centers, T56h4):
	// the API's properties for them are not recorded, so an OData
	// extract's lines load without centers, the ledger deriving them as
	// for any line without, until the pilot's $metadata names them (T56h5).
	// Nor is the line's ledger: every line is labelled the leading ledger,
	// so the quality gate cannot tell an OData extract's ledgers apart as
	// it does a file extract's (review:T56h4); the API's ledger property
	// is to be mapped from the pilot's $metadata (T56h5).
	// OKB9's default account assignments (TKA3A, TKA3C, TKA3P; T56h3) have
	// no released OData API (the SAP specialist, PO 2026-11-09): only a
	// file extract carries them.
	{Table: "ACDOCA", Service: "API_JOURNALENTRYITEMBASIC_SRV", Entity: "A_JournalEntryItemBasic", CompanyField: "CompanyCode", YearField: "FiscalYear",
		Static: map[string]string{"RLDNR": LeadingLedger},
		Fields: map[string]string{"RBUKRS": "CompanyCode", "GJAHR": "FiscalYear", "BELNR": "AccountingDocument",
			"DOCLN": "LedgerGLLineItem", "RACCT": "GLAccount", "HSL": "AmountInCompanyCodeCurrency",
			"BUDAT": "PostingDate", "BLART": "AccountingDocumentType", "LIFNR": "Supplier",
			"XBLNR": "DocumentReferenceID", "AUGBL": "ClearingAccountingDocument"}},
}

func (o *ODataSource) Name() string { return "odata:" + o.BaseURL }

func (o *ODataSource) Extract(ctx context.Context, scope Scope) (*Extract, error) {
	entities := o.Entities
	if entities == nil {
		entities = DefaultEntities
	}
	tables := map[string][]map[string]string{}
	for _, em := range entities {
		for _, cc := range companyFilter(em, scope) {
			rows, err := o.fetch(ctx, em, cc, scope.FiscalYear)
			if err != nil {
				return nil, fmt.Errorf("sap: extract %s (%s/%s): %w", em.Table, em.Service, em.Entity, err)
			}
			tables[em.Table] = append(tables[em.Table], rows...)
		}
	}
	var unlinked []string
	if slices.ContainsFunc(entities, func(em EntityMapping) bool { return em.Table == "LFBK" }) {
		unlinked = attachBanks(tables)
	}
	// Assemble via JSON so each SAP field lands in its struct field by tag.
	// Every row is a map of strings and every Extract field is a slice of
	// structs of strings (and optional strings), so this round trip cannot
	// fail.
	data, _ := json.Marshal(tables)
	var e Extract
	_ = json.Unmarshal(data, &e)
	e.VendorsWithoutPartner = unlinked
	// Supplier-level tables are fetched unfiltered; narrow them to the scope.
	return FilterScope(&e, scope), nil
}

// PartnerLink is the extraction's table of business partner -> supplier
// links (PARTNER, LIFNR). It is no SAP table and no table of the Extract:
// attachBanks reads it to give each bank account its vendor.
const PartnerLink = "partner supplier link"

// attachBanks gives each bank account (LFBK, read by business partner) the
// supplier its partner is, through the extract's own partner link rather
// than by assuming the two numbers equal (review:T72d). An account whose
// partner is no supplier is dropped: it is a customer's or another
// partner's, never a vendor's. It returns the vendors (LFA1) that no
// partner links to, whose bank accounts the extract therefore cannot tell;
// the quality gate warns of each (ADR-0034 As built).
func attachBanks(tables map[string][]map[string]string) []string {
	supplier := map[string]string{}
	linked := map[string]bool{}
	for _, l := range tables[PartnerLink] {
		if l["LIFNR"] != "" {
			supplier[l["PARTNER"]] = l["LIFNR"]
			linked[l["LIFNR"]] = true
		}
	}
	delete(tables, PartnerLink)
	var banks []map[string]string
	for _, b := range tables["LFBK"] {
		lifnr, ok := supplier[b["PARTNER"]]
		if !ok {
			continue
		}
		b["LIFNR"] = lifnr
		delete(b, "PARTNER")
		banks = append(banks, b)
	}
	tables["LFBK"] = banks
	var unlinked []string
	for _, v := range tables["LFA1"] {
		if !linked[v["LIFNR"]] {
			unlinked = append(unlinked, v["LIFNR"])
		}
	}
	return unlinked
}

func companyFilter(em EntityMapping, scope Scope) []string {
	if em.CompanyField == "" {
		return []string{""}
	}
	return scope.CompanyCodes
}

func (o *ODataSource) fetch(ctx context.Context, em EntityMapping, companyCode, year string) ([]map[string]string, error) {
	pageSize := o.PageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	httpc := o.HTTP
	if httpc == nil {
		httpc = &http.Client{Timeout: 2 * time.Minute}
	}
	var filters []string
	if companyCode != "" {
		filters = append(filters, em.CompanyField+" eq "+odataString(companyCode))
	}
	if em.YearField != "" && year != "" {
		filters = append(filters, em.YearField+" eq "+odataString(year))
	}
	selectFields := make([]string, 0, len(em.Fields))
	for _, prop := range em.Fields {
		selectFields = append(selectFields, prop)
	}

	var out []map[string]string
	for skip := 0; ; skip += pageSize {
		q := url.Values{}
		q.Set("$format", "json")
		q.Set("$top", strconv.Itoa(pageSize))
		q.Set("$skip", strconv.Itoa(skip))
		q.Set("$select", strings.Join(selectFields, ","))
		if len(filters) > 0 {
			q.Set("$filter", strings.Join(filters, " and "))
		}
		if o.Client != "" {
			q.Set("sap-client", o.Client)
		}
		u := fmt.Sprintf("%s/sap/opu/odata/sap/%s/%s?%s", strings.TrimRight(o.BaseURL, "/"), em.Service, em.Entity, q.Encode())
		results, err := o.page(ctx, httpc, u, skip)
		if err != nil {
			return nil, err
		}
		out = append(out, odataRows(em, results)...)
		if len(results) < pageSize {
			return out, nil
		}
	}
}

// odataString is s as an OData string literal: quoted, with each single
// quote doubled (OData v2 and v4), so a value cannot end the literal early
// and change the filter it sits in.
func odataString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// page reads one page of an entity set.
func (o *ODataSource) page(ctx context.Context, httpc *http.Client, u string, skip int) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for page at offset %d: %w", skip, err)
	}
	req.SetBasicAuth(o.User, o.Password)
	req.Header.Set("Accept", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request page at offset %d: %w", skip, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close() // the body has been read; a close error cannot change what it held
	if err != nil {
		return nil, fmt.Errorf("read page at offset %d: %w", skip, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %.300s", resp.StatusCode, body)
	}
	var page struct {
		D struct {
			Results []map[string]any `json:"results"`
		} `json:"d"`
	}
	// Numbers stay as their text (json.Number): decoded as float64, an
	// amount past 15-17 significant digits would be rounded (ADR-0005).
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&page); err != nil {
		return nil, fmt.Errorf("decode page: %w", err)
	}
	return page.D.Results, nil
}

// odataRows maps a page's records to SAP table rows: the mapping's static
// fields, then each SAP field from its OData property.
func odataRows(em EntityMapping, results []map[string]any) []map[string]string {
	var out []map[string]string
	for _, rec := range results {
		row := map[string]string{}
		for k, v := range em.Static {
			row[k] = v
		}
		for sapField, prop := range em.Fields {
			row[sapField] = odataValue(rec[prop])
		}
		out = append(out, row)
	}
	return out
}

// odataValue normalizes OData V2 JSON values to SAP table conventions:
// booleans become "X"/"", /Date(ms)/ becomes YYYYMMDD, numbers stay decimal strings.
func odataValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "X"
		}
		return ""
	case json.Number:
		return decimalText(t)
	case string:
		if strings.HasPrefix(t, "/Date(") && strings.HasSuffix(t, ")/") {
			msStr := strings.TrimSuffix(strings.TrimPrefix(t, "/Date("), ")/")
			if i := strings.IndexAny(msStr, "+-"); i > 0 {
				msStr = msStr[:i]
			}
			if ms, err := strconv.ParseInt(msStr, 10, 64); err == nil {
				return time.UnixMilli(ms).UTC().Format("20060102")
			}
		}
		return t
	default:
		return fmt.Sprint(t)
	}
}

// maxSpelledExp bounds the binary exponent decimalText spells out (about
// 1,200 decimal digits), so an absurd number cannot become a huge string.
const maxSpelledExp = 4096

// maxNumberLen bounds the literal decimalText spells out. Spelling costs time
// growing with the square of the precision, which grows with the length, and
// the page comes from another system; no amount comes near 64 characters.
const maxNumberLen = 64

// decimalText is a JSON number as a plain decimal ("1.5e3" is "1500") with
// every digit kept: the precision grows with the literal's length, so the
// shortest decimal that reads back the same is the number itself. A number
// too long, too large or too small to spell out keeps its literal; that
// includes one big.Float reads as infinite, or as zero when it is not.
func decimalText(n json.Number) string {
	s := n.String()
	if len(s) > maxNumberLen {
		return s
	}
	f, _, err := big.ParseFloat(s, 10, 64+4*uint(len(s)), big.ToNearestEven)
	if err != nil || f.IsInf() || (f.Sign() == 0 && !zeroLiteral(s)) || f.MantExp(nil) > maxSpelledExp || f.MantExp(nil) < -maxSpelledExp {
		return s
	}
	return f.Text('f', -1)
}

// zeroLiteral reports whether a JSON number's digits before its exponent
// are all zeros ("0", "-0.00", "0e5").
func zeroLiteral(s string) bool {
	mantissa, _, _ := strings.Cut(strings.ToLower(s), "e")
	return strings.Trim(mantissa, "-+0.") == ""
}
