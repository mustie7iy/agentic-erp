package db

import (
	"fmt"
	"io/fs"
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
		if strings.TrimSpace(m.Up) == "" || strings.TrimSpace(m.Down) == "" {
			t.Errorf("%s: up and down must both be present and non-empty", m.Name)
		}
	}
	downs, _ := fs.Glob(files, "migrations/*.down.sql")
	if len(downs) != len(ms) {
		t.Errorf("%d down migrations for %d up migrations", len(downs), len(ms))
	}
}
