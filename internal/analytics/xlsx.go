package analytics

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// XLSXMediaType is an Excel workbook's media type (ECMA-376 Part 1
// §12.3.23's workbook in its package).
const XLSXMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// The workbook's sheet names: fixed, so none needs the sheet-name rules
// of docs/design/specs/xlsx-minimal.md §10 applied to user text.
const (
	SheetQuery  = "Query"
	SheetReport = "Report"
)

// WriteXLSX writes the result as a one-sheet Excel workbook (T95d,
// ADR-0076 §8), the minimal SpreadsheetML package of
// docs/design/specs/xlsx-minimal.md: a header row of the columns' names,
// a money measure followed by its currency's column (<name>_currency) as
// the report run's CSV has it, then a row per result row. A measure is a
// number cell whose value is its exact decimal, written from its units
// and scale with no float in between, styled to show its scale's digits;
// a measure that cannot be computed is an empty cell. Every other cell is
// an inline string, which can never be a formula (§8): the writer emits
// no f element.
func WriteXLSX(w io.Writer, sheet string, r *Result) error {
	head, rows := table(r)
	styles := newStyles(rows)
	z := zip.NewWriter(w)
	parts := []struct {
		name string
		body []byte
	}{
		{"[Content_Types].xml", []byte(contentTypes)},
		{"_rels/.rels", []byte(packageRels)},
		{"xl/workbook.xml", []byte(xmlDecl + `<workbook xmlns="` + nsMain + `" xmlns:r="` + nsRel + `"><bookViews><workbookView/></bookViews><sheets><sheet name="` +
			escape(sheet) + `" sheetId="1" r:id="rId1"/></sheets></workbook>`)},
		{"xl/_rels/workbook.xml.rels", []byte(workbookRels)},
		{"xl/worksheets/sheet1.xml", worksheet(head, rows, styles)},
		{"xl/styles.xml", styles.xml()},
	}
	for _, p := range parts {
		f, err := z.Create(p.name)
		if err == nil {
			_, err = f.Write(p.body)
		}
		if err != nil {
			return fmt.Errorf("analytics: write workbook part %s: %w", p.name, err)
		}
	}
	if err := z.Close(); err != nil {
		return fmt.Errorf("analytics: close workbook: %w", err)
	}
	return nil
}

// Namespaces and relationship types of the Transitional conformance class
// (xlsx-minimal §2, §3).
const (
	nsMain = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsRel  = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	nsPkg  = "http://schemas.openxmlformats.org/package/2006/relationships"
)

// xmlDecl is the declaration every part starts with (xlsx-minimal §4).
const xmlDecl = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

