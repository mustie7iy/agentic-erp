// Package datasets is the analytical store's dataset contract (ADR-0044):
// the schema files under v1/, embedded so the extractor's tests, the
// semantic layer's contract test and erp-analytics' reader all read the
// same columns. The reader selects only these columns, so a column the
// contract does not list never leaves a Parquet file.
package datasets

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed v1/*.yaml
var v1 embed.FS

// Column is one Parquet column of a dataset.
type Column struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	Role     string `yaml:"role"`
	Nullable bool   `yaml:"nullable"`
}

// Join names a dimension dataset a fact dataset is joined to at read time.
type Join struct {
	Dataset string   `yaml:"dataset"`
	On      []string `yaml:"on"`
}

// Derived is a name a fact dataset answers by a rule over another dataset.
type Derived struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
	Rule string `yaml:"rule"`
}

// Dataset is one schema file.
type Dataset struct {
	Dataset   string    `yaml:"dataset"`
	Version   int       `yaml:"version"`
	Kind      string    `yaml:"kind"` // fact | dimension
	Grain     string    `yaml:"grain"`
	Load      string    `yaml:"load"` // append | snapshot
	Watermark string    `yaml:"watermark"`
	Date      string    `yaml:"date"`
	Joins     []Join    `yaml:"joins"`
	Derived   []Derived `yaml:"derived"`
	Columns   []Column  `yaml:"columns"`
}

// Column is the named column, or nil.
func (d *Dataset) Column(name string) *Column {
	for i := range d.Columns {
		if d.Columns[i].Name == name {
			return &d.Columns[i]
		}
	}
	return nil
}

// WithRole lists the columns of a role.
func (d *Dataset) WithRole(role string) []Column {
	var out []Column
	for _, c := range d.Columns {
		if c.Role == role {
			out = append(out, c)
		}
	}
	return out
}

// V1 is version 1 of the contract, by dataset name.
func V1() (map[string]*Dataset, error) { return parse(v1, "v1") }

// parse reads every schema file of dir, refusing an unknown field and a
// file whose dataset is not its name.
func parse(fsys fs.FS, dir string) (map[string]*Dataset, error) {
	files, err := fs.Glob(fsys, dir+"/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("datasets: %s: %w", dir, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("datasets: %s holds no schema file", dir)
	}
	out := map[string]*Dataset{}
	for _, f := range files {
		b, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("datasets: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		var d Dataset
		if err := dec.Decode(&d); err != nil {
			return nil, fmt.Errorf("datasets: %s: %w", f, err)
		}
		if want := strings.TrimSuffix(path.Base(f), ".yaml"); d.Dataset != want {
			return nil, fmt.Errorf("datasets: %s names dataset %q, want %q", f, d.Dataset, want)
		}
		out[d.Dataset] = &d
	}
	return out, nil
}
