// Package enablementagent is the enablement agent (ADR-0072 As built), a
// recommending agent (internal/recommend). Its first capability (T109a):
// a process owner has it write the description of a process (purchase to
// pay, or order to cash) from the tenant's active configuration and roles:
// who does what, with the limits and tolerances in force. It reads the
// configuration, the roles, their permissions and the segregation of
// duties rules through API tools (04-api-design §4.13) and leaves the
// description, in Markdown, as one recommendation of the kind
// process_document on the active configuration version. Its check holds
// the description to the configuration and the identity data in code:
// it names the version on its first line, every setting it cites is the
// active version's with that value, every role it names (in backticks, or
// a code joined by underscores) is one of the tenant's active roles, and
// it writes no number outside its citations, in digits or in words, and
// "percent" only after a cited setting, so every amount and percentage it
// states is the configuration's. Its
// second capability (T109b, help.go) explains a field or a problem code
// of an operation from the contract and the configuration, with a reply
// only. The agent changes nothing (Never). Its evaluation suite is
// evals/enablement (ADR-0039).
package enablementagent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/config"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// Type is the agent's type, as its profile names it and its evaluation
// suite certifies it (ADR-0039).
const Type = "enablement"

// The object type the agent recommends on, and its kind.
const (
	VersionObject   = "config_version"
	ProcessDocument = "process_document"
)

// Reads are the operations the agent reads with (T109a).
var Reads = []string{"listConfigVersions", "getConfigVersion", "listRoles", "listRolePermissions", "listSodRules"}

// Never are the operations the agent never calls (ADR-0072 §5): every
// write of the configuration and of access, which its tools hold none of.
var Never = []string{"createChangeRequest", "updateChangeRequest", "simulateChangeRequest", "submitChangeRequest", "withdrawChangeRequest",
	"activateChangeRequest", "applyCountryPack", "createRegressionSuite", "createRole", "updateRole", "grantRolePermission",
	"endRolePermission", "createRoleAssignment", "endRoleAssignment", "createSodRule", "endSodRule", "createPrincipal",
	"updatePrincipal", "blockPrincipal", "createCompanyAccess", "endCompanyAccess"}

// process is a process the agent describes: its title and what of the
// configuration it rests on.
type process struct {
	title, reads string
}

// processes are the processes the first version knows (T109a).
var processes = map[string]process{
	"purchase_to_pay": {"Purchase to pay", "the approval of requisitions and the release of purchase orders by value " +
		"(purchasing.requisition_approval, purchasing.order_release_approval: each tier's up_to and the roles and approvals of " +
		"its steps), the goods receipt's over-delivery tolerance (companies[<code>].gr_over_delivery_percent), the invoice " +
		"tolerance (companies[<code>].invoice_tolerance), the posting limits (auto_post_limit, service_posting_limit, " +
		"manual_entry_approval_limit), the payment terms (payment_terms), and the release of payments (payments.release_role, " +
		"payments.release_approvals, companies[<code>].release_dual_limit, payments.bank_change_hold_days, " +
		"payments.vendor_approver_role)"},
	"order_to_cash": {"Order to cash", "the pricing procedure and its condition types (pricing), the payment terms " +
		"(payment_terms), the dunning levels of each company (companies[<code>].dunning.levels: their days overdue) and the " +
		"companies' currencies"},
}

// Processes are the processes the agent describes, in order.
func Processes() []string { return slices.Sorted(maps.Keys(processes)) }

const system = `You describe a business process of a tenant from its active configuration and its roles, for a process owner to
read and decide on.

Read the active configuration version (getConfigVersion with the id you are given), the tenant's roles (listRoles), what
each role may do (listRolePermissions) and the segregation of duties rules (listSodRules). Then describe the process you
are asked about, step by step, in Markdown: who does what, with the limits, tolerances and approvals in force.

Recommend once, on the configuration version, with the kind process_document; the content is the description. Its first
line is the heading you are given, word for word. Cite every setting of the configuration you state as its path and its
value, each in backticks, as ` + "`companies[1000].invoice_tolerance.price_percent` = `2`" + `, the path as the configuration
names it (a list item by its key, as companies[1000], or by its position from 0, as requisition_approval[0]); a setting
written any other way is not seen. Write every role as its code in backticks, as ` + "`Z_PURCH_MGR`" + `, and only roles the
tenant has; put nothing else in backticks but citations. Write no number outside a citation, in digits or in words: every
amount, percentage, count of approvals or days, and every ordinal beyond second, is a setting cited, and "percent"
follows only a cited percentage. Keep the
description under ten thousand characters.`

