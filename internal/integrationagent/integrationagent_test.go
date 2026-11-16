package integrationagent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mustie7iy/agentic-erp/api"
	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/integrationagent"
	"github.com/mustie7iy/agentic-erp/internal/llm/llmtest"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

const (
	session   = "01a1aa00-0000-7000-8000-000000000a01"
	refusing  = "01a1aa00-0000-7000-8000-000000000b01" // a subscription whose endpoint refuses
	healthy   = "01a1aa00-0000-7000-8000-000000000b02"
	disabled  = "01a1aa00-0000-7000-8000-000000000b03" // disabled by the dispatcher, nothing kept
	dead1     = "01a1aa00-0000-7000-8000-000000000d01"
	pending1  = "01a1aa00-0000-7000-8000-000000000d02"
	failedRun = "01a1aa00-0000-7000-8000-000000000e01"
	oldRun    = "01a1aa00-0000-7000-8000-000000000e02"
	okRun     = "01a1aa00-0000-7000-8000-000000000e03"
	sap       = "01a1aa00-0000-7000-8000-000000000f01" // its latest run failed
	treasury  = "01a1aa00-0000-7000-8000-000000000f02" // its latest run succeeded after a failed one
	refused   = "01a1aa00-0000-7000-8000-000000000c01" // a file the channel refused
	rejected  = "01a1aa00-0000-7000-8000-000000000c02" // a file the bank rejected
)

// fakeAPI is a stand-in for the cell's API: three subscriptions, one
// failing with a dead and a pending delivery that failed, one healthy,
// one disabled; two bank files; two connectors with their runs; a
// session, and recommendations answered by their bodies. Quiet answers
// nothing failing. It keeps every request it was sent.
type fakeAPI struct {
	mu     sync.Mutex
	calls  []string
	quiet  bool
	refuse string // a path answered 403
	many   bool   // the refusing subscription has more dead deliveries than a line names
	made   int
}

func (a *fakeAPI) answer(path, query string) (int, string) {
	if path == a.refuse {
		return 403, `{"code": "identity.permission_missing", "status": 403}`
	}
	if a.quiet {
		return 200, `{"items": []}`
	}
	switch {
	case path == "/v1/webhook-subscriptions":
		return 200, `{"items": [{"id": "` + refusing + `", "url": "https://hooks.example.com/erp", "description": "Treasury events", "status": "active",
			"failing_since": "2026-11-16T09:00:00Z"}, {"id": "` + healthy + `", "url": "https://ok.example.com", "description": "Portal", "status": "active"},
			{"id": "` + disabled + `", "url": "https://gone.example.com", "description": "Old portal", "status": "disabled", "disabled_reason": "gone"}]}`
	case path == "/v1/webhook-subscriptions/"+refusing+"/deliveries" && query == "status=dead" && a.many:
		var items []string
		for i := range integrationagent.MaxEvidence + 1 {
			items = append(items, fmt.Sprintf(`{"id": "01a1aa00-0000-7000-8000-0000000001%02d", "type": "webhook.ping", "status": "dead", "attempts": 1, "last_status": 401}`, i))
		}
		return 200, `{"items": [` + strings.Join(items, ", ") + `]}`
	case path == "/v1/webhook-subscriptions/"+refusing+"/deliveries" && query == "status=dead":
		return 200, `{"items": [{"id": "` + dead1 + `", "type": "webhook.ping", "status": "dead", "attempts": 1, "last_status": 401}]}`
	case path == "/v1/webhook-subscriptions/"+refusing+"/deliveries" && query == "status=pending":
		return 200, `{"items": [{"id": "` + pending1 + `", "type": "com.example.erp.cash.cash_snapshot_taken.v1", "status": "pending", "attempts": 2,
			"last_error": "connection refused"}, {"id": "01a1aa00-0000-7000-8000-000000000d09", "status": "pending", "attempts": 0}]}`
	case path == "/v1/bank-files" && query == "status=failed":
		return 200, `{"items": [{"id": "` + refused + `", "payment_run_id": "run1", "house_bank_code": "RBC", "format": "pain.001.001.03", "status": "failed"}]}`
	case path == "/v1/bank-files" && query == "status=rejected":
		return 200, `{"items": [{"id": "` + rejected + `", "payment_run_id": "run2", "house_bank_code": "RBC", "format": "pain.001.001.03", "status": "rejected"}]}`
	case path == "/v1/connectors":
		return 200, `{"items": [{"id": "` + sap + `", "kind": "sap_file", "name": "SAP ECC", "status": "active"},
			{"id": "` + treasury + `", "kind": "sap_odata", "name": "SAP S/4", "status": "active"}, {"id": "idle", "kind": "email", "name": "Mail", "status": "active"}]}`
	case path == "/v1/migration-runs":
		return 200, `{"items": [{"id": "` + okRun + `", "connector_id": "` + treasury + `", "status": "reconciled", "created_at": "2026-11-16T10:00:00Z"},
			{"id": "` + oldRun + `", "connector_id": "` + treasury + `", "status": "failed", "created_at": "2026-11-15T10:00:00Z"},
			{"id": "` + failedRun + `", "connector_id": "` + sap + `", "status": "failed", "operation_id": "op1", "created_at": "2026-11-16T08:00:00Z"}]}`
	}
	return 200, `{"items": []}`
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body) // a test's own request
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Get("status"))
	a.mu.Unlock()
	write := func(status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // a test's answer; a client gone has nothing to read
	}
	q := r.URL.Query()
	q.Del("page_size")
	switch r.URL.Path {
	case "/v1/agent-sessions":
		write(201, `{"id": "`+session+`", "autonomy_level": 1}`)
	case "/v1/recommendations":
		var body struct {
			ObjectID   string `json:"object_id"`
			ObjectType string `json:"object_type"`
			Kind       string
		}
		_ = json.Unmarshal(b, &body) // the agent's own JSON
		a.mu.Lock()
		a.made++
		id := fmt.Sprintf("r%d", a.made)
		a.mu.Unlock()
		write(201, `{"id": "`+id+`", "object_id": "`+body.ObjectID+`", "object_type": "`+body.ObjectType+`", "kind": "`+body.Kind+`"}`)
	default:
		write(a.answer(r.URL.Path, q.Encode()))
	}
}

