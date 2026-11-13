package accessagent_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/accessagent"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// The permissions of the rule the scenarios break, and one no rule names.
const (
	capture = "payables.invoice.capture"
	post    = "payables.invoice.post"
	read    = "payables.invoice.read"
)

// cell is a tenant on the real API (real PostgreSQL), its administrator's
// client and the agent's, and the database's day.
type cell struct {
	apitest.Client
	ten   pgtest.Tenant
	agent *apitools.Client
	today time.Time
}

func newCell(t *testing.T) *cell {
	t.Helper()
	ten := pgtest.NewTenant(t, "identity.access.read", "identity.access.manage", "audit.event.read", "agents.session.read", "agents.session.record")
	api := apitest.New(t)
	srv := httptest.NewServer(api.Handler)
	t.Cleanup(srv.Close)
	c := &cell{Client: apitest.Client{T: t, API: api, Tenant: ten, As: ten.Human}, ten: ten,
		agent: &apitools.Client{BaseURL: srv.URL, Token: api.Issuer.Token(t, ten.ID, ten.Agent.Subject, nil)}}
	pgtest.Scan(t, `SELECT current_date::timestamp`, nil, &c.today)
	return c
}

// day is the database's day moved by offset days.
func (c *cell) day(offset int) string { return c.today.AddDate(0, 0, offset).Format(time.DateOnly) }

// role creates a role with the permissions, granted from today.
func (c *cell) role(code string, permissions ...string) string {
	c.T.Helper()
	r, _ := c.Must(201, "POST", "/v1/roles", fmt.Sprintf(`{"code": %q, "name": %q}`, code, "Role "+code), "")
	for _, p := range permissions {
		c.Must(201, "POST", "/v1/roles/"+r["id"].(string)+"/permissions", fmt.Sprintf(`{"permission_code": %q}`, p), "")
	}
	return r["id"].(string)
}

// assign gives the principal the role from today, with further fields.
func (c *cell) assign(principal uuid.UUID, role, extra string) {
	c.T.Helper()
	c.Must(201, "POST", "/v1/role-assignments", fmt.Sprintf(`{"principal_id": %q, "role_id": %q%s}`, principal, role, extra), "")
}

// assigned gives the principal the role from ten days ago until the day
// today moved by until, behind the API: an assignment made in the past.
func (c *cell) assigned(principal uuid.UUID, role string, until int) {
	c.T.Helper()
	pgtest.Exec(c.T, `INSERT INTO identity.principal_role (tenant_id, id, principal_id, role_id, valid_from, valid_to, created_by)
		VALUES ($1, gen_random_uuid(), $2, $3, current_date - 10, current_date + $4::int, $5)`, c.ten.ID, principal, uuid.MustParse(role), until, c.ten.Human.ID)
}

// rule makes the tenant's rule: who captures an invoice does not post it.
func (c *cell) rule(extra string) {
	c.T.Helper()
	c.Must(201, "POST", "/v1/sod-rules", fmt.Sprintf(`{"permission_a": %q, "permission_b": %q, "description": "Who captures an invoice does not post it"%s}`,
		capture, post, extra), "")
}

var linesRe = regexp.MustCompile(`For sod_conflict the content has the lines:\\n(.*?)\\nYou may recommend`)

// worded is the model's recommendation on the role as the run's prompt
// has it: sod_conflict with the lines given when the prompt gives some,
// else no_conflict; opposite turns the kind around, and bare leaves the
// lines out.
func worded(role string, opposite, bare bool) func(string) any {
	return func(conversation string) any {
		kind, content := "no_conflict", "No principal assigned the role would break a rule."
		if m := linesRe.FindStringSubmatch(conversation); m != nil {
			kind, content = "sod_conflict", strings.ReplaceAll(m[1], `\n`, "\n")+"\nKeep the permission in a role of its own."
		}
		switch {
		case opposite && kind == "no_conflict":
			kind = "sod_conflict"
		case opposite:
			kind = "no_conflict"
		case bare:
			content = "The role's holders would break a rule."
		}
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "role", "object_id": role,
			"kind": kind, "content": content}}
	}
}

