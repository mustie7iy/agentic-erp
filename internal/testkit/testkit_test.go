package testkit

import (
	"bytes"
	"os"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// The two copies of the reference configuration must stay identical.
func TestFixtureCopiesMatch(t *testing.T) {
	other, err := os.ReadFile("../config/testdata/valid.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(other, ConfigYAML) {
		t.Fatal("internal/testkit/config.yaml and internal/config/testdata/valid.yaml differ")
	}
	if config.HasErrors(config.Validate(Config(t))) {
		t.Fatal("reference configuration must be valid")
	}
}

func TestFixtures(t *testing.T) {
	if s := Store(t); len(s.Companies) != 2 {
		t.Fatal("store fixture")
	}
	r := Repo(t)
	if _, v, err := r.Active(); err != nil || v != 1 {
		t.Fatal("repo fixture")
	}
	if len(YAML(t, Config(t))) == 0 || (MemSource{}).Name() != "memory" {
		t.Fatal("helpers")
	}
}
