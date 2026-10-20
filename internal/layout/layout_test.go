package layout

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/mustie7iy/agentic-erp/internal/"

// shared are packages every context may import: the shared kernel (§6.7).
var shared = map[string]bool{"money": true}

// platformUsers are the platform packages allowed to import contexts: only
// test support that builds a configured tenant. Every other platform
// package serves the contexts and imports none of them.
var platformUsers = map[string]bool{"platform/fixture": true}

// compositionRoot is the package that wires every context's handlers into
// the API and their ports together, and so, like a command, may import any
// context; its sub-packages (apitest) may not.
const compositionRoot = "app"

// testSupport are packages only tests and other test support may import:
// fixtures, fake servers and harnesses; the same packages scripts/coverage.sh
// leaves out of the coverage figure, so a new one is added to both. Production code importing one would
// ship test machinery and, through fixture, half the contexts.
var testSupport = map[string]bool{
	"platform/pgtest": true, "platform/fixture": true, "app/apitest": true,
	"platform/authn/authntest": true, "platform/natsbus/natstest": true, "platform/tlsconf/tlstest": true,
	"platform/blobstore/blobstoretest": true, "platform/fieldcrypt/kmstest": true, "platform/sftpclient/sftptest": true,
	"platform/secrets/vaulttest": true, "platform/workflow/workflowtest": true,
	"llm/llmtest": true, "nfr": true,
}

// allowed is every import from one context to another that production code
// makes today, by importing unit. A new one fails TestImportRules; one that
// is no longer made fails it too, so the list stays exact.
var allowed = map[string][]string{
	"agent":                   {"apitools", "llm"},
	"analytics":               {"apitools"},
	"apitools":                {"llm"},
	"app":                     {"countrypack"}, // apitest releases a test's own country packs
	"approvals":               {"config"},
	"audit":                   {"operations"},
	"capture":                 {"llm"},
	"config":                  {"bankfile"},
	"configuration":           {"approvals", "config", "countrypack", "operations"},
	"countrypack":             {"config"},
	"controlplane":            {"agents", "audit", "config", "directory", "identity", "offboarding", "provisioning", "tenancy"},
	"delivery/configagent":    {"apitools", "llm"},
	"delivery/migrationagent": {"apitools", "llm"},
	"delivery/sapconfig":      {"config", "sap"},
	"delivery/testagent":      {"apitools", "llm"},
	"directory":               {"tenancy"},
	"drverify":                {"audit", "controlplane", "directory", "offboarding", "tenancy"},
	"eval":                    {"apitools", "llm"},
	"globalplane":             {"directory", "offboarding"},
	"integration":             {"bankfile", "config", "ledger", "operations", "partner", "payables", "procurement", "sap", "tenancy"},
	"ledger":                  {"approvals", "config", "tenancy"},
	"nfr":                     {"analytics", "app", "ledger"},
	"offboarding":             {"tenancy"},
	"partner":                 {"approvals", "bankfile", "config", "tenancy"},
	"payables":                {"approvals", "capture", "config", "configuration", "ledger", "operations", "partner", "procurement", "tenancy"},
	"payments":                {"bankfile", "config", "ledger", "operations", "partner", "tenancy"},
	"procurement":             {"approvals", "config", "ledger", "partner", "tenancy"},
	"provisioning":            {"config", "configuration", "identity", "tenancy"},
	"router":                  {"directory"},
	"workflows":               {"approvals", "configuration", "integration", "operations", "payments"},
}

// unit is the context a package under internal/ belongs to: its first path
// segment, the platform package itself, a delivery agent (each agent under
// delivery/ stands alone) or a command.
func unit(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) > 1 && (parts[0] == "platform" || parts[0] == "delivery" || parts[0] == "cmd") {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

// imports reads the production imports of every package under internal/
// (keyed by its path there) and cmd/ (keyed cmd/<name>); tests may import
// freely.
func imports(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for dir, prefix := range map[string]string{"..": "", "../../cmd": "cmd/"} {
		root, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if err := walk(root, prefix, out); err != nil {
			t.Fatalf("parse %s: %v", root, err)
		}
	}
	return out
}

// walk adds the imports of the production files below root to out, keying
// each package by prefix and its path below root. Build constraints are not
// applied: every variant of a file is checked.
func walk(root, prefix string, out map[string]map[string]bool) error {
	fset := token.NewFileSet()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// What the go tool ignores is not a package: testdata, vendor, _ and . directories.
			if name := d.Name(); path != root && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := prefix + filepath.ToSlash(rel)
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if dep, ok := strings.CutPrefix(imp, module); ok {
				if out[pkg] == nil {
					out[pkg] = map[string]bool{}
				}
				out[pkg][dep] = true
			}
		}
		return nil
	})
}

// TestImportRules: production code (internal/ and cmd/) imports no test
// support, platform packages (other than platformUsers) import no context,
// the composition root and commands import any, and a context imports
// another only where allowed says so.
func TestImportRules(t *testing.T) {
	used := map[string]bool{}
	for pkg, deps := range imports(t) {
		from := unit(pkg)
		for dep := range deps {
			to := unit(dep)
			if testSupport[dep] && !testSupport[pkg] {
				t.Errorf("%s imports %s: production code may not import test support", pkg, dep)
				continue
			}
			switch {
			case from == to, strings.HasPrefix(to, "platform/"), shared[to]:
			case strings.HasPrefix(from, "cmd/"), pkg == compositionRoot: // a command and the composition root wire contexts together
			case strings.HasPrefix(from, "platform/"):
				if !platformUsers[from] {
					t.Errorf("%s imports %s: a platform package may not depend on a context", pkg, dep)
				}
			case slices.Contains(allowed[from], to):
				used[from+" -> "+to] = true
			default:
				t.Errorf("%s imports %s: not in the allowlist of internal/layout (06 §6.7); add it with the interface it uses, and an ADR if it turns a dependency around", pkg, dep)
			}
		}
	}
	var stale []string
	for from, tos := range allowed {
		for _, to := range tos {
			if !used[from+" -> "+to] {
				stale = append(stale, from+" -> "+to)
			}
		}
	}
	sort.Strings(stale)
	for _, e := range stale {
		t.Errorf("allowlist entry %s: no longer imported, remove it", e)
	}
}

// TestUnit pins how packages map to the units the rules are stated in.
func TestUnit(t *testing.T) {
	for pkg, want := range map[string]string{
		"ledger":             "ledger",
		"llm/llmtest":        "llm",
		"platform/httpapi":   "platform/httpapi",
		"app/apitest":        "app",
		"delivery/sapconfig": "delivery/sapconfig",
	} {
		t.Run(pkg, func(t *testing.T) {
			if got := unit(pkg); got != want {
				t.Errorf("unit(%q): got %q, want %q", pkg, got, want)
			}
		})
	}
}
