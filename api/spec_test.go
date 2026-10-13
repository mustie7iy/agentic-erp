package api

// Contract rules from docs/design/04-api-design.md, checked on openapi.yaml.
// Linting (structure, examples against schemas) is vacuum's job; these tests
// cover the platform-specific rules a generic linter cannot know.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/db"
	"gopkg.in/yaml.v3"
)

type node = map[string]any

// operation is one method on one path, with its parameters resolved.
type operation struct {
	path, method string
	op           node
	params       []node
}

func (o operation) String() string { return strings.ToUpper(o.method) + " " + o.path }

// hasHeader reports whether the operation declares the header as a required parameter.
func (o operation) hasHeader(name string) bool {
	for _, p := range o.params {
		if p["in"] == "header" && strings.EqualFold(fmt.Sprint(p["name"]), name) && p["required"] == true {
			return true
		}
	}
	return false
}

func loadSpec(t *testing.T) node {
	t.Helper()
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec node
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	return spec
}

// resolve follows a local $ref ("#/components/...") to its target.
func resolve(t *testing.T, spec node, n node) node {
	t.Helper()
	for {
		ref, ok := n["$ref"].(string)
		if !ok {
			return n
		}
		var cur any = spec
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := cur.(node)
			if !ok {
				t.Fatalf("unresolvable $ref %s", ref)
			}
			cur = m[part]
		}
		target, ok := cur.(node)
		if !ok {
			t.Fatalf("unresolvable $ref %s", ref)
		}
		n = target
	}
}

