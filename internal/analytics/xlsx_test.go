package analytics

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// workbookResult has every kind of cell: text a spreadsheet would take for
// a formula, control characters, text shaped like an escape, amounts of
// three exponents (one below one minor unit's tenth), a ratio, a count and
// a measure that cannot be computed.
func workbookResult() *Result {
	return &Result{Columns: []Column{{Name: "customer", Kind: "dimension"}, {Name: "revenue", Kind: "measure", Type: Money},
		{Name: "margin", Kind: "measure", Type: Ratio}, {Name: "invoices", Kind: "measure", Type: Count}},
		Rows: []Row{
			{"=HYPERLINK(\"http://x\")", amt("BHD", "-0.005"), ratio("0.1234"), count(3)},
			{"+1 & <2>", amt("JPY", "5000"), nil, count(0)},
			{"-2\tA\nB\rC", usd("1234567.89"), ratio("-1.0000"), count(12)},
			{"@x _x0041_ \x08 ￾", amt("BHD", "1234.567"), ratio("0.0000"), nil},
			{"", nil, nil, count(1)},
		}}
}

// TestWorkbookReadsBackAsTheCSV (T95d): the workbook's one sheet, read back
// as a spreadsheet reads it, holds the CSV of the same result cell for
// cell; numbers are number cells, text inline strings and nothing a formula.
func TestWorkbookReadsBackAsTheCSV(t *testing.T) {
	r := workbookResult()
	var b bytes.Buffer
	if err := WriteXLSX(&b, SheetQuery, r); err != nil {
		t.Fatal(err)
	}
	var csvText bytes.Buffer
	if err := csv.NewWriter(&csvText).WriteAll(Records(r)); err != nil {
		t.Fatal(err)
	}
	want, err := csv.NewReader(&csvText).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	got, cells := readSheet(t, b.Bytes())
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Errorf("sheet:\ngot  %q\nwant %q", got, want)
	}
	if want := "customer revenue revenue_currency margin invoices"; strings.Join(got[0], " ") != want {
		t.Errorf("header: got %v, want %s", got[0], want)
	}
	for _, c := range cells {
		if c.F != nil {
			t.Errorf("cell %s: got a formula %q, want none", c.R, *c.F)
		}
		number := c.T == ""
		col, row := strings.TrimRight(c.R, "0123456789"), strings.TrimLeft(c.R, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
		wantNumber := row != "1" && (col == "B" || col == "D" || col == "E")
		if number != wantNumber || (!number && c.T != "inlineStr") {
			t.Errorf("cell %s: got type %q, want a number in B, D and E below the header and an inline string elsewhere", c.R, c.T)
		}
	}
}

