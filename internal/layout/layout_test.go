package layout

import (
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/mustie7iy/agentic-erp/internal/"

// shared are packages every context may import: the shared kernel (§6.7):
// money, and tax, the pure tax calculator (ADR-0049 §2, §8), a library of
// rules on amounts that the ledger and payables (T61d) call, which holds
// no state and owns no data. A shared package imports only shared ones.
var shared = map[string]bool{"money": true, "tax": true}

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
	"platform/sftpclient/sftptest/sftpstub":    true, // the walkthrough's stand-in SFTP server, built from the test double
	"platform/natsbus/natstest/natsstub":       true, // the walkthrough's event bus, the tests' embedded JetStream
	"platform/webhook/webhooktest/webhookstub": true, // the walkthrough's webhook subscriber
	"platform/secrets/vaulttest":               true, "platform/workflow/workflowtest": true,
	"llm/llmtest": true, "nfr": true,
	"einvoicing/einvoicetest":              true, // the e-invoicing provider's stand-in (ADR-0061 §3)
	"einvoicing/einvoicetest/providerstub": true, // the walkthrough's provider, built from the stand-in
}

// webPlatform are the platform packages the web workspace (internal/web
// and cmd/erp-web) may import: none holds a database connection, a unit of
// work or a tenant's scope. The workspace reaches business data only
// through the public API's client (api/gen) as the signed-in person
// (ADR-0052 §7), so it can never become a second backend.
var webPlatform = map[string]bool{
	"platform/authn": true, "platform/buildinfo": true, "platform/clock": true, "platform/etag": true, "platform/problem": true,
	"platform/proxyproto": true, "platform/secrets": true, "platform/telemetry": true, "platform/tlsconf": true, "platform/worker": true,
	"platform/database": true, // web/session's alone: webStore
	"platform/rdsiam":   true, // signs the directory login's RDS IAM token; holds no connection (ADR-0035)
}

// webStore is the one package of the workspace that holds a database pool:
// its session store, on the directory database's web schema, as the
// erp_web role that sees that schema alone (ADR-0052 §6, T73b). The pool
// (platform/database, pgxpool) may be linked through it and nothing else.
const webStore = "web/session"

// webPool are what only webStore (and the pool's own package) may import.
var webPool = map[string]string{module + "platform/database": "web/session", "github.com/jackc/pgx/v5/pgxpool": "platform/database"}

// webForbidden are packages the workspace may not link even through
// another package: they hold a tenant's scope or write tenants' data.
// platform/reqctx (and with it pgx's transaction type) is linked, through
// telemetry and authn, which keep a request's identity in it (ADR-0052,
// Built in T73a).
var webForbidden = []string{"platform/uow", "platform/outbox", "platform/audit", "platform/fieldcrypt", "platform/workflow", "platform/config"}

// allowed is every import from one context to another that production code
// makes today, by importing unit. A new one fails TestImportRules; one that
// is no longer made fails it too, so the list stays exact.
var allowed = map[string][]string{
	"agent":                   {"apitools", "llm"},
	"analytics":               {"apitools", "config", "configuration"},
	"apitools":                {"llm"},
	"app":                     {"countrypack"}, // apitest releases a test's own country packs
	"approvals":               {"config"},
	"assets":                  {"config"}, // a leaf (ADR-0057 §2): configuration and tenancy through ports the composition root wires
	"audit":                   {"operations"},
	"capture":                 {"llm"},
	"cash":                    {"config"}, // a leaf (ADR-0051 §2): the ledger, payments, payables and procurement through ports the composition root wires
	"closeagent":              {"apitools", "llm"},
	"closing":                 {"config"},                                 // a leaf (ADR-0058 §2): configuration, tenancy, the ledger and controlling through ports the composition root wires
	"collections":             {"config", "ledger", "partner", "tenancy"}, // a leaf (ADR-0065 §1): the ledger's customer open items, partners' names, tenancy and configuration through its ports, by the types their published APIs give
	"config":                  {"bankfile"},
	"configuration":           {"approvals", "config", "countrypack", "operations"},
	"consolidation":           {"config"},                       // a leaf (ADR-0059 §2): configuration and tenancy (the ledger and identity in later slices) through ports the composition root wires
	"credit":                  {"ledger", "partner", "tenancy"}, // a leaf (ADR-0066 §1): the ledger's customer open items, partners' customers and names and tenancy through its ports, by the types their published APIs give
	"controlling":             {"config", "ledger", "tenancy"},  // plan values over the ledger's centers and actuals, through ledger.AssignmentBalances (ADR-0050 §2); config: a company's date in its time zone (config.DocumentDay over controlling.Settings, ADR-0064 §3)
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
	"ledger":                  {"approvals", "config", "configuration", "tenancy"},
	"nfr":                     {"analytics", "app", "ledger"},
	"offboarding":             {"tenancy"},
	"partner":                 {"approvals", "bankfile", "config", "tenancy"},
	"payables":                {"approvals", "capture", "config", "configuration", "ledger", "operations", "partner", "procurement", "tenancy"},
	"payments":                {"approvals", "bankfile", "bankstatement", "config", "ledger", "operations", "partner", "tenancy"},
	"procurement":             {"approvals", "config", "ledger", "partner", "tenancy"},
	"provisioning":            {"config", "configuration", "identity", "tenancy"},
	"receivables":             {"config", "ledger", "partner", "tenancy"},                // as payables reads them (ADR-0063 §1)
	"sales":                   {"config", "ledger", "partner", "receivables", "tenancy"}, // as receivables reads them; receivables itself for billing, which posts through it (ADR-0067 §1, §9)
	"revenue":                 {"partner", "sales", "tenancy"},                           // a leaf (ADR-0068 §1): the sales context's orders (sales.Orders), partners' names and tenancy through its ports
	"reconagent":              {"apitools", "llm"},
	"cashagent":               {"apitools", "llm"},
	"router":                  {"directory"},
	"taxagent":                {"apitools", "llm"},
	// Registrations from the configuration, companies from the directory
	// (ADR-0049 §10); returns from the ledger's tax items through
	// ledger.TaxItems and the country packs' returns.yaml (§11, T61h);
	// their approval by another person through approvals (T61i).
	"taxfiling": {"approvals", "config", "countrypack", "ledger", "tenancy"},
	"workflows": {"approvals", "configuration", "integration", "ledger", "operations", "payments"}, // ledger: the recurring entries' activity (ADR-0056)
}

