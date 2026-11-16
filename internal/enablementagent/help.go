package enablementagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	anthropic "github.com/anthropics/anthropic-sdk-go"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// HelpReads are the operations the agent reads with when it explains
// (T109b): its reads and getSchema, an entity's custom fields. Beside
// them it replies (Reply).
var HelpReads = append(slices.Clone(Reads), "getSchema")

// Reply is the name of the tool the agent explains with.
const Reply = "reply"

// HelpTools are the tools the agent may call when it explains: its reads
// and its reply. None writes.
func HelpTools() []string { return append(slices.Clone(HelpReads), Reply) }

// Question is what a user asks help on: an operation and one of its
// fields or one of its problem codes.
type Question struct {
	Operation, Field, Problem string
}

// Helped is what a help run did: its session, the reply its check
// admitted and how many replies it refused.
type Helped struct {
	Session string
	Reply   string
	Refused int
}

// The refusals before a help run and of its check.
var (
	// ErrAsk refuses a question that names no operation, or neither or both
	// of a field and a problem code.
	ErrAsk = errors.New("enablementagent: ask about an operation's field or one of its problem codes, one of them")
	// ErrNotQuoted refuses a reply that quotes nothing of the contract, or
	// quotes in double quotes what the contract's descriptions do not
	// write, character for character.
	ErrNotQuoted = errors.New("enablementagent: the reply quotes the contract's description in double quotes, character for character")
	// ErrNotNamed refuses a reply that does not name the field or problem
	// code asked about.
	ErrNotNamed = errors.New("enablementagent: the reply names the field or problem code asked about")
	// ErrHelpNumber refuses a number outside the reply's quotations and
	// citations that the question does not write.
	ErrHelpNumber = errors.New("enablementagent: a number outside the quotations is a setting cited as `path` = `value`")
	// ErrSecond refuses a second reply: the first stands.
	ErrSecond = errors.New("enablementagent: the run has replied")
	// ErrNoReply fails a run whose model gave no reply the check admitted.
	ErrNoReply = errors.New("enablementagent: the agent gave no reply its check admits")
)

const helpSystem = `You explain to a user what a field or a problem code of an operation of this ERP system's API means, from the API
contract and the tenant's active configuration.

What the contract says of the operation, and of the field or the problem code asked about, is given to you between the
lines <contract> and </contract>. Read the active configuration version (getConfigVersion with the id you are given) for
the settings that apply; listRoles and listRolePermissions read who may call the operation, getSchema an entity's custom
fields.

Answer once, with the tool reply. Name the field or problem code asked about. Quote what the contract says in double
quotes, each passage exactly as the contract writes it. Cite each setting of the configuration that applies as its path
and its value, each in backticks, as ` + "`companies[1000].invoice_tolerance.price_percent` = `2`" + `. Write a role as its code
in backticks, and only roles the tenant has; put nothing else in backticks, and write no other code joined by
underscores, than the citations, the field or problem code asked about, the operation's id, its permission and its
problem codes. Write no other number outside a quotation, in
digits or in words; "percent" follows only a cited percentage. You change nothing and have no tool that writes. After the
reply, end with one short sentence.`

// Help runs one session on the question: it refuses, before anything is
// read or a model asked, a question that names an operation, field or
// problem code the contract does not have; it reads the active
// configuration version and the tenant's roles (a version or roles the
// agent may not read, without identity.access.read, stop the run before a
// session starts), offers the model its reads and its reply, and
// reports the reply its check admitted, also when the run fails after
// replying.
func (a *Agent) Help(ctx context.Context, q Question) (Helped, error) {
	if q.Operation == "" || (q.Field == "") == (q.Problem == "") {
		return Helped{}, ErrAsk
	}
	e, err := apitools.Explain(q.Operation, q.Field, q.Problem)
	if err != nil {
		return Helped{}, err
	}
	v, err := apitools.ActiveConfig(ctx, a.API)
	if err != nil {
		return Helped{}, err
	}
	roles, err := a.roles(ctx)
	if err != nil {
		return Helped{}, err
	}
	s, err := apitools.StartSession(ctx, a.API, "Explain "+asked(q)+" of "+q.Operation, a.LLM.Model)
	if err != nil {
		return Helped{}, err
	}
	res := Helped{Session: s.ID}
	tools, _ := apitools.Tools(a.API, s, HelpReads...) // reads of the contract open to agents (TestHelpTools)
	h := &helping{res: &res, asked: asked(q), name: q.Field + q.Problem, active: config.Readable(v.Document), roles: roles,
		sources: []string{e.Operation.Summary, e.Operation.Description, e.FieldDescription},
		allowed: append([]string{q.Field + q.Problem, e.Operation.ID, e.Operation.Permission}, e.Operation.ProblemCodes...)}
	tools = append(tools, h.tool(s))
	var mu sync.Mutex
	for _, t := range tools {
		t.Exclusive(&mu) // the reply's guard and answer share the result
	}
	prompt := fmt.Sprintf("The tenant's active configuration is version %d (id %s).\nThe user asks what %s of %s means.\n%s",
		v.Number, v.ID, h.asked, q.Operation, contract(e))
	h.numbers = recommend.Numbers(q.Operation + " " + h.name)
	iterations := a.MaxIterations
	if iterations <= 0 {
		iterations = 20
	}
	if _, err := apitools.Run(ctx, a.LLM, s, tools, a.LLM.Params(helpSystem, anthropic.NewBetaTextBlock(prompt)), iterations); err != nil {
		return res, err
	}
	if res.Reply == "" {
		return res, ErrNoReply
	}
	return res, nil
}

