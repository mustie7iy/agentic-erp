package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/money"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// Repeats is how often each case runs (ADR-0039).
const Repeats = 3

// Tenant is a throwaway tenant: its administrator's client, a client for
// any of its principals, and its end.
type Tenant interface {
	Admin() *apitools.Client
	As(ctx context.Context, subject string) (*apitools.Client, error)
	Close(ctx context.Context) error
}

// Tenants makes a throwaway tenant from a suite's tenant body.
type Tenants interface {
	New(ctx context.Context, body any) (Tenant, error)
}

// Models gives the agent its model for one repeat of a case: the cell's
// gateway live, the case's replay in CI.
type Models interface {
	For(ctx context.Context, agent *apitools.Client, c Case, saved map[string]string) (*llm.Runner, error)
}

// Env is what an agent adapter runs with.
type Env struct {
	Agent *apitools.Client // the agent principal's client
	LLM   *llm.Runner
	Task  map[string]any // the case's task, filled
	Dir   string         // the suite's directory, where the task's files are
	// Reply records what the agent answered, for the checks that read
	// $reply: an agent whose work is its answer (analytics) leaves no API
	// state to judge. A later reply replaces an earlier one.
	Reply func(doc any)
}

// Agent runs one agent on a case's task.
type Agent func(ctx context.Context, env Env) error

// Runner runs suites.
type Runner struct {
	Tenants Tenants
	Models  Models
	Agents  map[string]Agent // by agent type
	Repeats int              // 0: Repeats
	MaxCost money.Micro      // the run stops past it; 0: no limit of its own
	Now     func() time.Time
}

// ErrMaxCost stops a run that spent past its limit.
var ErrMaxCost = errors.New("eval: the run spent past -max-cost")

// Run runs every case of the suite, Repeats times each, and reports. A run
// stopped by its cost limit reports what it ran, with the error.
func (r *Runner) Run(ctx context.Context, s Suite) (Report, error) {
	agent, ok := r.Agents[s.Agent]
	if !ok {
		return Report{}, fmt.Errorf("eval: no adapter for agent type %s", s.Agent)
	}
	rep := Report{Agent: s.Agent, SuiteVersion: s.Version, Alias: s.Alias, Bars: s.Bars, StartedAt: clock.Now(r.Now).UTC(), Models: []string{},
		Repeats: r.repeats()}
	var err error
	for _, c := range s.Cases {
		cr := CaseResult{Name: c.Name}
		for range r.repeats() {
			var o Outcome
			if o, err = r.once(ctx, s, c, agent); err != nil {
				break
			}
			cr.Repeats = append(cr.Repeats, o)
			if err = rep.add(o); err != nil {
				break
			}
			if r.MaxCost > 0 && rep.CostMicro > r.MaxCost {
				err = ErrMaxCost
				break
			}
		}
		rep.Cases = append(rep.Cases, cr.graded())
		if err != nil {
			break
		}
	}
	rep.FinishedAt, rep.Complete = clock.Now(r.Now).UTC(), err == nil
	rep.grade()
	return rep, err
}

func (r *Runner) repeats() int {
	if r.Repeats > 0 {
		return r.Repeats
	}
	return Repeats
}

// once runs one repeat of a case on a tenant of its own. A failure of the
// harness (the tenant, the setup, the model's ledger) stops the run; the
// agent failing is the case's outcome.
func (r *Runner) once(ctx context.Context, s Suite, c Case, agent Agent) (o Outcome, err error) {
	ten, err := r.Tenants.New(ctx, s.Tenant)
	if err != nil {
		return Outcome{}, err
	}
	// Deferred, so an adapter that panics still leaves no tenant behind.
	defer func() {
		if cerr := ten.Close(context.WithoutCancel(ctx)); err == nil && cerr != nil {
			err = fmt.Errorf("eval: close the tenant: %w", cerr)
		}
	}()
	return r.run(ctx, s, c, ten, agent)
}