// sent are the calls the API was sent whose method and path contain
// what.
func (a *fakeAPI) sent(what string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(a.calls), func(c string) bool { return !strings.Contains(c, what) })
}

func client(t *testing.T, a *fakeAPI) *apitools.Client {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &apitools.Client{BaseURL: srv.URL, Token: "t"}
}

var sessionRe = regexp.MustCompile(`session id is ([0-9a-f-]{36})`)

// rec is a recommendation of kind with the content on the object.
func rec(objectType, id, kind, content string) llmtest.Step {
	return llmtest.ToolFn("createRecommendation", func(conversation string) any {
		return map[string]any{"body": map[string]any{"session_id": sessionRe.FindStringSubmatch(conversation)[1], "object_type": objectType,
			"object_id": id, "kind": kind, "content": content}}
	})
}

// refusedWith reports whether the model was told the refusal.
func refusedWith(t *testing.T, requests []string, want error) {
	t.Helper()
	told, _ := json.Marshal(want.Error()) // the model reads the refusal as JSON text
	if !strings.Contains(strings.Join(requests, "\n"), strings.Trim(string(told), `"`)) {
		t.Errorf("the model was not told %q", want)
	}
}

const (
	refusingLine = "Failing: subscription active, failing since 2026-11-16T09:00:00Z; failed deliveries " + dead1 + ", " + pending1
	disabledLine = "Failing: subscription disabled (gone)"
	sapLine      = "Failing: migration run " + failedRun + " failed"
)