// TestWorkbookNumbers (T95d): each number is written as its exact decimal,
// never through a float, in the standard's lexical form, and shown with
// its scale's digits through the style its scale has.
func TestWorkbookNumbers(t *testing.T) {
	var b bytes.Buffer
	if err := WriteXLSX(&b, SheetQuery, workbookResult()); err != nil {
		t.Fatal(err)
	}
	parts := unzip(t, b.Bytes())
	sheet, styles := parts["xl/worksheets/sheet1.xml"], parts["xl/styles.xml"]
	for name, want := range map[string]string{
		"BHD below a tenth of its minor unit": `<c r="B2" s="3"><v>-0.005</v></c>`,
		"JPY without decimals":                `<c r="B3" s="1"><v>5000</v></c>`,
		"USD, with no thousands separator":    `<c r="B4" s="2"><v>1234567.89</v></c>`,
		"a ratio to four digits":              `<c r="D2" s="4"><v>0.1234</v></c>`,
		"a count":                             `<c r="E2" s="1"><v>3</v></c>`,
		"a measure not computed is no cell":   `<row r="3"><c r="A3" t="inlineStr"><is><t xml:space="preserve">+1 &amp; &lt;2&gt;</t></is></c><c r="B3" s="1"><v>5000</v></c><c r="C3" t="inlineStr"><is><t xml:space="preserve">JPY</t></is></c><c r="E3" s="1"><v>0</v></c></row>`,
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(sheet, want) {
				t.Errorf("sheet: got %s, want %s", sheet, want)
			}
		})
	}
	lexical := regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`) // P1 §18.17.5.3, without exponent or "+"
	for _, v := range regexp.MustCompile(`<v>([^<]*)</v>`).FindAllStringSubmatch(sheet, -1) {
		if !lexical.MatchString(v[1]) || v[1] == "-0" {
			t.Errorf("number %q: not a plain decimal", v[1])
		}
	}
	// Scales 0 and 2 are the built-in #,##0 and #,##0.00; 3 and 4 are
	// custom formats from 164, in the order of their scales.
	for _, want := range []string{`<numFmts count="2"><numFmt numFmtId="164" formatCode="#,##0.000"/><numFmt numFmtId="165" formatCode="#,##0.0000"/></numFmts>`,
		`<cellXfs count="5"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="3" `, `<xf numFmtId="4" `, `<xf numFmtId="164" `, `<xf numFmtId="165" `} {
		if !strings.Contains(styles, want) {
			t.Errorf("styles: got %s, want %s", styles, want)
		}
	}
}

// TestWorkbookText (T95d): text is written as the standard's ST_Xstring:
// a character XML cannot carry and a carriage return as _xHHHH_, text
// shaped like an escape with its underscore escaped, the rest as it is.
func TestWorkbookText(t *testing.T) {
	for in, want := range map[string]string{
		"plain":          "plain",
		"=1+1":           "=1+1",
		"a & <b>":        "a &amp; &lt;b&gt;",
		"bell\x07":       "bell_x0007_",
		"cr\r":           "cr_x000D_",
		"tab\tlf\n":      "tab&#x9;lf&#xA;",
		"_x0041_":        "_x005F_x0041_",
		"_x00G1_ _x41_":  "_x00G1_ _x41_",
		"￾￿":             "_xFFFE__xFFFF_",
		"bad \xff utf-8": "bad � utf-8",
		"😀 é":            "😀 é",
		`"quoted" 'x'`:   "&#34;quoted&#34; &#39;x&#39;",
	} {
		t.Run(in, func(t *testing.T) {
			if got := escape(in); got != want {
				t.Errorf("escape(%q): got %q, want %q", in, got, want)
			}
		})
	}
}

// TestWorkbookPackage (T95d): the package holds the parts, content types
// and relationships of docs/design/specs/xlsx-minimal.md §2-§4, deflated,
// and its sheet is named as asked.
func TestWorkbookPackage(t *testing.T) {
	var b bytes.Buffer
	if err := WriteXLSX(&b, SheetReport, &Result{Columns: []Column{{Name: "company", Kind: "dimension"}}}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
		if f.Method != zip.Deflate {
			t.Errorf("%s: method %d, want deflate", f.Name, f.Method)
		}
	}
	if got, want := strings.Join(names, " "), "[Content_Types].xml _rels/.rels xl/workbook.xml xl/_rels/workbook.xml.rels xl/worksheets/sheet1.xml xl/styles.xml"; got != want {
		t.Errorf("parts: got %s, want %s", got, want)
	}
	parts := unzip(t, b.Bytes())
	for part, want := range map[string]string{
		"[Content_Types].xml":        `<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`,
		"_rels/.rels":                `Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"`,
		"xl/workbook.xml":            `<bookViews><workbookView/></bookViews><sheets><sheet name="Report" sheetId="1" r:id="rId1"/></sheets>`,
		"xl/_rels/workbook.xml.rels": `Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"`,
		"xl/worksheets/sheet1.xml":   `<sheetData><row r="1"><c r="A1" t="inlineStr"><is><t xml:space="preserve">company</t></is></c></row></sheetData>`,
		"xl/styles.xml":              `<cellXfs count="1">`,
	} {
		t.Run(part, func(t *testing.T) {
			if !strings.HasPrefix(parts[part], `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`) {
				t.Errorf("%s: got %.60s, want the XML declaration first", part, parts[part])
			}
			if !strings.Contains(parts[part], want) {
				t.Errorf("%s: got %s, want %s", part, parts[part], want)
			}
			var doc struct{}
			if err := xml.Unmarshal([]byte(parts[part]), &doc); err != nil {
				t.Errorf("%s: not well-formed XML: %v", part, err)
			}
		})
	}
	if strings.Contains(parts["xl/styles.xml"], "numFmts") {
		t.Errorf("styles without numbers: got %s, want no custom formats", parts["xl/styles.xml"])
	}
}

