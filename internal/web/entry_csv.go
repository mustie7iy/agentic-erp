package web

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/mustie7iy/agentic-erp/internal/web/i18n"
	"github.com/mustie7iy/agentic-erp/internal/web/ui"
)

// A CSV file of an entry's lines (T73h): UTF-8, a byte order mark
// allowed, comma- or semicolon-separated (a spreadsheet saved in a
// locale whose decimal sign is the comma separates with semicolons), with
// a header row naming its columns in any order: account (a number of the
// company's chart) and amount (signed, debit positive, in the person's
// locale as typed in the form) required, cost_center, profit_center,
// segment, partner, tax_code and line_text optional; another column is
// refused. A row whose fields are all empty is left out. Every row is
// checked as a typed line is, and none is sent while any is wrong.

// The file's columns, by the line's field each fills.
var csvColumns = map[string]string{"account": ui.FieldLineAccount, "amount": ui.FieldLineAmount, "cost_center": ui.FieldLineCostCenter,
	"profit_center": ui.FieldLineProfit, "segment": ui.FieldLineSegment, "partner": ui.FieldLinePartner, "tax_code": ui.FieldLineTaxCode,
	"line_text": ui.FieldLineText}

// maxFileLines is the most lines a file holds: an entry's most
// (JournalEntryCreate.lines).
const maxFileLines = 10000

// maxRowErrors is how many of a file's wrong rows the page lists; more
// are counted.
const maxRowErrors = 20

// csvLines reads a file's lines, each numbered by its row in the file
// (the header is row 1), or why the file is not one: not UTF-8, no
// header, a column unknown or missing, too many rows.
func csvLines(data []byte) ([]entryLine, i18n.Key) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, i18n.CSVNotUTF8
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma, r.FieldsPerRecord, r.TrimLeadingSpace = delimiter(data), -1, true
	header, err := r.Read()
	if err != nil {
		return nil, i18n.CSVNoHeader
	}
	columns, bad := csvHeader(header)
	if bad != "" {
		return nil, bad
	}
	var lines []entryLine
	for row := 2; ; row++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, i18n.CSVUnreadable
		}
		if len(rec) > len(columns) {
			return nil, i18n.CSVExtraFields
		}
		l := entryLine{no: row}
		for i, v := range rec {
			setField(&l, columns[i], v)
		}
		if l.blank() {
			continue
		}
		if lines = append(lines, l); len(lines) > maxFileLines {
			return nil, i18n.CSVTooManyRows
		}
	}
	switch len(lines) {
	case 0:
		return nil, i18n.CSVNoRows
	case 1:
		return nil, i18n.EntryLinesTooFew
	}
	return lines, ""
}

// delimiter is the file's separator: a semicolon when its first line
// holds one and no comma, else a comma.
func delimiter(data []byte) rune {
	first, _, _ := bytes.Cut(data, []byte("\n"))
	if bytes.ContainsRune(first, ';') && !bytes.ContainsRune(first, ',') {
		return ';'
	}
	return ','
}

// csvHeader is the field each column fills, or what is wrong with the
// header: a column unknown or named twice, or account or amount missing.
func csvHeader(header []string) ([]string, i18n.Key) {
	out := make([]string, len(header))
	seen := map[string]bool{}
	for i, h := range header {
		field, ok := csvColumns[strings.ToLower(strings.TrimSpace(h))]
		if !ok || seen[field] {
			return nil, i18n.CSVColumnUnknown
		}
		out[i], seen[field] = field, true
	}
	if !seen[ui.FieldLineAccount] || !seen[ui.FieldLineAmount] {
		return nil, i18n.CSVColumnMissing
	}
	return out, ""
}

// setField sets a line's field from the form's field or a file's column.
func setField(l *entryLine, field, v string) {
	switch field {
	case ui.FieldLineAccount:
		l.account = v
	case ui.FieldLineAmount:
		l.amount = v
	case ui.FieldLineCostCenter:
		l.costCenter = v
	case ui.FieldLineProfit:
		l.profitCenter = v
	case ui.FieldLineSegment:
		l.segment = v
	case ui.FieldLinePartner:
		l.partner = v
	case ui.FieldLineTaxCode:
		l.taxCode = v
	default: // ui.FieldLineText: the columns are csvColumns'
		l.text = v
	}
}

// rowErrors are the file's wrong rows as the page lists them: the first
// maxRowErrors errors, and how many rows have errors not listed.
func rowErrors(lines []entryLine) ([]ui.RowError, int) {
	var out []ui.RowError
	more := 0
	for _, l := range lines {
		var row []ui.RowError
		for _, f := range csvOrder {
			if k := l.errs[f.field]; k != "" {
				row = append(row, ui.RowError{Row: l.no, Column: f.column, Error: k})
			}
		}
		for _, rf := range l.refused {
			row = append(row, ui.RowError{Row: l.no, Column: apiColumns[rf.api], Error: i18n.EntryFieldRefused, Reason: rf.reason})
		}
		listed := min(len(row), maxRowErrors-len(out))
		out = append(out, row[:listed]...)
		if listed < len(row) {
			more++
		}
	}
	return out, more
}

// apiColumns are the file's columns by the API's names of the line's
// fields, which its field errors name.
var apiColumns = map[string]string{"account_id": "account", "amount": "amount", "cost_center_id": "cost_center", "profit_center_id": "profit_center",
	"segment_id": "segment", "partner_id": "partner", "tax_code": "tax_code", "line_text": "line_text"}

// csvOrder are the file's columns in the order a row's errors are said.
var csvOrder = []struct{ field, column string }{
	{ui.FieldLineAccount, "account"}, {ui.FieldLineAmount, "amount"}, {ui.FieldLineCostCenter, "cost_center"},
	{ui.FieldLineProfit, "profit_center"}, {ui.FieldLineSegment, "segment"}, {ui.FieldLinePartner, "partner"},
	{ui.FieldLineTaxCode, "tax_code"}, {ui.FieldLineText, "line_text"},
}