// asked is the field or problem code a question is about.
func asked(q Question) string {
	if q.Field != "" {
		return "the field " + q.Field
	}
	return "the problem code " + q.Problem
}

// contract is what the contract says, as the prompt gives it.
func contract(e apitools.Explained) string {
	var b strings.Builder
	o := e.Operation
	fmt.Fprintf(&b, "<contract>\noperation %s: %s %s\nsummary: %s\ndescription: %s\n", o.ID, o.Method, o.Path, o.Summary, o.Description)
	if o.Permission != "" {
		fmt.Fprintf(&b, "permission: %s\n", o.Permission)
	}
	fmt.Fprintf(&b, "problem codes: %s\n", strings.Join(o.ProblemCodes, ", "))
	if e.Field != "" {
		fmt.Fprintf(&b, "field %s: %s\n", e.Field, e.FieldDescription)
	}
	if e.Problem != "" && !slices.Contains(o.ProblemCodes, e.Problem) {
		fmt.Fprintf(&b, "problem code %s: one any operation may answer\n", e.Problem)
	}
	b.WriteString("</contract>")
	return b.String()
}

// helping is what one help run's check knows: what was asked and its name, the
// contract's texts the reply may quote, the numbers the question writes,
// the active configuration and the result its reply is kept in.
type helping struct {
	res     *Helped
	asked   string // what was asked, as the prompt says it
	name    string // the field's or the problem code's name
	sources []string
	numbers []string
	active  *config.Config
	roles   map[string]bool // the tenant's roles, whether each is active
	allowed []string        // names a reply may write in backticks beside roles
}

// tool is the agent's reply: its check guards it, and an admitted reply
// is the run's.
func (h *helping) tool(s *apitools.Session) *apitools.Tool {
	t := apitools.Local(s, Reply, "Give the user your explanation: the contract quoted in double quotes and the settings cited as `path` = `value`.",
		map[string]any{"text": map[string]any{"type": "string", "description": "The explanation."}}, []string{"text"},
		func(_ context.Context, in map[string]any) (any, error) {
			h.res.Reply, _ = in["text"].(string) // the guard admitted text
			return map[string]any{"replied": true}, nil
		})
	t.Guard(func(in map[string]any) error {
		text, _ := in["text"].(string) // not text: empty, refused below
		err := h.check(text)
		if err != nil {
			h.res.Refused++
		}
		return err
	})
	return t
}

// check is the agent's rule on a reply (T109b): one reply, naming what was
// asked, quoting the contract's descriptions character for character,
// citing settings as the active configuration holds them, and writing no
// number outside its quotations and citations that the question does not.
func (h *helping) check(text string) error {
	if h.res.Reply != "" {
		return ErrSecond
	}
	if !strings.Contains(text, h.name) {
		return fmt.Errorf("%w: %s", ErrNotNamed, h.name)
	}
	quoted, rest := recommend.Quotations(text)
	if len(quoted) == 0 {
		return ErrNotQuoted
	}
	for _, q := range quoted {
		if !slices.ContainsFunc(h.sources, func(s string) bool { return recommend.Quoted(s, q) }) {
			return fmt.Errorf("%w: not %q", ErrNotQuoted, q)
		}
	}
	prose, err := uncited(h.active, rest)
	if err != nil {
		return err
	}
	if err := rolesOnly(prose, h.roles, h.allowed); err != nil {
		return err
	}
	for _, n := range recommend.Numbers(prose) {
		if !slices.Contains(h.numbers, n) {
			return fmt.Errorf("%w: not %s", ErrHelpNumber, n)
		}
	}
	return nil
}