// TestMonitor: the agent finds the failing subscriptions (one refusing
// deliveries, one disabled), the bank files refused or rejected and the
// connector whose latest run failed, and recommends once on each. Its
// check refuses a kind not given for the object (redeliver without a
// dead delivery, pause of a disabled one, retransmit of a file the bank
// rejected), a first line other than the one computed, and a number the
// run did not read; nothing refused reaches the API. The healthy
// subscription and the connector whose latest run succeeded are not
// recommended on.
func TestMonitor(t *testing.T) {
	a := &fakeAPI{}
	model := llmtest.New(t,
		llmtest.Tool("listWebhookDeliveries", map[string]any{"id": refusing}),
		rec("webhook_subscription", refusing, "check_endpoint", "The endpoint refuses."),
		rec("webhook_subscription", refusing, "check_endpoint", refusingLine+"\nIt answered HTTP 401 to 4711 deliveries."),
		rec("webhook_subscription", refusing, "check_endpoint", refusingLine+"\nThe ping died on HTTP 401 and the snapshot's delivery got "+
			"connection refused: check the endpoint's signing secret and that it is listening."),
		rec("webhook_subscription", disabled, "pause", disabledLine+"\nPause it."),
		rec("webhook_subscription", disabled, "check_endpoint", disabledLine+"\nThe endpoint answered Gone: check its URL with its owner."),
		rec("bank_file", rejected, "retransmit", "Failing: bank file rejected\nSend it again."),
		rec("bank_file", rejected, "investigate", "Failing: bank file rejected\nThe bank rejected it: read its reason in the bank's report."),
		rec("bank_file", refused, "retransmit", "Failing: bank file failed\nThe channel refused it; transmit it again once RBC's channel answers."),
		rec("connector", sap, "investigate", sapLine+"\nRead operation op1 for why the run failed."),
		llmtest.Final("Five failing objects, each recommended on; the administrator decides."))
	res, err := (&integrationagent.Agent{LLM: model.Runner(), API: client(t, a)}).Monitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range res.Recommendations {
		got = append(got, m.ObjectType+" "+m.ObjectID+" "+m.Kind)
	}
	want := []string{"webhook_subscription " + refusing + " check_endpoint", "webhook_subscription " + disabled + " check_endpoint",
		"bank_file " + rejected + " investigate", "bank_file " + refused + " retransmit", "connector " + sap + " investigate"}
	if !slices.Equal(got, want) {
		t.Errorf("recommendations: got %v, want %v", got, want)
	}
	if res.Refused != 4 {
		t.Errorf("refused: got %d, want 4", res.Refused)
	}
	if got := len(a.sent("recommendations")); got != 5 {
		t.Errorf("recommendations sent: got %d, want 5: the check keeps the others from the API", got)
	}
	for _, want := range []string{"5 failing objects", "Line for " + refusing + ": " + refusingLine, "Kinds for " + refusing + ": check_endpoint, investigate, redeliver, pause",
		"Kinds for " + disabled + ": check_endpoint, investigate\\nLine", "Kinds for " + rejected + ": investigate\\nLine", "Kinds for " + refused + ": retransmit, investigate",
		"delivery " + pending1 + " of com.example.erp.cash.cash_snapshot_taken.v1: pending after 2 attempt(s), the last error: connection refused",
		"latest migration run " + failedRun + " of 2026-11-16T08:00:00Z: failed, operation op1", "endpoint https://hooks.example.com/erp"} {
		if !strings.Contains(model.Requests[0], want) {
			t.Errorf("prompt: got %s, want %q", model.Requests[0], want)
		}
	}
	for _, not := range []string{healthy, treasury, oldRun} {
		if strings.Contains(model.Requests[0], not) {
			t.Errorf("prompt: got %s, want no %s", model.Requests[0], not)
		}
	}
	refusedWith(t, model.Requests, integrationagent.ErrKind)
	refusedWith(t, model.Requests, integrationagent.ErrLine)
	refusedWith(t, model.Requests, integrationagent.ErrUnread)
	for id, want := range map[string]string{refusing: "Treasury events", refused: "pain.001.001.03 from RBC", sap: "SAP ECC"} {
		if got := res.Names[id]; got != want {
			t.Errorf("the name of %s: got %q, want %q", id, got, want)
		}
	}
}

// TestMonitorEvidenceBound: a line names at most MaxEvidence failed
// deliveries, those that died first; once it has them, the waiting ones
// are not read.
func TestMonitorEvidenceBound(t *testing.T) {
	a := &fakeAPI{many: true}
	model := llmtest.New(t, llmtest.Final("Nothing recommended."))
	if _, err := (&integrationagent.Agent{LLM: model.Runner(), API: client(t, a)}).Monitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`Line for ` + refusing + `: ([^\\]*)`).FindStringSubmatch(model.Requests[0])
	if line == nil {
		t.Fatalf("prompt: got %s, want a line for %s", model.Requests[0], refusing)
	}
	if got := strings.Count(line[1], "01a1aa00-"); got != integrationagent.MaxEvidence {
		t.Errorf("the deliveries the line names: got %d, want %d", got, integrationagent.MaxEvidence)
	}
	if strings.Contains(line[1], "0000000110") {
		t.Errorf("the line: got %s, want the eleventh delivery left out", line[1])
	}
	if got := a.sent(refusing + "/deliveries?pending"); len(got) != 0 {
		t.Errorf("waiting deliveries read: got %v, want none", got)
	}
}

