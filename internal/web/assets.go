package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The vendored htmx (ADR-0052 §4): one file, served from this origin, no
// build step. An upgrade replaces the file and these constants in one
// reviewed change; a file that does not match them fails startup.
const (
	HTMXFile   = "vendor/htmx-2.0.11.min.js"
	HTMXSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"
	CSSFile    = "css/workspace.css"
)

// static is what the workspace serves besides its pages: the stylesheet
// and htmx, the only script (a test walks the tree for any other).
//
//go:embed static/css/*.css static/vendor/*.js
var static embed.FS

// asset is one static file.
type asset struct {
	body        []byte
	contentType string
}

// Assets are the static files by their content-hashed paths, so a
// response can be cached for a year: a changed file has a new path.
type Assets struct {
	CSS    string // the stylesheet's path
	Script string // htmx's path
	files  map[string]asset
}

// LoadAssets reads the embedded static files.
func LoadAssets() (*Assets, error) {
	sub, _ := fs.Sub(static, "static") // fails only for an invalid path, and "static" is valid
	return loadAssets(sub)
}

// loadAssets reads the static files of fsys, each a stylesheet or a
// script, checking htmx against its pinned hash.
func loadAssets(fsys fs.FS) (*Assets, error) {
	a := &Assets{files: map[string]asset{}}
	paths := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		types := map[string]string{".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8"}
		ext := path.Ext(name)
		if types[ext] == "" {
			return fmt.Errorf("%s: only stylesheets and htmx are served", name)
		}
		sum := sha256.Sum256(body)
		if name == HTMXFile && hex.EncodeToString(sum[:]) != HTMXSHA256 {
			return fmt.Errorf("%s: SHA-256 %x, want the pinned %s", name, sum, HTMXSHA256)
		}
		hashed := "/static/" + strings.TrimSuffix(name, ext) + "." + hex.EncodeToString(sum[:6]) + ext
		a.files[hashed] = asset{body: body, contentType: types[ext]}
		paths[name] = hashed
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: static files: %w", err)
	}
	a.CSS, a.Script = paths[CSSFile], paths[HTMXFile]
	if a.CSS == "" || a.Script == "" {
		return nil, fmt.Errorf("web: static files: want %s and %s", CSSFile, HTMXFile)
	}
	return a, nil
}

// ServeHTTP serves a static file by its hashed path, cached for a year.
func (a *Assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f, ok := a.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", f.contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Write(f.body)
}