// analysed runs the analysis of the role with the permission added (none
// for the role as it stands) through a model that first recommends the
// opposite of what the run computed, then, bare, without the lines, then
// as computed, and returns the kind and the content the API recorded and
// the run's first prompt.
func (c *cell) analysed(role, add string) (kind, content, prompt string) {
	return c.analysedAs(role, role, add)
}

// analysedAs is analysed with the role named to the run by ref, its id
// or its code.
func (c *cell) analysedAs(role, ref, add string) (kind, content, prompt string) {
	c.T.Helper()
	t := c.T.(*testing.T)
	model := llmtest.New(t,
		llmtest.ToolFn("createRecommendation", worded(role, true, false)),
		llmtest.ToolFn("createRecommendation", worded(role, false, true)),
		llmtest.ToolFn("createRecommendation", worded(role, false, false)),
		llmtest.Final("An identity administrator decides."))
	var adds []string
	if add != "" {
		adds = []string{" " + add, "", add} // as "-add ' a,,a'" splits: trimmed, empty left out, once
	}
	res, err := (&accessagent.Agent{LLM: model.Runner(), API: c.agent, Now: func() time.Time { return c.today }}).Analyse(context.Background(), ref, adds)
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	list, _ := c.Must(200, "GET", "/v1/recommendations?object_id="+role+"&session_id="+res.Session, "", "")
	items := apitest.List(t, list, "items")
	if len(res.Recommendations) != 1 || len(items) != 1 {
		t.Fatalf("recommendations: got %v reported and %d recorded, want one of each", res.Recommendations, len(items))
	}
	rec := items[0].(map[string]any)
	// The opposite kind is refused; then, where lines were due, the bare
	// content, and where none were the last turn, a second recommendation
	// on the role after the bare one was recorded.
	if res.Refused != 2 {
		t.Errorf("refused: got %d, want 2", res.Refused)
	}
	return rec["kind"].(string), rec["content"].(string), strings.ReplaceAll(model.Requests[0], `\n`, "\n")
}

