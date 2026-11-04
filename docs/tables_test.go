package docs

// The structure of the design documents' tables, and the data model against
// the migrations (review:batch53). Batch 53's integration dropped five rows of
// 03 §3.21's table in a merge resolution and left two lines of prose and a
// blank line in it, so its last row rendered as a paragraph of pipes; only the
// reviewer saw it. These checks would have refused it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	delimiterCell = regexp.MustCompile(`^:?-+:?$`)
	// blockStart is a line that ends a table without being one of its rows:
	// a heading, list item, block quote or HTML block (GFM: "the table is
	// broken at the first empty line, or beginning of another block-level
	// structure"). Any other text line directly after a row is read as a row
	// of the table, which is the defect this file looks for.
	blockStart = regexp.MustCompile(`^(#{1,6}\s|[-*+]\s|\d+[.)]\s|>|<)`)
)

// cells splits a table row into its cells as GFM does: on every pipe not
// escaped with a backslash (a backslash escapes the byte after it), inside
// code spans too, dropping the empty piece before a leading pipe and after a
// trailing one.
func cells(row string) []string {
	s := strings.TrimSpace(row)
	var out []string
	var cell strings.Builder
	trailing := false
	for i := 0; i < len(s); i++ {
		trailing = false
		switch {
		case s[i] == '\\' && i+1 < len(s):
			cell.WriteString(s[i : i+2])
			i++
		case s[i] == '|':
			out = append(out, strings.TrimSpace(cell.String()))
			cell.Reset()
			trailing = true
		default:
			cell.WriteByte(s[i])
		}
	}
	out = append(out, strings.TrimSpace(cell.String()))
	if len(out) > 1 && strings.HasPrefix(s, "|") {
		out = out[1:]
	}
	if len(out) > 1 && trailing {
		out = out[:len(out)-1]
	}
	return out
}

func isRow(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "|") }

func isDelimiter(line string) bool {
	if !isRow(line) {
		return false
	}
	for _, c := range cells(line) {
		if !delimiterCell.MatchString(c) {
			return false
		}
	}
	return true
}

// fence returns the fence character a line opens or closes a code block
// with, or 0.
func fence(line string) byte {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
		return s[0]
	}
	return 0
}

// tableScan reads a markdown document line by line. A table is a run of
// rows (lines starting with "|") whose first is its header and second its
// delimiter row; it ends at a blank line, a fence or another block.
type tableScan struct {
	lines    []string
	problems []string
	fence    byte
	// header is the line of the table being read (1-based), 0 outside one;
	// cols its column count.
	header, cols int
	// lastHeader is the header text of the last table when only blank lines
	// followed it, and lastLine its line, for the check of a split table.
	lastHeader string
	lastLine   int
}

func (s *tableScan) problem(n int, format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf("line %d: ", n)+fmt.Sprintf(format, args...))
}

// endTable closes the table being read; keep says whether its header stays
// the candidate for the split-table check (only blank lines follow it).
func (s *tableScan) endTable(keep bool) {
	if s.header != 0 && keep {
		s.lastHeader, s.lastLine = strings.TrimSpace(s.lines[s.header-1]), s.header
	} else if !keep {
		s.lastHeader = ""
	}
	s.header, s.cols = 0, 0
}

// tableProblems lists, for one markdown document, each place its tables are
// not well formed:
//
//   - a run of rows that does not open with a header and a delimiter row
//     where a table may start (after a blank line, a heading or a fence):
//     rows cut off from their header, as by a blank line inside a table;
//   - a row whose cell count differs from its header's, or a second
//     delimiter row;
//   - a text line directly after a row that is neither a row nor the start
//     of another block: GFM reads it as a row, so prose has got into the
//     table;
//   - two tables with the same header separated only by blank lines: one
//     table that a blank line split in two.
//
// Fenced code blocks are skipped; indented code blocks are not, so a line of
// one starting with | is read as a row (none does today). A line of prose
// starting with | is reported as rows without a header: wrap the line so
// that it does not (a merge's cut-off rows after prose look the same).
func tableProblems(text string) []string {
	s := &tableScan{lines: strings.Split(text, "\n")}
	for i := 0; i < len(s.lines); i++ {
		i = s.line(i)
	}
	return s.problems
}

// line reads line i (0-based) and returns the index of the last line it
// consumed.
func (s *tableScan) line(i int) int {
	l, n := s.lines[i], i+1
	if c := fence(l); c != 0 && (s.fence == 0 || s.fence == c) {
		s.endTable(false)
		if s.fence == 0 {
			s.fence = c
		} else {
			s.fence = 0
		}
		return i
	}
	switch {
	case s.fence != 0:
	case strings.TrimSpace(l) == "":
		s.endTable(true)
	case isRow(l) && s.header != 0:
		s.row(n, l)
	case isRow(l):
		return s.start(i)
	case s.header != 0 && !blockStart.MatchString(strings.TrimSpace(l)):
		s.problem(n, "text that is not a row inside the table whose header is line %d: a blank line must end the table before prose, or the line must start with |", s.header)
		s.endTable(false)
	default:
		s.endTable(false)
	}
	return i
}

