package countrypack

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/config"
)

// ReleasedFile lists the hash of every released pack directory.
const ReleasedFile = "released.sha256"

// Library is every released pack, by directory (cc/version).
type Library struct {
	packs  map[string]*Pack
	hashes map[string]string // by directory, as released
	// Warnings are the packs' findings that do not block loading, such as
	// a missing tax specialist's sign-off (the pilot gate), each path
	// starting with the pack's directory.
	Warnings []config.Finding
}

// Get is the pack of a country (either case) and version, or nil.
func (l *Library) Get(country, version string) *Pack {
	return l.packs[strings.ToLower(country)+"/"+version]
}

// All is every pack, by country, then version.
func (l *Library) All() []*Pack {
	out := slices.Collect(maps.Values(l.packs))
	slices.SortFunc(out, Order)
	return out
}

// Order orders packs by country, then version, as cmp.Compare does.
func Order(a, b *Pack) int {
	if c := cmp.Compare(a.Manifest.Country, b.Manifest.Country); c != 0 {
		return c
	}
	return CompareVersions(a.Manifest.Version, b.Manifest.Version)
}

// Hash is the hash the pack was released with, from the hash list.
func (l *Library) Hash(p *Pack) string { return l.hashes[p.Dir()] }

// Latest is the newest version of a country's pack, or nil.
func (l *Library) Latest(country string) *Pack {
	var latest *Pack
	for _, p := range l.packs {
		if strings.EqualFold(p.Manifest.Country, country) && (latest == nil || CompareVersions(p.Manifest.Version, latest.Manifest.Version) > 0) {
			latest = p
		}
	}
	return latest
}

// LoadAll loads every pack in fsys (cc/version directories beside the
// hash list) and refuses any that is invalid, whose hash is not the
// released one, or that is listed but gone: released packs never change
// and are never removed, since an upgrade merges from the adopted one.
func LoadAll(fsys fs.FS) (*Library, error) {
	released, err := readReleased(fsys)
	if err != nil {
		return nil, err
	}
	dirs, err := packDirs(fsys)
	if err != nil {
		return nil, err
	}
	lib := &Library{packs: map[string]*Pack{}, hashes: map[string]string{}}
	for _, dir := range dirs {
		p, warnings, err := loadReleased(fsys, dir, released)
		if err != nil {
			return nil, err
		}
		lib.packs[dir], lib.hashes[dir] = p, released[dir]
		for _, w := range warnings { // named by the pack, as several share file names
			w.Path = dir + "/" + w.Path
			lib.Warnings = append(lib.Warnings, w)
		}
		delete(released, dir)
	}
	for dir := range released {
		return nil, fmt.Errorf("countrypack: released pack %s is missing; released packs are never removed", dir)
	}
	return lib, nil
}

// packDirs lists the cc/version directories; files beside them, such as
// the hash list, are not packs.
func packDirs(fsys fs.FS) ([]string, error) {
	countries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("countrypack: %w", err)
	}
	var dirs []string
	for _, c := range countries {
		if !c.IsDir() {
			continue
		}
		versions, err := fs.ReadDir(fsys, c.Name())
		if err != nil {
			return nil, fmt.Errorf("countrypack: %w", err)
		}
		for _, v := range versions {
			if v.IsDir() {
				dirs = append(dirs, c.Name()+"/"+v.Name())
			}
		}
	}
	return dirs, nil
}

func loadReleased(fsys fs.FS, dir string, released map[string]string) (*Pack, []config.Finding, error) {
	h, err := Hash(fsys, dir)
	if err != nil {
		return nil, nil, err
	}
	switch want, ok := released[dir]; {
	case !ok:
		return nil, nil, fmt.Errorf("countrypack: pack %s is not in %s; release it with the line %q", dir, ReleasedFile, h+"  "+dir)
	case want != h:
		return nil, nil, fmt.Errorf("countrypack: pack %s has hash %s but was released as %s; a released pack never changes, make a new version", dir, h, want)
	}
	return Load(fsys, dir)
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readReleased reads the hash list: "<sha256>  <cc>/<version>" per line,
// with # comments and blank lines.
func readReleased(fsys fs.FS) (map[string]string, error) {
	b, err := fs.ReadFile(fsys, ReleasedFile)
	if err != nil {
		return nil, fmt.Errorf("countrypack: %w", err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !hashRe.MatchString(fields[0]) {
			return nil, fmt.Errorf("countrypack: %s line %d: want <sha256> <cc>/<version>", ReleasedFile, n)
		}
		if _, dup := out[fields[1]]; dup {
			return nil, fmt.Errorf("countrypack: %s line %d: %s listed twice", ReleasedFile, n, fields[1])
		}
		out[fields[1]] = fields[0]
	}
	return out, nil
}

// Hash is the sha256 of a pack directory: each file's name, length and
// content, in name order.
func Hash(fsys fs.FS, dir string) (string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return "", fmt.Errorf("countrypack: %w", err)
	}
	h := sha256.New()
	for _, e := range entries {
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return "", fmt.Errorf("countrypack: %w", err)
		}
		h.Write([]byte(e.Name() + "\x00" + strconv.Itoa(len(b)) + "\x00"))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Load reads, parses and validates the pack in dir, which must be named
// after its manifest. Findings with error severity refuse it; the warnings
// are returned.
func Load(fsys fs.FS, dir string) (*Pack, []config.Finding, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("countrypack: %w", err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if files[e.Name()], err = fs.ReadFile(fsys, path.Join(dir, e.Name())); err != nil {
			return nil, nil, fmt.Errorf("countrypack: %w", err)
		}
	}
	p, err := Parse(files)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", dir, err)
	}
	findings := Validate(p)
	if config.HasErrors(findings) {
		return nil, nil, fmt.Errorf("countrypack: %s is invalid: %v", dir, findings)
	}
	if p.Dir() != dir {
		return nil, nil, fmt.Errorf("countrypack: directory %s holds the pack of %s, which belongs in %s", dir, p.Manifest.Country+" "+p.Manifest.Version, p.Dir())
	}
	return p, findings, nil
}

// AddUnreleased adds the packs in fsys (cc/version directories) as they
// are, hashed but not released: for a demonstration or a test of a pack
// not yet released (ADR-0048; the walkthrough used one until a released
// pack defined a return, T61j). Production refuses them (ERP_PACKS_DIR). A pack already
// in the library, or invalid, is refused; each warns that it is not
// released.
func (l *Library) AddUnreleased(fsys fs.FS) error {
	dirs, err := packDirs(fsys)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if _, dup := l.packs[dir]; dup {
			return fmt.Errorf("countrypack: pack %s is released; an unreleased pack takes another version", dir)
		}
		h, err := Hash(fsys, dir)
		if err != nil {
			return err
		}
		p, warnings, err := Load(fsys, dir)
		if err != nil {
			return err
		}
		l.packs[dir], l.hashes[dir] = p, h
		l.Warnings = append(l.Warnings, config.Finding{Severity: config.SevWarning, Path: dir,
			Message: "not released: added from a directory for a demonstration or a test; production refuses it"})
		for _, w := range warnings {
			w.Path = dir + "/" + w.Path
			l.Warnings = append(l.Warnings, w)
		}
	}
	return nil
}