// Agent writes a process's description from the configuration (T109a)
// and explains a field or a problem code of an operation (T109b).
type Agent struct {
	LLM *llm.Runner
	API *apitools.Client
	// MaxIterations bounds the model's requests in one run; default 30.
	MaxIterations int
}

// The refusals before a run and of the agent's check, beside the
// pattern's.
var (
	// ErrProcess refuses a process the first version does not describe.
	ErrProcess = fmt.Errorf("enablementagent: a process is one of %s", strings.Join(Processes(), ", "))
	// ErrHeading refuses a description whose first line is not the heading
	// the run gave: the process and the configuration version described.
	ErrHeading = errors.New("enablementagent: the description's first line is the heading given, naming the process and the configuration version")
	// ErrUncited refuses a description that cites no setting.
	ErrUncited = errors.New("enablementagent: the description cites the settings it rests on as `path` = `value`")
	// ErrSetting refuses a citation of a setting the active configuration
	// does not hold with that value.
	ErrSetting = errors.New("enablementagent: every setting cited as `path` = `value` is the active configuration's, with that value")
	// ErrRole refuses a role named that is not one of the tenant's active
	// roles: any text in backticks outside the citations, and any code
	// joined by underscores.
	ErrRole = errors.New("enablementagent: every text in backticks outside a citation, and every code joined by underscores, is one of the tenant's active roles")
	// ErrNumber refuses a number outside the citations, in digits or in
	// words, and "percent" that does not follow a cited setting.
	ErrNumber = errors.New("enablementagent: every number is a setting cited as `path` = `value`, never in words; percent follows only a cited setting")
)

// role is a role as the run reads it before its session.
type role struct {
	Code   string `json:"code"`
	Status string `json:"status"`
}

// Document runs one session on the process: it refuses a process it does
// not know, reads the active configuration version and the tenant's roles
// (a version or roles the agent may not read stop the run before a
// session starts), offers the model the tools and reports what it
// recommended, also when the run fails after recommending.
func (a *Agent) Document(ctx context.Context, name string) (recommend.Result, error) {
	p, known := processes[name]
	if !known {
		return recommend.Result{}, fmt.Errorf("%w, not %q", ErrProcess, name)
	}
	v, err := apitools.ActiveConfig(ctx, a.API)
	if err != nil {
		return recommend.Result{}, err
	}
	roles, err := a.roles(ctx)
	if err != nil {
		return recommend.Result{}, err
	}
	d := &documenting{heading: Heading(p.title, v.Number), active: config.Readable(v.Document), roles: roles}
	task := recommend.Task{
		Purpose: fmt.Sprintf("Describe %s from configuration version %d", strings.ToLower(p.title), v.Number),
		Prompt: fmt.Sprintf("Describe the process %s. It rests on %s.\nThe tenant's active configuration is version %d (id %s).\nThe description's first line is:\n%s",
			strings.ToLower(p.title), p.reads, v.Number, v.ID, d.heading),
		Objects: map[string]string{v.ID: VersionObject},
		Check:   d.check,
	}
	return agent(a.MaxIterations).Run(ctx, a.LLM, a.API, task)
}

// roles are the tenant's roles by code, whether each is active: what a
// run holds the roles its text names to (roles the agent may not read
// stop it before a session).
func (a *Agent) roles(ctx context.Context) (map[string]bool, error) {
	roles, err := apitools.List[role](ctx, a.API, "/v1/roles", nil)
	if err != nil {
		return nil, fmt.Errorf("enablementagent: the roles: %w", err)
	}
	out := map[string]bool{}
	for _, r := range roles {
		out[r.Code] = r.Status == "active"
	}
	return out, nil
}

// Heading is the first line of a process's description: its title and the
// configuration version it describes.
func Heading(title string, version int) string {
	return fmt.Sprintf("# %s, configuration version %d", title, version)
}

