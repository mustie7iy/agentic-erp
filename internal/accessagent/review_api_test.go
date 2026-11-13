package accessagent_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/accessagent"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
)

// reviewed is the model's recommendation on the principal as the run's
// prompt computed it: the kind and line given for it, then words; wrong
// turns the kind around, and number adds a figure the run did not read.
func reviewed(principal string, wrong, number bool) func(string) any {
	return func(conversation string) any {
		kind := regexp.MustCompile(`Kind for ` + principal + `: ([a-z_]+)`).FindStringSubmatch(conversation)[1]
		line := regexp.MustCompile(`Line for ` + principal + `: ([^\\]+)`).FindStringSubmatch(conversation)[1]
		content := line + "\nThe reasons, as the run found them."
		switch {
		case wrong && kind == "keep_roles":
			kind = "remove_roles"
		case wrong:
			kind = "keep_roles"
		case number:
			content += " Used 4242 times."
		}
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": "principal",
			"object_id": principal, "kind": kind, "content": content}}
	}
}

// TestPrepare: the access review of a tenant on the real API. The person
// (the tenant's human) made roles and assignments in the period through
// its role "all": kept. The agent holds "machine", whose permissions it
// did not use and some of which only reads need: review. The service
// holds "machine" too and three roles it never used, one of them given
// behind the API, which makes it break the rule that who captures an
// invoice does not post it: the removal of the two whose permissions only
// changes need, the rule named; the capture role is kept out of it, since
// a read (getUpload) needs its permission and so its use is not recorded. A blocked
// principal is not reviewed. A wrong kind, a line not computed and a
// number not read are refused; one recommendation per principal; a
// second run of the period starts no session and adds none.
func TestPrepare(t *testing.T) {
	c := newCell(t)
	c.rule("")
	c.assign(c.ten.Service.ID, c.role("taxer", "tax.return.prepare"), "")
	c.assign(c.ten.Service.ID, c.role("capt", capture), "")
	c.assigned(c.ten.Service.ID, c.role("poster", post), 20)
	blocked, _ := c.Must(201, "POST", "/v1/principals", `{"kind": "human", "display_name": "Olga Gone", "external_subject": "olga"}`, "")
	pgtest.Exec(t, `UPDATE identity.principal SET status = 'blocked', version = version + 1 WHERE id = $1`, uuid.MustParse(blocked["id"].(string)))
	period := accessagent.Period{From: c.day(-1), To: c.day(1)}
	human, agent, service := c.ten.Human.ID.String(), c.ten.Agent.ID.String(), c.ten.Service.ID.String()
	model := llmtest.New(t,
		llmtest.Tool("listSodRules", map[string]any{}),
		llmtest.ToolFn("createRecommendation", reviewed(human, true, false)),
		llmtest.ToolFn("createRecommendation", reviewed(human, false, true)),
		llmtest.ToolFn("createRecommendation", func(conversation string) any {
			in := reviewed(human, false, false)(conversation).(map[string]any)
			body := in["body"].(map[string]any)
			body["content"] = "Access review: keep\n" + body["content"].(string)
			return in
		}),
		llmtest.ToolFn("createRecommendation", reviewed(human, false, false)),
		llmtest.ToolFn("createRecommendation", reviewed(agent, false, false)),
		llmtest.ToolFn("createRecommendation", reviewed(service, false, false)),
		llmtest.Final("The review is prepared; the reviewer decides."))
	a := &accessagent.Agent{LLM: model.Runner(), API: c.agent, Now: func() time.Time { return c.today }}
	res, err := a.Prepare(context.Background(), period)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if len(res.Recommendations) != 3 || res.Refused != 3 {
		t.Errorf("recommendations: got %v and %d refused, want three and three refused", res.Recommendations, res.Refused)
	}
	heading := "Access review " + period.From + ".." + period.To + ": "
	for who, want := range map[string]struct{ kind, line string }{
		human:   {"keep_roles", heading + "keep all"},
		agent:   {"review", heading + "review machine"},
		service: {"remove_roles", heading + "remove poster, taxer; segregation of duties: " + capture + " / " + post},
	} {
		list, _ := c.Must(200, "GET", "/v1/recommendations?object_id="+who, "", "")
		items := apitest.List(t, list, "items")
		if len(items) != 1 {
			t.Fatalf("recommendations on %s: got %d, want one", who, len(items))
		}
		rec := items[0].(map[string]any)
		if rec["kind"] != want.kind {
			t.Errorf("kind on %s: got %v, want %s", who, rec["kind"], want.kind)
		}
		if got := strings.Split(rec["content"].(string), "\n")[0]; got != want.line {
			t.Errorf("line on %s: got %q, want %q", who, got, want.line)
		}
	}
	prompt := strings.ReplaceAll(model.Requests[0], `\n`, "\n")
	for _, want := range []string{
		"3 principals to recommend on",
		"- role taxer: not used in the period\n  permissions never used: tax.return.prepare",
		"- role all: used, last at " + c.day(0),
		"- role machine: use not recorded (only reads need its permissions)",
		"- role capt: use not recorded (only reads need its permissions)",
		"- breaks rule " + capture + " / " + post + ": " + capture + " through capt, " + post + " through poster",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt: got %s, want it to contain %q", prompt, want)
		}
	}
	if strings.Contains(prompt, "Olga") {
		t.Errorf("prompt: got %s, want no blocked principal", prompt)
	}
	again, err := (&accessagent.Agent{LLM: llmtest.New(t).Runner(), API: c.agent, Now: func() time.Time { return c.today }}).Prepare(context.Background(), period)
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if again.Session != "" || len(again.Recommendations) != 0 || !strings.Contains(again.Reply, "nothing to add") {
		t.Errorf("the second run: got %+v, want no session, no recommendation and a reply saying so", again)
	}
}