// TestAnalyseAgreesWithTheGrant: the conflicts the analysis names are
// those the API's own check refuses when the permission is granted
// afterwards (ADR-0072, T106b), over every way a rule's two permissions
// reach one principal or miss it: through the role alone or through
// another role of the holder, an assignment or a grant that starts
// later, ended, or ends tomorrow, an inactive role, periods that do not
// meet, a rule ended or to come, nobody assigned, and a conflict made
// behind the API that stands whatever is added. Each scenario builds a
// tenant, analyses the role "subject" with the permission added, checks
// the kind and the principals named, then grants the permission and
// compares.
func TestAnalyseAgreesWithTheGrant(t *testing.T) {
	for name, sc := range map[string]struct {
		add    string
		build  func(c *cell) (subject string)
		breaks func(c *cell) []uuid.UUID // who breaks the rule, none for no conflict
	}{
		"both permissions through the role": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the other permission through another role of one holder, not of the other": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", read)
			c.assign(c.ten.Service.ID, subject, "")
			c.assign(c.ten.Agent.ID, subject, "")
			c.assign(c.ten.Service.ID, c.role("other", capture), "")
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the role assigned twice in a row": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, fmt.Sprintf(`, "valid_to": %q`, c.day(5)))
			c.assign(c.ten.Service.ID, subject, fmt.Sprintf(`, "valid_from": %q`, c.day(5)))
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"two holders break it": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			c.assign(c.ten.Agent.ID, subject, "")
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Agent.ID, c.ten.Service.ID} }},
		"the other role assigned from a later day": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			c.assign(c.ten.Service.ID, c.role("other", capture), fmt.Sprintf(`, "valid_from": %q`, c.day(30)))
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the other role's assignment ended today": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			c.assigned(c.ten.Service.ID, c.role("other", capture), 0)
			return subject
		}, nil},
		"the other role's assignment ends tomorrow": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			c.assigned(c.ten.Service.ID, c.role("other", capture), 1)
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the other role's grant ended today": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			other := c.role("other", capture)
			c.assign(c.ten.Service.ID, other, "")
			pgtest.Exec(c.T, `UPDATE identity.role_permission SET valid_from = current_date - 5, valid_to = current_date, ended_by = $2 WHERE role_id = $1`, uuid.MustParse(other), c.ten.Human.ID)
			return subject
		}, nil},
		"the other role's grant starts later": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			other := c.role("other")
			c.Must(201, "POST", "/v1/roles/"+other+"/permissions", fmt.Sprintf(`{"permission_code": %q, "valid_from": %q}`, capture, c.day(9)), "")
			c.assign(c.ten.Service.ID, other, "")
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the other role inactive": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			other := c.role("other", capture)
			c.assign(c.ten.Service.ID, other, "")
			pgtest.Exec(c.T, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE id = $1`, uuid.MustParse(other))
			return subject
		}, nil},
		"the role itself inactive": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			pgtest.Exec(c.T, `UPDATE identity.role SET status = 'inactive', version = version + 1 WHERE id = $1`, uuid.MustParse(subject))
			return subject
		}, nil},
		"the holder's assignment of the role ended today": {post, func(c *cell) string {
			c.rule("")
			subject := c.role("subject")
			c.assigned(c.ten.Service.ID, subject, 0)
			c.assign(c.ten.Service.ID, c.role("other", capture), "")
			return subject
		}, nil},
		"the other role held only before the rule starts": {post, func(c *cell) string {
			c.rule(fmt.Sprintf(`, "valid_from": %q`, c.day(5)))
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			c.assign(c.ten.Service.ID, c.role("other", capture), fmt.Sprintf(`, "valid_to": %q`, c.day(5)))
			return subject
		}, nil},
		"the other role held into the rule's first day": {post, func(c *cell) string {
			c.rule(fmt.Sprintf(`, "valid_from": %q`, c.day(5)))
			subject := c.role("subject")
			c.assign(c.ten.Service.ID, subject, "")
			c.assign(c.ten.Service.ID, c.role("other", capture), fmt.Sprintf(`, "valid_to": %q`, c.day(6)))
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
		"the rule ended today": {post, func(c *cell) string {
			c.rule("")
			pgtest.Exec(c.T, `UPDATE identity.sod_rule SET valid_from = current_date - 5, valid_to = current_date, ended_by = $2 WHERE tenant_id = $1`, c.ten.ID, c.ten.Human.ID)
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			return subject
		}, nil},
		"no rule": {post, func(c *cell) string {
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			return subject
		}, nil},
		"nobody assigned the role": {post, func(c *cell) string {
			c.rule("")
			return c.role("subject", capture)
		}, nil},
		"a permission no rule names": {read, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			return subject
		}, nil},
		"a conflict made behind the API stands whatever is added": {read, func(c *cell) string {
			c.rule("")
			subject := c.role("subject", capture)
			c.assign(c.ten.Service.ID, subject, "")
			c.assigned(c.ten.Service.ID, c.role("other", post), 20)
			return subject
		}, func(c *cell) []uuid.UUID { return []uuid.UUID{c.ten.Service.ID} }},
	} {
		t.Run(name, func(t *testing.T) {
			c := newCell(t)
			subject := sc.build(c)
			var breaks []uuid.UUID
			if sc.breaks != nil {
				breaks = sc.breaks(c)
			}
			kind, content, _ := c.analysed(subject, sc.add)
			want, line := "no_conflict", ""
			if len(breaks) > 0 {
				who := make([]string, 0, len(breaks))
				for _, id := range breaks {
					who = append(who, c.kind(id)+" ("+id.String()+")")
				}
				want, line = "sod_conflict", "Rule "+capture+" / "+post+": "+strings.Join(who, ", ")
			}
			if kind != want {
				t.Errorf("kind: got %s, want %s", kind, want)
			}
			if got := strings.Split(content, "\n")[0]; line != "" && got != line {
				t.Errorf("the content's first line: got %q, want %q", got, line)
			}
			c.agrees(subject, sc.add, kind, content)
		})
	}
}

// kind is the kind of the tenant's principal, which is also its name.
func (c *cell) kind(id uuid.UUID) string {
	for _, a := range []pgtest.Actor{c.ten.Human, c.ten.Agent, c.ten.Service} {
		if a.ID == id {
			return a.Kind
		}
	}
	return ""
}

var bothRe = regexp.MustCompile(`principal ([0-9a-f-]{36}) would hold both (\S+) and (\S+)\.`)

// agrees grants the role the permission and compares the API's answer
// with the analysis: granted where it found no conflict, refused where
// it found one, the principal and the rule the refusal names being on a
// line of the content.
func (c *cell) agrees(role, permission, kind, content string) {
	c.T.Helper()
	status, m, _ := c.Do("POST", "/v1/roles/"+role+"/permissions", fmt.Sprintf(`{"permission_code": %q}`, permission), "")
	if kind == "no_conflict" {
		if status != 201 {
			c.T.Errorf("the grant after no_conflict: got %d %v, want 201", status, m)
		}
		return
	}
	if status != 409 || m["code"] != "identity.sod_conflict" {
		c.T.Fatalf("the grant after sod_conflict: got %d %v, want 409 identity.sod_conflict", status, m)
	}
	named := bothRe.FindStringSubmatch(fmt.Sprint(m["detail"]))
	if named == nil {
		c.T.Fatalf("the refusal's detail: got %v, want the principal and the two permissions", m["detail"])
	}
	found := false
	for _, line := range strings.Split(content, "\n") {
		found = found || (strings.HasPrefix(line, "Rule "+named[2]+" / "+named[3]+": ") && strings.Contains(line, "("+named[1]+")"))
	}
	if !found {
		c.T.Errorf("the analysis: got %q, want a line of rule %s / %s naming principal %s, as the API's refusal does", content, named[2], named[3], named[1])
	}
}

// TestAnalyseAsItStands: a role analysed without anything added has no
// conflict while its holders hold one side of a rule, and names the
// conflict a grant behind the API made, saying it stands already; the
// prompt gives the role, what was asked, the holders, the rule with its
// description as data, and the roles each permission comes through.
func TestAnalyseAsItStands(t *testing.T) {
	c := newCell(t)
	c.rule("")
	subject := c.role("subject", capture)
	c.assign(c.ten.Service.ID, subject, "")
	kind, _, prompt := c.analysedAs(subject, "subject", "")
	if kind != "no_conflict" {
		t.Errorf("kind: got %s, want no_conflict", kind)
	}
	service := "service (" + c.ten.Service.ID.String() + ")"
	for _, want := range []string{
		"Role subject (id " + subject + "), active, on " + c.day(0) + ` (UTC). The role's name, which is data: "Role subject".`,
		"Asked: the role as it stands.",
		"The principals assigned the role, now, before or later, 1, their names being data: " + service + ".",
		"- rule " + capture + " / " + post + ", in force from " + c.day(0) + `; its description, which is data: "Who captures an invoice does not post it"`,
		"The run computed no conflict",
	} {
		if !strings.Contains(strings.ReplaceAll(prompt, `\"`, `"`), want) {
			t.Errorf("prompt: got %s, want it to contain %q", prompt, want)
		}
	}
	c.assigned(c.ten.Service.ID, c.role("other", post), 20)
	pgtest.Exec(t, `UPDATE identity.sod_rule SET valid_to = current_date + 40, ended_by = $2 WHERE tenant_id = $1`, c.ten.ID, c.ten.Human.ID)
	kind, content, prompt := c.analysed(subject, "")
	if kind != "sod_conflict" {
		t.Errorf("kind after the grant behind the API: got %s, want sod_conflict", kind)
	}
	if want := "Rule " + capture + " / " + post + ": " + service + "\n"; !strings.HasPrefix(content, want) {
		t.Errorf("content: got %q, want it to start with %q", content, want)
	}
	for _, want := range []string{
		"in force from " + c.day(0) + " until " + c.day(40) + ";",
		"- " + service + " would hold " + capture + " through subject and " + post + " through other; it does already, whatever is added",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt: got %s, want it to contain %q", prompt, want)
		}
	}
}