const contentTypes = xmlDecl + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
	`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
	`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
	`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
	`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`

const packageRels = xmlDecl + `<Relationships xmlns="` + nsPkg + `">` +
	`<Relationship Id="rId1" Type="` + nsRel + `/officeDocument" Target="xl/workbook.xml"/></Relationships>`

const workbookRels = xmlDecl + `<Relationships xmlns="` + nsPkg + `">` +
	`<Relationship Id="rId1" Type="` + nsRel + `/worksheet" Target="worksheets/sheet1.xml"/>` +
	`<Relationship Id="rId2" Type="` + nsRel + `/styles" Target="styles.xml"/></Relationships>`

// xcell is a cell of the sheet: text, or a number (num) whose text is its
// exact decimal at scale digits. An empty text cell is not written.
type xcell struct {
	text  string
	num   bool
	scale int
}

// table lays the result out as the report run's CSV does: each column,
// a money measure followed by its currency's column.
func table(r *Result) ([]string, [][]xcell) {
	var head []string
	for _, c := range r.Columns {
		head = append(head, c.Name)
		if c.Type == Money {
			head = append(head, c.Name+"_currency")
		}
	}
	rows := make([][]xcell, 0, len(r.Rows))
	for _, row := range r.Rows {
		var cells []xcell
		for i, c := range r.Columns {
			var cur string
			switch v := row[i].(type) {
			case string:
				cells = append(cells, xcell{text: v})
			case Value:
				cells, cur = append(cells, xcell{text: v.String(), num: true, scale: v.Scale}), v.Currency
			default: // nil: a measure that cannot be computed
				cells = append(cells, xcell{})
			}
			if c.Type == Money {
				cells = append(cells, xcell{text: cur})
			}
		}
		rows = append(rows, cells)
	}
	return head, rows
}

// Records is the result as rows of text, the header first, laid out as
// the workbook lays it out: the report run's CSV (getReportRun,
// format=csv) writes them.
func Records(r *Result) [][]string {
	head, rows := table(r)
	out := [][]string{head}
	for _, cells := range rows {
		rec := make([]string, len(cells))
		for i, c := range cells {
			rec[i] = c.text
		}
		out = append(out, rec)
	}
	return out
}

// worksheet is the sheet part: the header row, then the rows, every cell
// with its A1 reference (xlsx-minimal §5).
func worksheet(head []string, rows [][]xcell, st styles) []byte {
	var b bytes.Buffer
	b.WriteString(xmlDecl + `<worksheet xmlns="` + nsMain + `"><sheetData>`)
	header := make([]xcell, len(head))
	for i, h := range head {
		header[i] = xcell{text: h}
	}
	for n, cells := range append([][]xcell{header}, rows...) {
		row := strconv.Itoa(n + 1)
		b.WriteString(`<row r="` + row + `">`)
		for i, c := range cells {
			ref := column(i+1) + row
			switch {
			case c.num:
				b.WriteString(`<c r="` + ref + `" s="` + strconv.Itoa(st.index(c.scale)) + `"><v>` + c.text + `</v></c>`)
			case c.text != "":
				b.WriteString(`<c r="` + ref + `" t="inlineStr"><is><t xml:space="preserve">` + escape(c.text) + `</t></is></c>`)
			}
		}
		b.WriteString(`</row>`)
	}
	b.WriteString(`</sheetData></worksheet>`)
	return b.Bytes()
}

// column is the letters of the 1-based column n in bijective base 26: A,
// …, Z, AA (xlsx-minimal §5, P1 §18.17.2.3.1).
func column(n int) string {
	var s []byte
	for ; n > 0; n = (n - 1) / 26 {
		s = append(s, byte('A'+(n-1)%26))
	}
	slices.Reverse(s)
	return string(s)
}

// styles numbers the cell formats: index 0 is General (text), then one
// per scale the numbers have, in order, each showing that many decimals.
type styles []int

func newStyles(rows [][]xcell) styles {
	var scales styles
	for _, cells := range rows {
		for _, c := range cells {
			if c.num && !slices.Contains(scales, c.scale) {
				scales = append(scales, c.scale)
			}
		}
	}
	slices.Sort(scales)
	return scales
}

// index is the cellXfs index of the scale's format.
func (s styles) index(scale int) int { return slices.Index(s, scale) + 1 }

// builtinFormats are the built-in number formats a scale uses (P1
// §18.8.30): #,##0 and #,##0.00 need no numFmt element.
var builtinFormats = map[int]int{0: 3, 2: 4}

// firstCustomFormat is the first id of a custom number format, as the
// standard's examples number them (xlsx-minimal §9).
const firstCustomFormat = 164

// xml is the styles part (xlsx-minimal §4): every font, fill, border and
// xf an xf refers to exists.
func (s styles) xml() []byte {
	var custom, xfs strings.Builder
	n := 0
	for _, scale := range s {
		id, ok := builtinFormats[scale]
		if !ok {
			id = firstCustomFormat + n
			n++
			fmt.Fprintf(&custom, `<numFmt numFmtId="%d" formatCode="#,##0.%s"/>`, id, strings.Repeat("0", scale))
		}
		fmt.Fprintf(&xfs, `<xf numFmtId="%d" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>`, id)
	}
	var b bytes.Buffer
	b.WriteString(xmlDecl + `<styleSheet xmlns="` + nsMain + `">`)
	if n > 0 {
		fmt.Fprintf(&b, `<numFmts count="%d">%s</numFmts>`, n, custom.String())
	}
	b.WriteString(`<fonts count="1"><font><sz val="11"/><name val="Calibri"/></font></fonts>` +
		`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
		`<borders count="1"><border/></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`)
	fmt.Fprintf(&b, `<cellXfs count="%d"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>%s</cellXfs>`, len(s)+1, xfs.String())
	b.WriteString(`<cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`)
	return b.Bytes()
}

// escape writes text as an ST_Xstring in XML (xlsx-minimal §7): text
// already shaped like an escape keeps its underscore as _x005F_, a
// character XML 1.0 cannot carry and a carriage return become _xHHHH_,
// and only then is the text XML-escaped. In that order: Go's
// encoding/xml would replace a control character with U+FFFD.
func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if looksEscaped(s[i:]) {
			b.WriteString("_x005F_")
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\r' || !xmlChar(r):
			fmt.Fprintf(&b, "_x%04X_", r)
		default: // invalid UTF-8 decodes as U+FFFD, which is written
			b.WriteRune(r)
		}
		i += size
	}
	var out strings.Builder
	// A strings.Builder never fails a write, so neither does EscapeText.
	_ = xml.EscapeText(&out, []byte(b.String()))
	return out.String()
}

// looksEscaped reports whether s starts with _xHHHH_.
func looksEscaped(s string) bool {
	if len(s) < 7 || s[:2] != "_x" || s[6] != '_' {
		return false
	}
	for _, c := range s[2:6] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// xmlChar reports whether r is an XML 1.0 Char (XML §2.2, production
// [2]).
func xmlChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}