func (s *tableScan) row(n int, l string) {
	switch got := len(cells(l)); {
	case isDelimiter(l):
		s.problem(n, "a second delimiter row in the table whose header is line %d", s.header)
	case got != s.cols:
		s.problem(n, "%d cells, want %d as in the header at line %d (a pipe inside a cell is written \\|)", got, s.cols, s.header)
	}
}

// start reads a run of rows that begins at line i and returns the last line
// it consumed: the delimiter of a new table, or the last row of a headless
// run, reported once.
func (s *tableScan) start(i int) int {
	l, n := s.lines[i], i+1
	if i+1 < len(s.lines) && isDelimiter(s.lines[i+1]) {
		if h := strings.TrimSpace(l); h == s.lastHeader {
			s.problem(n, "a table with the same header as the table at line %d, with only blank lines between: one table split in two", s.lastLine)
		}
		s.header, s.cols = n, len(cells(l))
		if got := len(cells(s.lines[i+1])); got != s.cols {
			s.problem(n+1, "the delimiter row has %d cells, want %d as in the header", got, s.cols)
		}
		return i + 1
	}
	end := i
	for end+1 < len(s.lines) && isRow(s.lines[end+1]) {
		end++
	}
	s.problem(n, "rows without a header and delimiter row (lines %d-%d): rows cut off from their table, by a blank line or a line that is not a row, a table missing its header, or prose wrapped onto a line starting with |", n, end+1)
	s.lastHeader = ""
	return end
}

// designDocs lists the design documents and ADRs, relative to docs/.
func designDocs(t *testing.T) []string {
	t.Helper()
	var docs []string
	for _, glob := range []string{"design/*.md", "design/adr/*.md"} {
		files, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %s: %v", glob, err)
		}
		docs = append(docs, files...)
	}
	if len(docs) == 0 {
		t.Fatal("found no design documents in docs/design")
	}
	return docs
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestDesignTablesWellFormed: every table in docs/design and its ADRs has a
// header, a delimiter row and rows of the header's width, in one piece.
func TestDesignTablesWellFormed(t *testing.T) {
	for _, path := range designDocs(t) {
		for _, p := range tableProblems(readDoc(t, path)) {
			t.Errorf("docs/%s:%s", filepath.ToSlash(path), strings.TrimPrefix(p, "line "))
		}
	}
}

// TestTableProblems: each malformation is reported at its line, and what is
// well formed is not.
func TestTableProblems(t *testing.T) {
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"well formed":            {"| a | b |\n|---|:-:|\n| 1 | 2 |\n| `x\\|y` | 3 |\n\ntext\n", nil},
		"no outer pipes":         {"| a | b\n|---|---\n| 1 | 2\n", nil},
		"heading after a table":  {"| a |\n|---|\n| 1 |\n## next\n", nil},
		"list after a table":     {"| a |\n|---|\n| 1 |\n- item\n", nil},
		"pipes in a code block":  {"```\n| a |\n| b | c |\n```\n", nil},
		"empty cell":             {"| a | b | c |\n|---|---|---|\n| 1 || 3 |\n", nil},
		"escaped backslash":      {"| a | b |\n|---|---|\n| 1 \\\\| 2 |\n", nil},
		"pipes in a code span":   {"| a | b | c |\n|---|---|---|\n| 1 | `x || y` |\n", []string{"line 3: 4 cells, want 3"}},
		"pipe wrapped in prose":  {"some text `x\n|| y` more\n", []string{"line 2: rows without a header"}},
		"prose then cut rows":    {"| a |\n|---|\n| 1 |\n\nprose left by a merge\n| 2 |\n| 3 |\n", []string{"line 6: rows without a header and delimiter row (lines 6-7)"}},
		"different headers":      {"| a |\n|---|\n| 1 |\n\n| b |\n|---|\n| 2 |\n", nil},
		"same header after text": {"| a |\n|---|\n| 1 |\n\nprose\n\n| a |\n|---|\n| 2 |\n", nil},
		"too many cells":         {"| a | b |\n|---|---|\n| 1 | `p|q` |\n", []string{"line 3: 3 cells, want 2"}},
		"too few cells":          {"| a | b |\n|---|---|\n| 1 |\n", []string{"line 3: 1 cells, want 2"}},
		"short delimiter":        {"| a | b |\n|---|\n| 1 | 2 |\n", []string{"line 2: the delimiter row has 1 cells, want 2"}},
		"second delimiter":       {"| a |\n|---|\n|---|\n", []string{"line 3: a second delimiter row"}},
		"no header":              {"text\n\n| 1 | 2 |\n| 3 | 4 |\n", []string{"line 3: rows without a header and delimiter row (lines 3-4)"}},
		"split by a blank line":  {"| a |\n|---|\n| 1 |\n\n| a |\n|---|\n| 2 |\n", []string{"line 5: a table with the same header as the table at line 1"}},
		"prose in a table":       {"| a |\n|---|\n| 1 |\nprose\n\n| 2 |\n", []string{"line 4: text that is not a row inside the table whose header is line 1", "line 6: rows without a header"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := tableProblems(c.text)
			if len(got) != len(c.want) {
				t.Fatalf("got %d problems %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for i, w := range c.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("problem %d: got %q, want it to start with %q", i, got[i], w)
				}
			}
		})
	}
}