// TestMonitorIterations: an agent whose MaxIterations is unset may make
// thirty requests in a run, also with few failing objects: reading
// further before recommending does not run out of turns.
func TestMonitorIterations(t *testing.T) {
	var steps []llmtest.Step
	for range 20 {
		steps = append(steps, llmtest.Tool("listBankFiles", map[string]any{"status": "failed"}))
	}
	model := llmtest.New(t, append(steps, llmtest.Final("Read everything; nothing more to recommend."))...)
	res, err := (&integrationagent.Agent{LLM: model.Runner(), API: client(t, &fakeAPI{})}).Monitor(context.Background())
	if err != nil {
		t.Fatalf("twenty-one requests with five failing objects: got %v, want the run to finish", err)
	}
	if res.Reply != "Read everything; nothing more to recommend." {
		t.Errorf("reply: got %q", res.Reply)
	}
}

// TestMonitorNothingFailing: a tenant with nothing failing gets no
// session and a reply saying so.
func TestMonitorNothingFailing(t *testing.T) {
	a := &fakeAPI{quiet: true}
	res, err := (&integrationagent.Agent{LLM: llmtest.New(t).Runner(), API: client(t, a)}).Monitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != "" {
		t.Errorf("session: got %s, want none", res.Session)
	}
	if len(res.Recommendations) != 0 {
		t.Errorf("recommendations: got %v, want none", res.Recommendations)
	}
	if !strings.HasPrefix(res.Reply, "Nothing is failing") {
		t.Errorf("reply: got %q, want it to say nothing is failing", res.Reply)
	}
	if got := a.sent("agent-sessions"); len(got) != 0 {
		t.Errorf("sessions: got %v, want none", got)
	}
}

// TestMonitorRefusals: a read the agent may not make, or an API that is
// gone, stops the run before a session starts.
func TestMonitorRefusals(t *testing.T) {
	gone := &apitools.Client{BaseURL: "http://127.0.0.1:1", Token: "t"}
	for name, c := range map[string]struct {
		api  *apitools.Client
		want string
	}{
		"the subscriptions":   {client(t, &fakeAPI{refuse: "/v1/webhook-subscriptions"}), "the webhook subscriptions: apitools"},
		"their deliveries":    {client(t, &fakeAPI{refuse: "/v1/webhook-subscriptions/" + refusing + "/deliveries"}), "the deliveries of webhook subscription " + refusing},
		"the bank files":      {client(t, &fakeAPI{refuse: "/v1/bank-files"}), "the bank files: apitools"},
		"the connectors":      {client(t, &fakeAPI{refuse: "/v1/connectors"}), "the connectors: apitools"},
		"their runs":          {client(t, &fakeAPI{refuse: "/v1/migration-runs"}), "the migration runs: apitools"},
		"the API gone at all": {gone, "127.0.0.1:1"},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := (&integrationagent.Agent{LLM: llmtest.New(t).Runner(), API: c.api}).Monitor(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
			if res.Session != "" {
				t.Errorf("a session: got %s, want none", res.Session)
			}
		})
	}
}

// TestNever: the agent's tools are reads and the one recommendation; none
// is an operation of its never list, which holds every write of
// connectors, webhooks, bank files, migration runs and operations, and
// the contract closes the writes it names to agents (ADR-0072, T110a).
func TestNever(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	tools := integrationagent.Tools()
	for _, id := range tools {
		t.Run(id, func(t *testing.T) {
			op, ok := ops[id]
			if !ok {
				t.Fatalf("%s: not in the contract", id)
			}
			if op.Method != http.MethodGet && id != recommend.Proposer {
				t.Errorf("%s: got %s, want a read", id, op.Method)
			}
		})
	}
	for id, op := range ops {
		if op.Method == http.MethodGet {
			continue
		}
		for _, of := range []string{"Connector", "Webhook", "BankFile", "MigrationRun", "Operation"} {
			if strings.Contains(id, of) && !slices.Contains(integrationagent.Never, id) {
				t.Errorf("never list: got %v, want it to hold %s", integrationagent.Never, id)
			}
		}
	}
	for _, id := range integrationagent.Never {
		t.Run(id, func(t *testing.T) {
			if slices.Contains(tools, id) {
				t.Errorf("%s is among the agent's tools", id)
			}
			if _, ok := ops[id]; !ok {
				t.Errorf("%s: not in the contract", id)
			}
		})
	}
}
