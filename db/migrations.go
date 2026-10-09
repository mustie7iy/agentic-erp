// Package db holds the physical data model: versioned PostgreSQL migrations
// (docs/design/03-data-model.md). Files are named NNNNNN_name.up.sql and
// NNNNNN_name.down.sql and are applied in order.
package db

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var files embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Name string
	Up   string
	Down string
}

// Migrations returns every migration in apply order.
func Migrations() []Migration {
	ups, _ := fs.Glob(files, "migrations/*.up.sql") // the pattern is constant and valid
	sort.Strings(ups)
	out := make([]Migration, 0, len(ups))
	for _, up := range ups {
		name := strings.TrimSuffix(strings.TrimPrefix(up, "migrations/"), ".up.sql")
		u, _ := files.ReadFile(up) // embedded files always read
		d, _ := files.ReadFile("migrations/" + name + ".down.sql")
		out = append(out, Migration{Name: name, Up: string(u), Down: string(d)})
	}
	return out
}