// leaves are contexts no other context may import, whatever allowed says:
// they read others through ports and nothing depends on them, so a new
// arrow into one turns a decided direction around (ADR-0051 §2). The
// composition root and commands wire them like any context, and a context
// may ask one a question through a port it declares itself, as partner
// asks cash about planning groups.
var leaves = map[string]bool{"assets": true, "cash": true, "closing": true, "collections": true, "consolidation": true, "credit": true, "einvoicing": true, "revenue": true}

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
// support, a shared package imports only shared ones, platform packages
// (other than platformUsers) import no context, the composition root and
// commands import any, a context imports no leaf and another context only
// where allowed says so.
func TestImportRules(t *testing.T) {
	used := map[string]bool{}
	for pkg, deps := range imports(t) {
		from := unit(pkg)
		for dep := range deps {
			to := unit(dep)
			if (from == "web" || pkg == "cmd/erp-web") && to != "web" && !shared[to] && !webPlatform[to] {
				t.Errorf("%s imports %s: the web workspace imports only its own packages, the shared kernel and %v (ADR-0052 §7)", pkg, dep, slices.Sorted(maps.Keys(webPlatform)))
				continue
			}
			if (from == "web" || pkg == "cmd/erp-web") && to == "platform/database" && pkg != webStore {
				t.Errorf("%s imports %s: in the workspace only its session store (%s) holds a database pool (ADR-0052 §6)", pkg, dep, webStore)
				continue
			}
			if testSupport[dep] && !testSupport[pkg] {
				t.Errorf("%s imports %s: production code may not import test support", pkg, dep)
				continue
			}
			switch {
			case shared[from] && from != to && !shared[to]:
				t.Errorf("%s imports %s: a shared package imports only shared ones", pkg, dep)
			case from == to, strings.HasPrefix(to, "platform/"), shared[to]:
			case strings.HasPrefix(from, "cmd/"), pkg == compositionRoot: // a command and the composition root wire contexts together
			case strings.HasPrefix(from, "platform/"):
				if !platformUsers[from] {
					t.Errorf("%s imports %s: a platform package may not depend on a context", pkg, dep)
				}
			case leaves[to]:
				t.Errorf("%s imports %s: %s is a leaf context, which nothing depends on (ADR-0051 §2); declare a port and let the composition root wire it", pkg, dep, to)
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

// TestWebLinksNoBackend: erp-web links no context and no package that
// writes tenants' data, not even indirectly (ADR-0052 §7).
func TestWebLinksNoBackend(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "../../cmd/erp-web")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		dep := fields[0]
		for _, imported := range fields[1:] {
			if only, pooled := webPool[imported]; pooled && dep != module+only {
				t.Errorf("erp-web: %s imports %s: only %s may, the workspace's one database pool (ADR-0052 §6)", dep, imported, only)
			}
		}
		pkg, ok := strings.CutPrefix(dep, module)
		if !ok {
			continue
		}
		n++
		to := unit(pkg)
		if !strings.HasPrefix(to, "platform/") && to != "web" && !shared[to] {
			t.Errorf("erp-web links %s: a context; the workspace reaches it only through the API (ADR-0052 §7)", pkg)
		}
		if slices.Contains(webForbidden, to) {
			t.Errorf("erp-web links %s, which holds a database pool or writes tenants' data (ADR-0052 §7)", pkg)
		}
	}
	if n < 3 {
		t.Fatalf("erp-web links %d internal packages: the check judged nothing", n)
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

// TestStaticBinaries: erp-server, erp-router, erp-web and erp build without cgo and
// link no DuckDB; erp-analytics alone carries the cgo exception (ADR-0032,
// amended by ADR-0044).
func TestStaticBinaries(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", "../../cmd/erp-server", "../../cmd/erp-router", "../../cmd/erp-web", "../../cmd/erp")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "duckdb") || strings.HasSuffix(dep, "/internal/analytics/reader") {
			t.Errorf("dependency %s: got it in a static binary, want it in erp-analytics alone", dep)
		}
	}
}