// TestTableProblemsBatch53: the defect batch 53's integration made in 03
// §3.21 (a row dropped, prose and a blank line left inside the table) is
// reported at the lines it broke.
func TestTableProblemsBatch53(t *testing.T) {
	doc := readDoc(t, filepath.Join("design", "03-data-model.md"))
	lines := strings.Split(doc, "\n")
	var tampered []string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "| `assets.retirement` |"):
			continue
		case strings.HasPrefix(l, "| `assets.depreciation_batch` |"):
			tampered = append(tampered, "it (§7, T57f, migration 000200), its retirements (§8, T57g, migration", "")
		}
		tampered = append(tampered, l)
	}
	if len(tampered) != len(lines)+1 {
		t.Fatalf("the tamper changed %d lines to %d; want one row removed and two lines added: are both rows still in §3.21?", len(lines), len(tampered))
	}
	if p := tableProblems(doc); len(p) != 0 {
		t.Fatalf("03-data-model.md has table problems before the tamper: %q", p)
	}
	got := tableProblems(strings.Join(tampered, "\n"))
	want := []string{"text that is not a row inside the table", "rows without a header and delimiter row"}
	if len(got) != len(want) {
		t.Fatalf("got %d problems %q, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if !strings.Contains(got[i], w) {
			t.Errorf("problem %d: got %q, want it to say %q", i, got[i], w)
		}
	}
}

var (
	sqlComment = regexp.MustCompile(`--[^\n]*`)
	// tableStatement finds, in order, a table created (followed by its column
	// list: a name built by format(), as the yearly partitions and the
	// per-context outbox and inbox are, is not a table of the data model),
	// dropped (one or more, comma-separated) or renamed.
	tableStatement = regexp.MustCompile(`(?is)\b(?:` +
		`CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s*\(` +
		`|DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_.]*(?:\s*,\s*[a-z_][a-z0-9_.]*)*)` +
		`|ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(?:ONLY\s+)?([a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*)\s+RENAME\s+TO\s+([a-z_][a-z0-9_]*))`)
	listSeparator = regexp.MustCompile(`\s*,\s*`)
)

// liveTables applies the migrations' statements in order and returns each
// table still standing, schema-qualified and lower case, with the migration
// that created (or renamed) it.
func liveTables(migrations map[string]string) map[string]string {
	names := make([]string, 0, len(migrations))
	for name := range migrations {
		names = append(names, name)
	}
	sort.Strings(names)
	live := map[string]string{}
	for _, name := range names {
		sql := sqlComment.ReplaceAllString(migrations[name], "")
		for _, m := range tableStatement.FindAllStringSubmatch(sql, -1) {
			switch {
			case m[1] != "":
				live[strings.ToLower(m[1])] = name
			case m[2] != "":
				for _, d := range listSeparator.Split(m[2], -1) {
					delete(live, strings.ToLower(d))
				}
			default:
				old := strings.ToLower(m[3])
				delete(live, old)
				live[old[:strings.IndexByte(old, '.')+1]+strings.ToLower(m[4])] = name
			}
		}
	}
	return live
}

var (
	dottedName  = regexp.MustCompile(`\b[a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)+`)
	codeName    = regexp.MustCompile("`([a-z_][a-z0-9_]*)`")
	schemaTitle = regexp.MustCompile("^## .*\\(`([a-z_]+)`")
	// An erDiagram entity: its attribute block, or either side of a
	// relationship ("a ||--o{ b : label").
	entityBlock  = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*\{`)
	relationship = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s+\S+\s+([a-z_][a-z0-9_]*)\s*:`)
)