func operations(t *testing.T, spec node) []operation {
	t.Helper()
	paths, _ := spec["paths"].(node)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ops []operation
	for _, path := range keys {
		item := paths[path].(node)
		var common []node
		if ps, ok := item["parameters"].([]any); ok {
			for _, p := range ps {
				common = append(common, resolve(t, spec, p.(node)))
			}
		}
		for method, v := range item {
			if method == "parameters" || method == "summary" || method == "description" || method == "servers" {
				continue
			}
			op := v.(node)
			o := operation{path: path, method: method, op: op, params: append([]node(nil), common...)}
			if ps, ok := op["parameters"].([]any); ok {
				for _, p := range ps {
					o.params = append(o.params, resolve(t, spec, p.(node)))
				}
			}
			ops = append(ops, o)
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
	return ops
}

// seededPermissions parses the ref.permission seed: code -> is_approval.
func seededPermissions(t *testing.T) map[string]bool {
	t.Helper()
	stmt := regexp.MustCompile(`(?is)INSERT\s+INTO\s+ref\.permission\s*\([^)]*\)\s*VALUES(.*?);`)
	row := regexp.MustCompile(`\(\s*'([a-z_]+(?:\.[a-z_]+)+)'\s*,\s*'(?:[^']|'')*'\s*,\s*(true|false)\s*\)`)
	comment := regexp.MustCompile(`--[^\n]*`)
	perms := map[string]bool{}
	for _, m := range db.Migrations() {
		for _, s := range stmt.FindAllStringSubmatch(comment.ReplaceAllString(m.Up, ""), -1) {
			for _, r := range row.FindAllStringSubmatch(s[1], -1) {
				perms[r[1]] = r[2] == "true"
			}
		}
	}
	if len(perms) == 0 {
		t.Fatal("no ref.permission seed found in the migrations")
	}
	return perms
}

var (
	// Paths carry the major version; servers do not. Segments are kebab-case
	// nouns or {params}; custom methods (AIP-136) end in one :verb.
	pathPattern     = regexp.MustCompile(`^/v1(/([a-z0-9]+(-[a-z0-9]+)*|\{[a-z_]+\}))+(:[a-z][a-zA-Z]*)?$`)
	operationIDForm = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	snakeCase       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// Approval decisions: POST /v1/approval-requests/{id}:approve|:reject.
	approvalDecision = regexp.MustCompile(`^/v1/approval-requests/\{id\}:(approve|reject)$`)
)

func hasTag(tags []any, want string) bool { return containsValue(tags, want) }

func containsValue(list []any, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// instanceMethod: POST /v1/things/{id}:verb. collectionMethod: POST /v1/things:verb.
func instanceMethod(path string) bool   { return strings.Contains(path, "}:") }
func collectionMethod(path string) bool { return strings.Contains(path, ":") && !instanceMethod(path) }

func TestDocument(t *testing.T) {
	spec := loadSpec(t)
	if v, _ := spec["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x", v)
	}
	info, _ := spec["info"].(node)
	if info["version"] != "1.0.0" {
		t.Errorf("info.version: got %v, want 1.0.0", info["version"])
	}
	if info["title"] == nil {
		t.Error("info.title: got none, want a title")
	}
	if desc := fmt.Sprint(info["description"]); !strings.Contains(desc, "04-api-design.md") {
		t.Errorf("info.description: got %q, want it to point to 04-api-design.md", desc)
	}
	servers, _ := spec["servers"].([]any)
	if len(servers) == 0 {
		t.Fatal("no servers")
	}
	for _, s := range servers {
		srv := s.(node)
		url := fmt.Sprint(srv["url"])
		vars, _ := srv["variables"].(node)
		if !strings.Contains(url, "{cell}") || vars["cell"] == nil {
			t.Errorf("server %s must use a {cell} variable", url)
		}
		if strings.Contains(url, "/v1") {
			t.Errorf("server %s must not carry the version; paths start with /v1/", url)
		}
	}
	security, _ := spec["security"].([]any)
	schemes, _ := spec["components"].(node)["securitySchemes"].(node)
	if len(security) == 0 {
		t.Error("security: got no global security requirement, want one")
	}
	if len(schemes) == 0 {
		t.Error("securitySchemes: got none, want a security scheme")
	}
	for _, req := range security {
		for name := range req.(node) {
			if s, ok := schemes[name].(node); !ok || s["scheme"] != "bearer" && s["type"] != "oauth2" && s["type"] != "openIdConnect" {
				t.Errorf("security scheme %s must be an OAuth2/OIDC bearer scheme", name)
			}
		}
	}
}

func TestOperations(t *testing.T) {
	spec := loadSpec(t)
	perms := seededPermissions(t)
	tags := map[string]bool{}
	for _, tg := range spec["tags"].([]any) {
		tags[fmt.Sprint(tg.(node)["name"])] = true
	}
	ops := operations(t, spec)
	if len(ops) == 0 {
		t.Fatal("no operations")
	}
	ids := map[string]string{}
	for _, o := range ops {
		if !pathPattern.MatchString(o.path) {
			t.Errorf("%s: path must be /v1/ plus kebab-case segments or {params}, optionally ending in :verb", o)
		}
		switch o.method {
		case "get", "post", "patch":
		default:
			t.Errorf("%s: method not allowed (no PUT or DELETE; drafts are cancelled, master data blocked)", o)
		}
		if strings.Contains(o.path, ":") && o.method != "post" {
			t.Errorf("%s: custom methods are POST", o)
		}

		id, _ := o.op["operationId"].(string)
		if !operationIDForm.MatchString(id) {
			t.Errorf("%s: operationId %q must be camelCase", o, id)
		} else if prev, dup := ids[id]; dup {
			t.Errorf("%s: operationId %s already used by %s", o, id, prev)
		}
		ids[id] = o.String()
		opTags, _ := o.op["tags"].([]any)
		if len(opTags) == 0 {
			t.Errorf("%s: no tags", o)
		}
		for _, tg := range opTags {
			if !tags[fmt.Sprint(tg)] {
				t.Errorf("%s: tag %v is not declared", o, tg)
			}
		}
		if s, _ := o.op["summary"].(string); s == "" {
			t.Errorf("%s: no summary", o)
		}

		perm, _ := o.op["x-permission"].(string)
		approval, seeded := perms[perm]
		switch {
		case perm == "":
			t.Errorf("%s: no x-permission", o)
		case !seeded:
			t.Errorf("%s: x-permission %s is not seeded in ref.permission", o, perm)
		}
		agent, hasAgent := o.op["x-agent"]
		if hasAgent && agent != false {
			t.Errorf("%s: x-agent may only be false (omit it otherwise), got %v", o, agent)
		}
		// Approval decisions are made only on approval requests (04 §4.9); the
		// objects they govern move on when their request completes.
		decision := approvalDecision.MatchString(o.path)
		if (strings.HasSuffix(o.path, ":approve") || strings.HasSuffix(o.path, ":reject")) &&
			!decision && !strings.HasPrefix(o.path, "/v1/payment-runs/") {
			t.Errorf("%s: approve/reject belong on /v1/approval-requests/{id} only (payment-run release is the one direct human transition)", o)
		}
		humanOnly := approval || decision || (o.method != "get" && hasTag(opTags, "identity"))
		if humanOnly && agent != false {
			t.Errorf("%s: approval decisions, approval permissions and identity administration need x-agent: false", o)
		}
		// x-object-permissions maps object types to the approval permission the
		// server requires in addition to x-permission on approval decisions.
		objPerms, hasObjPerms := o.op["x-object-permissions"].(node)
		if decision && len(objPerms) == 0 {
			t.Errorf("%s: approval decisions must list x-object-permissions", o)
		}
		if _, present := o.op["x-object-permissions"]; present && !hasObjPerms {
			t.Errorf("%s: x-object-permissions must map object types to permission codes", o)
		}
		for objectType, code := range objPerms {
			if isApproval, ok := perms[fmt.Sprint(code)]; !ok || !isApproval {
				t.Errorf("%s: x-object-permissions[%s] = %v must be a seeded approval permission (is_approval = true)", o, objectType, code)
			}
		}
		if o.path == "/v1/approval-requests/{id}:cancel" && hasAgent {
			t.Errorf("%s: requesters, agents included, may cancel their own approval requests", o)
		}

		if o.method == "post" || o.method == "patch" {
			if !o.hasHeader("Idempotency-Key") {
				t.Errorf("%s: Idempotency-Key header is required on every POST and PATCH", o)
			}
		}
		if o.method == "patch" || instanceMethod(o.path) {
			if !o.hasHeader("If-Match") {
				t.Errorf("%s: If-Match header is required on PATCH and custom methods", o)
			}
		}
		if collectionMethod(o.path) {
			if o.hasHeader("If-Match") {
				t.Errorf("%s: collection custom methods start new work; If-Match does not apply", o)
			}
			if _, ok := o.op["responses"].(node)["202"]; !ok {
				t.Errorf("%s: collection custom methods return 202 with an Operation", o)
			}
		}
		if o.method == "patch" {
			body := resolve(t, spec, o.op["requestBody"].(node))
			content := body["content"].(node)
			if _, ok := content["application/merge-patch+json"]; !ok || len(content) != 1 {
				t.Errorf("%s: PATCH bodies are application/merge-patch+json only", o)
			}
		}

		problem4xx := false
		for code, r := range o.op["responses"].(node) {
			resp := resolve(t, spec, r.(node))
			content, _ := resp["content"].(node)
			if strings.HasPrefix(code, "4") {
				if pj, ok := content["application/problem+json"].(node); ok && pj["schema"].(node)["$ref"] == "#/components/schemas/Problem" {
					problem4xx = true
				}
			}
			if code == "202" {
				if c, ok := content["application/json"].(node); !ok || c["schema"].(node)["$ref"] != "#/components/schemas/Operation" {
					t.Errorf("%s: 202 responses carry an Operation", o)
				}
			}
		}
		if !problem4xx {
			t.Errorf("%s: no 4xx application/problem+json response", o)
		}
	}
}

// walk visits every map in the document with its JSON-pointer-like location.
func walk(at string, v any, visit func(at string, n node)) {
	switch x := v.(type) {
	case node:
		visit(at, x)
		for k, c := range x {
			walk(at+"/"+k, c, visit)
		}
	case []any:
		for i, c := range x {
			walk(fmt.Sprintf("%s/%d", at, i), c, visit)
		}
	}
}

func TestRepresentations(t *testing.T) {
	spec := loadSpec(t)
	walk("#", spec, func(at string, n node) {
		if ref, ok := n["$ref"].(string); ok {
			resolve(t, spec, n) // fails the test on a dangling reference
			if !strings.HasPrefix(ref, "#/components/") {
				t.Errorf("%s: only local component references are allowed, got %s", at, ref)
			}
		}
		// Enumerations are open (04 §4.4): listed with x-extensible-enum, never a closed enum.
		if _, ok := n["enum"]; ok && !strings.Contains(at, "/examples/") {
			t.Errorf("%s: use x-extensible-enum, not enum", at)
		}
		if strings.HasSuffix(at, "/properties") && !strings.Contains(at, "/examples/") {
			for name := range n {
				if !snakeCase.MatchString(name) {
					t.Errorf("%s: property %s is not snake_case", at, name)
				}
			}
		}
		// Money, quantities, rates and percentages are decimal strings; JSON integers are for counts only.
		types, _ := n["type"].([]any)
		if n["type"] == "number" || containsValue(types, "number") {
			t.Errorf("%s: type number is not allowed; use Money, Quantity or Decimal (integers only for counts)", at)
		}
	})
	params := spec["components"].(node)["parameters"].(node)
	for name, p := range params {
		if pn := p.(node); pn["in"] == "query" && !snakeCase.MatchString(fmt.Sprint(pn["name"])) {
			t.Errorf("parameter %s: query parameter names are snake_case", name)
		}
	}
	schemas := spec["components"].(node)["schemas"].(node)
	money := schemas["Money"].(node)["properties"].(node)
	if typ := money["amount"].(node)["type"]; typ != "string" {
		t.Errorf("Money.amount type: got %v, want string (a decimal string)", typ)
	}
	if money["currency"] == nil {
		t.Error("Money.currency: got none, want a currency")
	}
	for _, name := range []string{"Problem", "FieldError", "Operation", "AuditFields", "Extensions", "Quantity"} {
		if schemas[name] == nil {
			t.Errorf("component schema %s is missing", name)
		}
	}
	responses := spec["components"].(node)["responses"].(node)
	for _, name := range []string{"BadRequest", "Unauthorized", "Forbidden", "NotFound", "Conflict", "PreconditionFailed", "Unprocessable", "PreconditionRequired", "TooManyRequests"} {
		r, ok := responses[name].(node)
		if !ok {
			t.Errorf("component response %s is missing", name)
			continue
		}
		if _, ok := r["content"].(node)["application/problem+json"]; !ok {
			t.Errorf("component response %s must be application/problem+json", name)
		}
	}
}
