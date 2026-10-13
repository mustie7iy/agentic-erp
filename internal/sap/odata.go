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
	{Table: "LFB1", Service: "API_BUSINESS_PARTNER", Entity: "A_SupplierCompany", CompanyField: "CompanyCode",
		Fields: map[string]string{"LIFNR": "Supplier", "BUKRS": "CompanyCode", "AKONT": "ReconciliationAccount",
			"ZTERM": "PaymentTerms"}},
	{Table: "LFBK", Service: "API_BUSINESS_PARTNER", Entity: "A_BusinessPartnerBank",
		Fields: map[string]string{"LIFNR": "BusinessPartner", "BANKS": "BankCountryKey", "BANKN": "BankAccount"}},
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
	{Table: "RBKP", Service: "API_SUPPLIERINVOICE_PROCESS_SRV", Entity: "A_SupplierInvoice", CompanyField: "CompanyCode", YearField: "FiscalYear",
		Fields: map[string]string{"BELNR": "SupplierInvoice", "GJAHR": "FiscalYear", "BUKRS": "CompanyCode",
			"LIFNR": "InvoicingParty", "XBLNR": "SupplierInvoiceIDByInvcgParty", "BLDAT": "DocumentDate",
			"WAERS": "DocumentCurrency", "RMWWR": "InvoiceGrossAmount", "WMWST1": "TaxAmount", "RBSTAT": "SupplierInvoiceStatus"}},
	{Table: "RSEG", Service: "API_SUPPLIERINVOICE_PROCESS_SRV", Entity: "A_SuplrInvcItemPurOrdRef", YearField: "FiscalYear",
		Fields: map[string]string{"BELNR": "SupplierInvoice", "GJAHR": "FiscalYear", "BUZEI": "SupplierInvoiceItem",
			"EBELN": "PurchaseOrder", "EBELP": "PurchaseOrderItem", "MENGE": "QuantityInPurchaseOrderUnit",
			"WRBTR": "SupplierInvoiceItemAmount"}},
	{Table: "ACDOCA", Service: "API_JOURNALENTRYITEMBASIC_SRV", Entity: "A_JournalEntryItemBasic", CompanyField: "CompanyCode", YearField: "FiscalYear",
		Static: map[string]string{"RLDNR": "0L"},
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
	// Assemble via JSON so each SAP field lands in its struct field by tag.
	// Every row is a map of strings and every Extract field is a slice of
	// string-only structs, so this round trip cannot fail.
	data, _ := json.Marshal(tables)
	var e Extract
	_ = json.Unmarshal(data, &e)
	// Supplier-level tables are fetched unfiltered; narrow them to the scope.
	return FilterScope(&e, scope), nil
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

// decimalText is a JSON number as a plain decimal ("1.5e3" is "1500") with
// every digit kept: the precision grows with the literal's length, so the
// shortest decimal that reads back the same is the number itself. A number
// too large or too small to spell out keeps its literal.
func decimalText(n json.Number) string {
	s := n.String()
	f, _, err := big.ParseFloat(s, 10, 64+4*uint(len(s)), big.ToNearestEven)
	if err != nil || f.MantExp(nil) > maxSpelledExp || f.MantExp(nil) < -maxSpelledExp {
		return s
	}
	return f.Text('f', -1)
}
