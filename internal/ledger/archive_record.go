package ledger

import (
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/currency"
)

// EntryRecord is a journal entry as a line of a file that hands entries
// out whole: the records context's archive files (ADR-0079 §7) and the
// audit context's evidence exports (T101a). JSON, its amounts decimal
// strings in their currency's digits, a debit positive and a credit
// negative.
type EntryRecord struct {
	EntryID          uuid.UUID    `json:"entry_id"`
	Ledger           string       `json:"ledger"`
	DocumentType     string       `json:"document_type"`
	DocumentNumber   string       `json:"document_number"`
	FiscalYear       int16        `json:"fiscal_year"`
	FiscalPeriod     int16        `json:"fiscal_period"`
	DocumentDate     string       `json:"document_date"`
	PostingDate      string       `json:"posting_date"`
	Currency         string       `json:"currency"`
	LocalCurrency    string       `json:"local_currency"`
	GroupCurrency    string       `json:"group_currency"`
	RateToLocal      string       `json:"rate_to_local"`
	RateToGroup      string       `json:"rate_to_group"`
	Reference        *string      `json:"reference"`
	HeaderText       *string      `json:"header_text"`
	SourceContext    string       `json:"source_context"`
	SourceDocumentID *uuid.UUID   `json:"source_document_id"`
	ReversalOfID     *uuid.UUID   `json:"reversal_of_id"`
	ReversalOfYear   *int16       `json:"reversal_of_year"`
	PostedAt         string       `json:"posted_at"`
	PostedBy         uuid.UUID    `json:"posted_by"`
	Lines            []LineRecord `json:"lines"`
}

// LineRecord is a line of an entry's record.
type LineRecord struct {
	LineNo      int32      `json:"line_no"`
	Account     string     `json:"account"`
	Amount      string     `json:"amount"`
	AmountLocal string     `json:"amount_local"`
	AmountGroup string     `json:"amount_group"`
	PartnerID   *uuid.UUID `json:"partner_id"`
	TaxCode     *string    `json:"tax_code"`
	Quantity    *string    `json:"quantity"`
	Unit        *string    `json:"unit"`
	Text        *string    `json:"text"`
}

// Record is the entry in a file's form, its amounts in the digits exps
// gives their currencies.
func (x ArchivedEntry) Record(exps currency.Exponents) EntryRecord {
	out := EntryRecord{EntryID: x.ID, Ledger: x.Ledger, DocumentType: x.DocumentType, DocumentNumber: x.DocumentNumber, FiscalYear: x.FiscalYear,
		FiscalPeriod: x.FiscalPeriod, DocumentDate: x.DocumentDate.Format(time.DateOnly), PostingDate: x.PostingDate.Format(time.DateOnly),
		Currency: x.Currency, LocalCurrency: x.LocalCurrency, GroupCurrency: x.GroupCurrency, RateToLocal: x.RateToLocal, RateToGroup: x.RateToGroup,
		Reference: x.Reference, HeaderText: x.HeaderText, SourceContext: x.SourceContext, SourceDocumentID: x.SourceDocumentID,
		ReversalOfID: x.ReversalOfID, ReversalOfYear: x.ReversalOfYear, PostedAt: x.PostedAt.UTC().Format(time.RFC3339Nano), PostedBy: x.PostedBy,
		Lines: make([]LineRecord, len(x.Lines))}
	for i, l := range x.Lines {
		out.Lines[i] = LineRecord{LineNo: l.LineNo, Account: l.Account, Amount: l.Amount.Plain(exps[x.Currency]), AmountLocal: l.AmountLocal.Plain(exps[x.LocalCurrency]),
			AmountGroup: l.AmountGroup.Plain(exps[x.GroupCurrency]), PartnerID: l.PartnerID, TaxCode: l.TaxCode, Quantity: l.Quantity, Unit: l.Unit, Text: l.Text}
	}
	return out
}
