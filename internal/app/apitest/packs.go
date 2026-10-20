package apitest

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mustie7iy/agentic-erp/internal/countrypack"
)

// Packs reads the country packs below dir (<cc>/<version>/ directories,
// as packs/ holds them) and releases each as it is, for app.Config.Packs:
// a test's own packs, beside the image's.
func Packs(tb testing.TB, dir string) fstest.MapFS {
	tb.Helper()
	m := fstest.MapFS{}
	src := os.DirFS(dir)
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		m[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		tb.Fatalf("apitest: read packs %s: %v", dir, err)
	}
	var released strings.Builder
	versions, _ := fs.Glob(m, "*/*") // the pattern is valid
	for _, v := range versions {
		h, err := countrypack.Hash(m, v)
		if err != nil {
			tb.Fatalf("apitest: hash pack %s: %v", v, err)
		}
		released.WriteString(h + "  " + v + "\n")
	}
	m[countrypack.ReleasedFile] = &fstest.MapFile{Data: []byte(released.String())}
	return m
}