// agent is the agent as the pattern runs it.
func agent(maxIterations int) recommend.Agent {
	return recommend.Agent{Type: Type, Reads: Reads, System: system, MaxIterations: maxIterations,
		Kinds: map[string][]string{VersionObject: {ProcessDocument}}}
}

// Tools are the operations the agent may call when it describes a
// process: its reads and the one that recommends.
func Tools() []string { return agent(0).Tools() }

// documenting is what one run's check knows: the heading, the active
// configuration and the tenant's roles, active or not.
type documenting struct {
	heading string
	active  *config.Config
	roles   map[string]bool
}

var (
	// spanRe is a text in backticks.
	spanRe = regexp.MustCompile("`([^`]*)`")
	// codeRe is a word joined by underscores, the form of a role's code
	// (process_owner, Z_PURCH_MGR).
	codeRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:_[A-Za-z0-9]+)+\b`)
	// wordsRe is a number written in words beyond one: a count or an
	// amount the check of digits does not see.
	wordsRe = regexp.MustCompile(`(?i)\b(?:two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|` +
		`sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundreds?|thousands?|` +
		`millions?|billions?|dozens?|half|halves|twice|double|triple|zero|quarters?|third|fourth|fifth|sixth|seventh|eighth|ninth|` +
		`tenth|eleventh|twelfth|thirteenth|fourteenth|fifteenth|sixteenth|seventeenth|eighteenth|nineteenth|twentieth|` +
		`thirtieth|fortieth|fiftieth|sixtieth|seventieth|eightieth|ninetieth|hundredth|thousandth|millionth)\b`)
	// percentRe is a percentage's word or sign.
	percentRe = regexp.MustCompile(`(?i)\bper ?cent\b|%`)
)

// check is the agent's rule on a description (T109a): the heading first,
// at least one setting cited and every one as the active version holds
// it, every role named an active role of the tenant's, and no figure
// outside the citations.
func (d *documenting) check(p recommend.Proposed, _ *recommend.Figures) error {
	first, rest, _ := strings.Cut(p.Content, "\n")
	if strings.TrimSpace(first) != d.heading {
		return ErrHeading
	}
	if len(config.Cited(rest)) == 0 {
		return ErrUncited
	}
	prose, err := uncited(d.active, rest)
	if err != nil {
		return err
	}
	if err := rolesOnly(prose, d.roles, nil); err != nil {
		return err
	}
	if n := recommend.Numbers(spanRe.ReplaceAllString(prose, " ")); len(n) > 0 {
		return fmt.Errorf("%w: not %s", ErrNumber, strings.Join(n, ", "))
	}
	return nil
}

// rolesOnly refuses a text in backticks, or a code joined by underscores,
// that is neither an active role nor one of the names allowed beside them.
func rolesOnly(prose string, roles map[string]bool, allowed []string) error {
	named := codeRe.FindAllString(spanRe.ReplaceAllString(prose, " "), -1)
	for _, m := range spanRe.FindAllStringSubmatch(prose, -1) {
		named = append(named, m[1])
	}
	for _, code := range named {
		if !roles[code] && !slices.Contains(allowed, code) {
			return fmt.Errorf("%w: not %s", ErrRole, code)
		}
	}
	return nil
}

// uncited holds every setting a text cites to the configuration, with its
// value, and returns the text without its citations (a percentage's word
// or sign right after one goes with it) and its list's numbering; a number
// in words, or a percentage's word or sign left in it, refuses the text.
func uncited(c *config.Config, text string) (string, error) {
	if s, unheld := config.Unheld(c, text); unheld {
		return "", fmt.Errorf("%w: not %s = %s", ErrSetting, s.Path, s.Value)
	}
	for _, s := range config.Cited(text) {
		if !strings.HasSuffix(s.Path, "percent") {
			continue // a percentage's word follows only a percentage
		}
		for _, unit := range []string{" percent", " per cent", "%", " %"} {
			text = strings.ReplaceAll(text, s.Text+unit, " ")
		}
	}
	prose := recommend.Unnumbered(config.Uncited(text))
	if w := wordsRe.FindString(prose); w != "" {
		return "", fmt.Errorf("%w: not %q", ErrNumber, w)
	}
	if w := percentRe.FindString(prose); w != "" {
		return "", fmt.Errorf("%w: not %q", ErrNumber, w)
	}
	return prose, nil
}