func (r *Runner) run(ctx context.Context, s Suite, c Case, ten Tenant, agent Agent) (Outcome, error) {
	saved := map[string]string{}
	for _, call := range append(append([]Call{}, s.Setup...), c.Setup...) {
		as := ten.Admin()
		if call.As != "" {
			var err error
			if as, err = ten.As(ctx, call.As); err != nil {
				return Outcome{}, fmt.Errorf("eval: case %q: setup as %s: %w", c.Name, call.As, err)
			}
		}
		if err := do(ctx, as, call, saved); err != nil {
			return Outcome{}, fmt.Errorf("eval: case %q: setup %s %s: %w", c.Name, call.Method, call.Path, err)
		}
	}
	if err := capped(ctx, ten.Admin()); err != nil {
		return Outcome{}, fmt.Errorf("eval: case %q: %w", c.Name, err)
	}
	as, err := ten.As(ctx, s.AgentSubject)
	if err != nil {
		return Outcome{}, err
	}
	runner, err := r.Models.For(ctx, as, c, saved)
	if err != nil {
		return Outcome{}, err
	}
	task, err := fill(any(c.Task), saved)
	if err != nil {
		return Outcome{}, fmt.Errorf("eval: case %q: task: %w", c.Name, err)
	}
	taskMap, _ := task.(map[string]any) // fill keeps a map a map; a case without a task has none
	o := Outcome{Correct: true, Safe: true}
	var reply any
	env := Env{Agent: as, LLM: runner, Task: taskMap, Dir: s.Dir, Reply: func(doc any) { reply = doc }}
	if err := agent(ctx, env); err != nil {
		o.fail(Correctness, "agent: "+err.Error())
	}
	for _, ch := range c.Checks {
		failed, err := check(ctx, ten.Admin(), ch, saved, reply)
		if err != nil {
			return Outcome{}, fmt.Errorf("eval: case %q: check %q: %w", c.Name, ch.Name, err)
		}
		if failed != nil {
			o.fail(ch.Kind, ch.Name+": "+failed.Error())
		}
	}
	if o.CostMicro, o.Currency, o.Models, err = spent(ctx, ten.Admin(), s.Alias); err != nil {
		return o, fmt.Errorf("eval: case %q: %w", c.Name, err)
	}
	return o, nil
}

// spent is what the repeat's tenant spent on models, from its ledger
// (getAiUsage, ADR-0036): the agent's calls and the server's on its behalf
// (capture), and the model the suite's alias routes to when it called any.
// The ledger reports cents, so a repeat's cost is within half a cent and
// -max-cost sums rounded figures; the gateway's caps bound the real spend.
func spent(ctx context.Context, admin *apitools.Client, alias string) (money.Micro, string, []string, error) {
	usage, err := get(ctx, admin, "/v1/ai-usage")
	if err != nil {
		return 0, "", nil, err
	}
	calls, _ := at(usage, "calls")        // getAiUsage always answers calls
	amount, _ := at(usage, "cost.amount") // and its cost
	if text(calls) == "0" {
		return 0, "", []string{}, nil
	}
	cur, _ := at(usage, "cost.currency")
	minor, err := money.ParseDecimal(text(amount), 2)
	if err != nil {
		return 0, "", nil, fmt.Errorf("eval: the usage's cost %s: %w", text(amount), err)
	}
	policy, err := get(ctx, admin, "/v1/ai-policy")
	if err != nil {
		return 0, "", nil, err
	}
	model, ok := at(policy, "effective_routes."+alias)
	if !ok {
		return 0, "", nil, fmt.Errorf("eval: the tenant routes no alias %s", alias)
	}
	return money.MicroOf(minor), text(cur), []string{text(model)}, nil
}

// capped refuses a tenant whose AI policy has no monthly cap: a live run
// spends only within one, whatever the runner's own limit (ADR-0039).
func capped(ctx context.Context, admin *apitools.Client) error {
	doc, err := get(ctx, admin, "/v1/ai-policy")
	if err != nil {
		return err
	}
	if _, ok := at(doc, "monthly_cap.amount"); !ok {
		return errors.New("the setup leaves the tenant's AI policy without a monthly cap")
	}
	return nil
}

// do makes one setup call and saves what it names.
func do(ctx context.Context, c *apitools.Client, call Call, saved map[string]string) error {
	path, err := fillString(call.Path, saved)
	if err != nil {
		return err
	}
	body, err := fill(call.Body, saved)
	if err != nil {
		return err
	}
	method := call.Method
	if method == "" {
		method = http.MethodPost
	}
	res, err := c.Do(ctx, method, path, nil, body, "*")
	if err != nil {
		return fmt.Errorf("eval: call: %w", err)
	}
	if (call.Status == 0 && !res.OK()) || (call.Status != 0 && res.Status != call.Status) {
		return fmt.Errorf("answered %d: %s", res.Status, res.Body)
	}
	doc, err := decode(res.Body)
	if err != nil {
		return err
	}
	for name, p := range call.Save {
		if p, err = fillString(p, saved); err != nil {
			return err
		}
		v, ok := at(doc, p)
		if !ok {
			return fmt.Errorf("the answer has no %s to save as %s", p, name)
		}
		saved[name] = text(v)
	}
	return nil
}

// decode reads an answer keeping numbers as they were written.
func decode(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("eval: the answer is not JSON: %w", err)
	}
	return doc, nil
}
