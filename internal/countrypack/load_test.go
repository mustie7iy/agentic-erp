package countrypack

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mustie7iy/agentic-erp/packs"
)

// testFS is the test packs with a hash list releasing each as it is.
func testFS(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	src := os.DirFS("testdata/packs")
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		m[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	release(t, m)
	return m
}

// release writes a hash list releasing every pack in m as it is now.
func release(t *testing.T, m fstest.MapFS) {
	t.Helper()
	dirs, err := packDirs(m)
	if err != nil {
		t.Fatalf("packDirs: %v", err)
	}
	list := "# test packs\n\n"
	for _, d := range dirs {
		h, err := Hash(m, d)
		if err != nil {
			t.Fatalf("hash %s: %v", d, err)
		}
		list += h + "  " + d + "\n"
	}
	m[ReleasedFile] = &fstest.MapFile{Data: []byte(list)}
}

// The packs in the image load: each is valid and still the version that
// was released (ADR-0048).
func TestReleasedPacks(t *testing.T) {
	if _, err := LoadAll(packs.FS); err != nil {
		t.Fatalf("LoadAll(packs.FS): %v", err)
	}
}

// Every pack directory on disk is embedded: one left out of the embed
// pattern would be silently missing from the image.
func TestEveryPackEmbedded(t *testing.T) {
	onDisk, err := packDirs(os.DirFS("../../packs"))
	if err != nil {
		t.Fatalf("packDirs(disk): %v", err)
	}
	embedded, err := packDirs(packs.FS)
	if err != nil {
		t.Fatalf("packDirs(embedded): %v", err)
	}
	if !slices.Equal(onDisk, embedded) {
		t.Errorf("embedded packs: got %v, want %v (the packs on disk)", embedded, onDisk)
	}
}

func TestLoadAll(t *testing.T) {
	m := testFS(t)
	m["README.md"] = &fstest.MapFile{Data: []byte("not a pack")}
	m["zz/notes.txt"] = &fstest.MapFile{Data: []byte("not a version")}
	lib, err := LoadAll(m)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if p := lib.Get("ZZ", "2026.1"); p == nil || p.Manifest.Name != "Testland" {
		t.Errorf("Get(ZZ, 2026.1): got %+v, want Testland", p)
	}
	if p := lib.Latest("zz"); p == nil || p.Manifest.Version != "2026.2" {
		t.Errorf("Latest(zz): got %+v, want version 2026.2", p)
	}
	if p := lib.Latest("CA"); p != nil {
		t.Errorf("Latest(CA): got %+v, want nil", p)
	}
	var dirs []string
	for _, p := range lib.All() {
		dirs = append(dirs, p.Dir())
		if want, _ := Hash(m, p.Dir()); lib.Hash(p) != want {
			t.Errorf("Hash(%s): got %q, want %q", p.Dir(), lib.Hash(p), want)
		}
	}
	if want := []string{"zz/2026.1", "zz/2026.2"}; !slices.Equal(dirs, want) {
		t.Errorf("All: got %v, want %v", dirs, want)
	}
	if len(lib.Warnings) != 0 {
		t.Errorf("warnings: got %v, want none", lib.Warnings)
	}
}

func TestLoadAllWarnsWithoutSignOff(t *testing.T) {
	m := testFS(t)
	f := m["zz/2026.1/pack.yaml"]
	f.Data = []byte(strings.Split(string(f.Data), "sign_off:")[0])
	release(t, m)
	lib, err := LoadAll(m)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(lib.Warnings) != 1 || lib.Warnings[0].Path != "zz/2026.1/pack.yaml: sign_off" {
		t.Errorf("warnings: got %v, want the missing sign-off", lib.Warnings)
	}
	if lib.Get("ZZ", "2026.1").SignedOff() {
		t.Error("SignedOff: got true, want false")
	}
}

// failFS fails opening one name.
type failFS struct {
	fs.FS
	fail string
}

func (f failFS) Open(name string) (fs.File, error) {
	if name == f.fail {
		return nil, errors.New("disk on fire")
	}
	return f.FS.Open(name)
}

func TestLoadAllRefuses(t *testing.T) {
	tests := []struct {
		name   string
		change func(fstest.MapFS) fs.FS
		want   string
	}{
		{"changed after release", func(m fstest.MapFS) fs.FS {
			m["zz/2026.1/calendar.yaml"].Data = []byte("holidays: []\n")
			return m
		}, "a released pack never changes"},
		{"not released", func(m fstest.MapFS) fs.FS {
			m[ReleasedFile].Data = []byte(strings.Split(string(m[ReleasedFile].Data), "zz/2026.1")[1])
			return m
		}, "zz/2026.1 is not in released.sha256"},
		{"released pack removed", func(m fstest.MapFS) fs.FS {
			m[ReleasedFile].Data = append(m[ReleasedFile].Data, []byte(strings.Repeat("a", 64)+"  zz/2025.1\n")...)
			return m
		}, "released pack zz/2025.1 is missing"},
		{"malformed hash list", func(m fstest.MapFS) fs.FS {
			m[ReleasedFile].Data = []byte("abc zz/2026.1\n")
			return m
		}, "line 1: want <sha256>"},
		{"pack listed twice", func(m fstest.MapFS) fs.FS {
			line := strings.Split(string(m[ReleasedFile].Data), "\n")[2]
			m[ReleasedFile].Data = []byte(line + "\n" + line + "\n")
			return m
		}, "listed twice"},
		{"no hash list", func(m fstest.MapFS) fs.FS { delete(m, ReleasedFile); return m }, "released.sha256"},
		{"invalid pack", func(m fstest.MapFS) fs.FS {
			m["zz/2026.1/payments.yaml"].Data = []byte("payment_terms: [{key: \"\"}]\n")
			return m
		}, "zz/2026.1 is invalid"},
		{"unparsable pack", func(m fstest.MapFS) fs.FS {
			m["zz/2026.1/payments.yaml"].Data = []byte("payment_term: []\n")
			return m
		}, "zz/2026.1: countrypack: payments.yaml"},
		{"two documents in a file", func(m fstest.MapFS) fs.FS {
			m["zz/2026.1/payments.yaml"].Data = []byte("payment_terms: []\n---\npayment_terms: []\n")
			return m
		}, "one YAML document expected"},
		{"upper-case hash", func(m fstest.MapFS) fs.FS {
			m[ReleasedFile].Data = []byte(strings.ToUpper(string(m[ReleasedFile].Data)))
			return m
		}, "line 3: want <sha256>"},
		{"pack in the wrong directory", func(m fstest.MapFS) fs.FS {
			for _, f := range Files {
				m["zz/2027.1/"+f] = m["zz/2026.1/"+f]
			}
			return m
		}, "belongs in zz/2026.1"},
		{"root unreadable", func(m fstest.MapFS) fs.FS { return failFS{m, "."} }, "disk on fire"},
		{"country unreadable", func(m fstest.MapFS) fs.FS { return failFS{m, "zz"} }, "disk on fire"},
		{"pack unreadable", func(m fstest.MapFS) fs.FS { return failFS{m, "zz/2026.1"} }, "disk on fire"},
		{"file unreadable", func(m fstest.MapFS) fs.FS { return failFS{m, "zz/2026.1/tax.yaml"} }, "disk on fire"},
	}
	rereleased := map[string]bool{"invalid pack": true, "two documents in a file": true, "unparsable pack": true, "pack in the wrong directory": true}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := tt.change(testFS(t))
			if m, ok := fsys.(fstest.MapFS); ok && rereleased[tt.name] {
				release(t, m) // the change is released, to reach what follows the hash check
			}
			_, err := LoadAll(fsys)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("LoadAll: got %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// Load reads files Hash did not, so its own read failure is reached only
// when the file vanishes between the two; failing it directly covers it.
func TestLoadReadFailure(t *testing.T) {
	m := testFS(t)
	if _, _, err := Load(failFS{m, "zz/2026.1/pack.yaml"}, "zz/2026.1"); err == nil {
		t.Error("Load: got nil, want the read error")
	}
	if _, _, err := Load(m, "zz/none"); err == nil {
		t.Error("Load of a missing directory: got nil, want an error")
	}
}

// Packs are ordered by country first, then by version as numbers.
func TestOrder(t *testing.T) {
	pack := func(country, version string) *Pack {
		return &Pack{Manifest: Manifest{Country: country, Version: version}}
	}
	tests := []struct {
		name string
		a, b *Pack
		want int
	}{
		{"country first", pack("CA", "2027.1"), pack("ZZ", "2026.1"), -1},
		{"version as numbers", pack("ZZ", "2026.10"), pack("ZZ", "2026.9"), 1},
		{"same", pack("ZZ", "2026.1"), pack("ZZ", "2026.1"), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Order(tt.a, tt.b); got != tt.want {
				t.Errorf("Order: got %d, want %d", got, tt.want)
			}
		})
	}
}

// unreadable is a file system whose files cannot be read.
type unreadable struct{ fstest.MapFS }

func (u unreadable) ReadFile(string) ([]byte, error) { return nil, fs.ErrPermission }

func (u unreadable) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".yaml") {
		return nil, fs.ErrPermission
	}
	return u.MapFS.Open(name)
}