// namedTables returns the tables 03-data-model.md names, as the document
// names them: schema.name anywhere (prose, tables, the column lists in code
// blocks), and, in a section titled with its schema ("(`ledger`)"), an
// unqualified name in a code span outside code blocks (a table's first
// cell, or prose: "`tenant_key` holds the tenant's keys", §3.3) or as an
// entity of the section's ER diagrams. A bare code span that is a column
// of the same name in that section counts too: the check is a floor (a
// table the document never names, or names only in another section), not
// a proof that it describes the table.
func namedTables(doc string) map[string]bool {
	named := map[string]bool{}
	for _, chain := range dottedName.FindAllString(doc, -1) {
		parts := strings.Split(chain, ".")
		for i := 0; i+1 < len(parts); i++ {
			named[parts[i]+"."+parts[i+1]] = true
		}
	}
	schema, code, mermaid := "", false, false
	for _, l := range strings.Split(doc, "\n") {
		if strings.HasPrefix(l, "## ") {
			schema = ""
			if m := schemaTitle.FindStringSubmatch(l); m != nil {
				schema = m[1]
			}
		}
		if fence(l) != 0 {
			code = !code
			mermaid = code && strings.Contains(l, "mermaid")
			continue
		}
		if schema == "" || code && !mermaid {
			continue
		}
		for _, n := range sectionNames(l, mermaid) {
			named[schema+"."+n] = true
		}
	}
	return named
}

// sectionNames returns the unqualified names line gives: its code spans,
// or an ER diagram's entities.
func sectionNames(l string, mermaid bool) []string {
	var out []string
	switch {
	case mermaid:
		if m := entityBlock.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
		if m := relationship.FindStringSubmatch(l); m != nil {
			out = append(out, m[1], m[2])
		}
	default:
		for _, m := range codeName.FindAllStringSubmatch(l, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

func readMigrations(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("found no migrations in db/migrations (%v)", err)
	}
	out := map[string]string{}
	for _, f := range files {
		out[filepath.Base(f)] = readDoc(t, f)
	}
	return out
}

// TestDataModelNamesEveryTable: every table the migrations leave standing is
// named in 03-data-model.md, so a table cannot be added, or its row lost in
// a merge with nothing else naming it, without the data model saying so.
func TestDataModelNamesEveryTable(t *testing.T) {
	named := namedTables(readDoc(t, filepath.Join("design", "03-data-model.md")))
	live := liveTables(readMigrations(t))
	tables := make([]string, 0, len(live))
	for table := range live {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		if !named[table] {
			t.Errorf("db/migrations/%s creates %s, which docs/design/03-data-model.md does not name: want `%s` in the data model, or the bare name in a code span or ER entity of its schema's section", live[table], table, table)
		}
	}
}

// TestLiveTables: creations, drops (one or several) and renames apply in
// migration order; comments and format()-built names are not tables.
func TestLiveTables(t *testing.T) {
	got := liveTables(map[string]string{
		"000002_b.up.sql": "DROP TABLE IF EXISTS s.gone, s.also;\nALTER TABLE s.old RENAME TO renamed;",
		"000001_a.up.sql": "CREATE TABLE s.kept (id int);\nCREATE TABLE IF NOT EXISTS s.gone(id int);\n" +
			"create table s.also (id int);\nCREATE TABLE s.old (id int);\n-- CREATE TABLE s.commented (id int);\n" +
			"EXECUTE format('CREATE TABLE s.part_%s PARTITION OF s.kept', y);",
	})
	want := map[string]string{"s.kept": "000001_a.up.sql", "s.renamed": "000002_b.up.sql"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestNamedTables: the ways 03-data-model.md names a table, and mentions
// that do not count.
func TestNamedTables(t *testing.T) {
	doc := "## 3.2 Reference (`ref`)\n\n| Table | Purpose |\n|---|---|\n| `currency` | x |\n\n" +
		"Prose names `in_prose`.\n\n```mermaid\nerDiagram\n  a ||--o{ b : has\n  c { uuid id PK }\n```\n\n" +
		"```\nnot `in_code`\n```\n\nbare_word\n\n" +
		"## 3.9 Other\n\n| `elsewhere` | x |\n|---|---|\n\nSee payables.supplier_invoice.purchase_order_id and `closing.close_task`.\n"
	named := namedTables(doc)
	for table, want := range map[string]bool{
		"ref.currency": true, "ref.in_prose": true, "ref.a": true, "ref.b": true, "ref.c": true,
		"payables.supplier_invoice": true, "closing.close_task": true,
		"ref.in_code": false, "ref.bare_word": false, "ref.elsewhere": false, "ref.uuid": false,
	} {
		t.Run(table, func(t *testing.T) {
			if got := named[table]; got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
