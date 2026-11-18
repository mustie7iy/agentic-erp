// Package csvcell writes CSV for a spreadsheet (ADR-0052 Built in T73j4;
// ADR-0063 As built T75m): cells a spreadsheet program reads as text, not
// as formulas, as OWASP's CSV injection page advises (fetched 2026-11-02),
// and UTF-8 with a byte order mark. The workspace's report downloads and
// the API's CSV of the AR aging share it.
package csvcell

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ByteOrderMark is UTF-8's, a file's first bytes: spreadsheet programs
// need it to read the accents of a French company's names (from memory,
// not verified: ADR-0052).
const ByteOrderMark = "\xef\xbb\xbf"

// numeric is a cell a spreadsheet reads as a number or a date, as it is:
// an amount as the API writes it, or an ISO date.
var numeric = regexp.MustCompile(`^(-?[0-9]+(\.[0-9]+)?|[0-9]{4}-[0-9]{2}-[0-9]{2})$`)

// formulaStarts are the first characters OWASP's CSV injection page names
// (fetched 2026-11-02, ADR-0052 Built in T73j4): a spreadsheet may read a
// cell beginning with one of them as a formula. The full-width forms are
// U+FF1D, U+FF0B, U+FF0D and U+FF20.
const formulaStarts = "=+-@\t\r\n＝＋－＠"

// Safe is a cell as the file holds it, safe for a spreadsheet, as
// OWASP's CSV injection page advises: numbers and dates as they are, so
// a spreadsheet reads them as numbers; every other text (an account's
// name, a reference a person or an agent typed) quoted, its quotes
// doubled, so a separator (a comma or a semicolon, both of which OWASP
// names), a quote or a line break inside it starts no new cell;
// and a text that begins as a formula does kept as text by a leading
// apostrophe. OWASP says no strategy is safe for every spreadsheet and
// every program that reads the file (ADR-0052).
func Safe(v string) string {
	if v == "" || numeric.MatchString(v) {
		return v
	}
	if first, _ := utf8.DecodeRuneInString(v); strings.ContainsRune(formulaStarts, first) {
		v = "'" + v
	}
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// Write writes the byte order mark and the rows, each cell Safe, the
// cells separated by commas and the lines ended by a line feed, and
// returns the first error the writer gave.
func Write(w io.Writer, rows [][]string) error {
	// A bufio.Writer keeps its first error, which every later write
	// returns again and Flush reports: the writes' own are dropped.
	bw := bufio.NewWriter(w)
	_, _ = bw.WriteString(ByteOrderMark)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				_ = bw.WriteByte(',')
			}
			_, _ = bw.WriteString(Safe(cell))
		}
		_ = bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("csvcell: write the rows: %w", err)
	}
	return nil
}