// TestWorkbookColumns: columns are lettered in bijective base 26 (P1
// §18.17.2.3.1), up to Excel's last, XFD.
func TestWorkbookColumns(t *testing.T) {
	for n, want := range map[int]string{1: "A", 26: "Z", 27: "AA", 52: "AZ", 53: "BA", 702: "ZZ", 703: "AAA", 16384: "XFD"} {
		if got := column(n); got != want {
			t.Errorf("column(%d): got %s, want %s", n, got, want)
		}
	}
}

// TestWorkbookWriteFailures: a failed write is returned, whether it shows
// while a part is written or only when the package is closed.
func TestWorkbookWriteFailures(t *testing.T) {
	big := &Result{Columns: []Column{{Name: "customer", Kind: "dimension"}}}
	for i := range 500 {
		big.Rows = append(big.Rows, Row{strings.Repeat(strconv.Itoa(i), 40)})
	}
	for name, r := range map[string]*Result{"while writing a part": big, "on closing": {Columns: []Column{{Name: "c", Kind: "dimension"}}}} {
		t.Run(name, func(t *testing.T) {
			if err := WriteXLSX(failingWriter{}, SheetQuery, r); !errors.Is(err, errWrite) {
				t.Errorf("got %v, want the write's error", err)
			}
		})
	}
}

var errWrite = errors.New("disk full")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

// sheetCell is a cell as the sheet part holds it.
type sheetCell struct {
	R  string  `xml:"r,attr"`
	T  string  `xml:"t,attr"`
	S  string  `xml:"s,attr"`
	F  *string `xml:"f"`
	V  string  `xml:"v"`
	Is struct {
		T string `xml:"t"`
	} `xml:"is"`
}

// readSheet reads the workbook's sheet back as a spreadsheet does: each
// row's cells placed by their references, text unescaped from ST_Xstring
// (xlsx-minimal §7), a missing cell empty.
func readSheet(t *testing.T, b []byte) ([][]string, []sheetCell) {
	t.Helper()
	var sheet struct {
		Rows []struct {
			R     string      `xml:"r,attr"`
			Cells []sheetCell `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal([]byte(unzip(t, b)["xl/worksheets/sheet1.xml"]), &sheet); err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var all []sheetCell
	width := 0
	for i, row := range sheet.Rows {
		if row.R != strconv.Itoa(i+1) {
			t.Fatalf("row %d: got r=%s, want %d", i+1, row.R, i+1)
		}
		var rec []string
		for _, c := range row.Cells {
			col := strings.TrimSuffix(c.R, row.R)
			for column(len(rec)+1) != col {
				rec = append(rec, "")
			}
			text := c.V
			if c.T == "inlineStr" {
				text = unescapeXstring(c.Is.T)
			}
			rec = append(rec, text)
			all = append(all, c)
		}
		width = max(width, len(rec))
		out = append(out, rec)
	}
	for i := range out {
		for len(out[i]) < width {
			out[i] = append(out[i], "")
		}
	}
	return out, all
}

// unescapeXstring reverses ST_Xstring's escapes (P1 §22.9.2.19).
func unescapeXstring(s string) string {
	return regexp.MustCompile(`_x([0-9A-Fa-f]{4})_`).ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseUint(m[2:6], 16, 32) // four hex digits, matched
		return string(rune(n))
	})
}

// unzip is the package's parts by name.
func unzip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		part, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(part)
	}
	return out
}
