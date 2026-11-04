package db

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Packaging rules that hold without a database: numbered without gaps, every
// up has a down, and no migration is empty.
func TestMigrationFiles(t *testing.T) {
	ms := Migrations()
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range ms {
		if want := fmt.Sprintf("%06d_", i+1); !strings.HasPrefix(m.Name, want) {
			t.Errorf("migration %d is %q, want prefix %q", i+1, m.Name, want)
		}
		if strings.TrimSpace(m.Up) == "" {
			t.Errorf("%s: up migration: got empty, want SQL", m.Name)
		}
		if strings.TrimSpace(m.Down) == "" {
			t.Errorf("%s: down migration: got empty, want SQL", m.Name)
		}
	}
	downs, _ := fs.Glob(files, "migrations/*.down.sql")
	if len(downs) != len(ms) {
		t.Errorf("%d down migrations for %d up migrations", len(downs), len(ms))
	}
}

// migrationName is a migration's file name without .up.sql, as tests
// name it.
var migrationName = regexp.MustCompile(`^[0-9]{6}_[a-z0-9_]+$`)

// TestMigrationTestsNameTheirMigration: a test that migrates part of the
// way names the migration it is about by file (migrateBefore and
// migrationNamed take NNNNNN_slug), and that file exists, so integration's
// renumbering of a lane's migrations leaves no test seeding before the
// wrong one (batch 54: a test kept 199 after its migration became 211, and
// passed). Its doc comment names the same number, so the prose renumbers
// with it. The rule is read from the source, so it holds in -short runs,
// which skip the database tests themselves.
func TestMigrationTestsNameTheirMigration(t *testing.T) {
	known := map[string]bool{}
	for _, m := range Migrations() {
		known[m.Name] = true
	}
	tests, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	callers := 0
	for _, path := range tests {
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || helperOfMigrations[fn.Name.Name] || !callsMigrationHelper(fn.Body) {
				continue
			}
			callers++
			checkMigrationNames(t, fset.Position(fn.Pos()).String(), fn, known)
		}
	}
	if callers == 0 {
		t.Error("tests naming a migration: got none, want the contract-step tests (is the helper renamed?)")
	}
}

// helperOfMigrations are the helpers that name a migration for a test;
// they take the name from their caller.
var helperOfMigrations = map[string]bool{"migrateBefore": true, "migrationNamed": true}

func callsMigrationHelper(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && helperOfMigrations[id.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// checkMigrationNames checks the migration file names fn spells: at least
// one, each an embedded migration, each one's number in fn's doc comment.
func checkMigrationNames(t *testing.T, at string, fn *ast.FuncDecl, known map[string]bool) {
	t.Helper()
	var names []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil && migrationName.MatchString(s) {
				names = append(names, s)
			}
		}
		return true
	})
	if len(names) == 0 {
		t.Errorf("%s %s: got no migration file name, want the NNNNNN_slug of the migration it tests as a literal", at, fn.Name.Name)
	}
	doc := fn.Doc.Text()
	for _, name := range names {
		if !known[name] {
			t.Errorf("%s %s: migration %s: got no such file in db/migrations, want the migration's current name (renumbered?)", at, fn.Name.Name, name)
		}
		if number := name[:6]; !strings.Contains(doc, number) {
			t.Errorf("%s %s: doc comment: got no %s, want it to name the migration it tests", at, fn.Name.Name, number)
		}
	}
}