// TestAddUnreleased: packs from a directory join the library as they are,
// each warned of as not released; a released pack's directory, an invalid
// pack or an unreadable one is refused.
func TestAddUnreleased(t *testing.T) {
	next := func() fstest.MapFS {
		m := fstest.MapFS{}
		src := testFS(t)
		for name, f := range src {
			if rest, ok := strings.CutPrefix(name, "zz/2026.2/"); ok {
				m["zz/2026.3/"+rest] = &fstest.MapFile{Data: f.Data}
			}
		}
		manifest, _, _ := strings.Cut(strings.Replace(string(m["zz/2026.3/pack.yaml"].Data), `version: "2026.2"`, `version: "2026.3"`, 1), "sign_off:")
		m["zz/2026.3/pack.yaml"].Data = []byte(manifest) // not signed off: a warning
		m["zz/2026.3/returns.yaml"] = &fstest.MapFile{Data: []byte(testReturns)}
		return m
	}
	lib, err := LoadAll(testFS(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.AddUnreleased(next()); err != nil {
		t.Fatal(err)
	}
	p := lib.Get("ZZ", "2026.3")
	if p == nil || p.Return("TEST-1") == nil || lib.Hash(p) == "" || lib.Latest("ZZ") != p {
		t.Fatalf("the unreleased pack: got %v, want it with its return, hashed, the latest", p)
	}
	var warned []string
	for _, w := range lib.Warnings {
		warned = append(warned, w.Path)
	}
	if got, want := strings.Join(warned, " "), "zz/2026.3 zz/2026.3/pack.yaml: sign_off"; got != want {
		t.Errorf("warnings: got %s, want %s", got, want)
	}
	invalid := next()
	invalid["zz/2026.3/pack.yaml"].Data = []byte("country: zz\n")
	unsigned := next()
	unsigned["zz/2026.3/pack.yaml"].Data = append(unsigned["zz/2026.3/pack.yaml"].Data, "signed: nobody\n"...)
	for name, c := range map[string]struct {
		fsys fs.FS
		want string
	}{
		"a released one":    {testFS(t), "is released"},
		"an invalid one":    {invalid, "2026.3"},
		"an unreadable one": {unreadable{next()}, "permission"},
		"no directory":      {os.DirFS(filepath.Join(t.TempDir(), "none")), "no such file"},
		"unknown sign-off":  {unsigned, "signed"},
	} {
		t.Run(name, func(t *testing.T) {
			lib, err := LoadAll(testFS(t))
			if err != nil {
				t.Fatal(err)
			}
			if err := lib.AddUnreleased(c.fsys); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error naming %q", err, c.want)
			}
		})
	}
}
