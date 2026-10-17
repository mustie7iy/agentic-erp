package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// Alias is the model alias the analytics agent names unless ERP_AGENT_MODEL
// says otherwise.
const Alias = "erp-analytics"

// Agent turns a plain-language question into semantic queries and a
// dashboard. Its tools can only read governed metrics and compose specs;
// all arithmetic happens in the engine.
type Agent struct {
	Client anthropic.Client
	Model  string
	Today  func() time.Time
}

// NewAgent calls models with client, naming model: the cell's model
// gateway with the person's token, and its analytics alias (ADR-0036).
func NewAgent(client anthropic.Client, model string) *Agent {
	return &Agent{Client: client, Model: model, Today: time.Now}
}

const analystPrompt = `You are the analytics agent of an ERP system. People ask business questions in plain language; you answer each one with a dashboard built from governed data.

How to work:
1. Call list_metrics to see the governed metrics, their meaning, dimensions and the defaults for everyday words (for example "sales" means the metric listed under defaults).
2. Interpret the question. When a term is ambiguous, use the governed default and record the assumption instead of asking. Comparisons like "vs last year this time" mean the same months of the previous fiscal year, up to the last complete month in the data (see data_through in query results and today's date).
3. Call run_query for the data you need. Prefer a few well-shaped queries (for example measures by fiscal_year and fiscal_month) over many small ones.
4. Call build_dashboard once with a spec: a title, the question, your assumptions, 2-4 tiles, one or two charts, a comparison table where it helps, short commentary, and 2-4 follow-up questions. Tiles, charts and tables reference query results by query_id; never type numbers into them.
5. Commentary must only use numbers present in the results, their subtotals, or changes between two results that differ in one dimension (e.g. the same month in two years). The tool rejects any other figure; if it does, fix the commentary and call build_dashboard again.
6. If the data can't answer the question (a metric or breakdown doesn't exist), say so plainly in the assumptions and answer the closest question the data supports.

When build_dashboard succeeds, reply with one or two sentences summarizing the answer.`

type listArgs struct{}

type queryArgs struct {
	Measures []string     `json:"measures" jsonschema:"required,description=Metric names from list_metrics"`
	By       []string     `json:"by,omitempty" jsonschema:"description=Dimensions to break down by"`
	Where    []filterArgs `json:"where,omitempty" jsonschema:"description=Filters on dimensions"`
	OrderBy  string       `json:"order_by,omitempty" jsonschema:"description=A measure or dimension in the query"`
	Desc     bool         `json:"desc,omitempty"`
	Limit    int          `json:"limit,omitempty"`
}

type filterArgs struct {
	Dim    string   `json:"dim" jsonschema:"required"`
	Op     string   `json:"op" jsonschema:"required,enum=in,enum=not_in,enum=<=,enum=>=,enum=between"`
	Values []string `json:"values" jsonschema:"required"`
}

type buildArgs struct {
	Spec Spec `json:"spec" jsonschema:"required,description=Dashboard spec: title; question; assumptions; tiles [{label query_id measure where compare_where compare_label}]; charts [{title type(bar|line) query_id x series measure}]; tables [{title query_id compare{dim baseline current measure}}]; commentary; follow_ups"`
}

func toolText(v any) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	text, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("analytics: encode tool result: %w", err)
		}
		text = string(b)
	}
	return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: text}}, nil
}

// catalog is what list_metrics returns.
func (m *Model) catalog() map[string]any {
	type metricInfo struct {
		Name        string     `json:"name"`
		Label       string     `json:"label"`
		Description string     `json:"description"`
		Synonyms    []string   `json:"synonyms,omitempty"`
		Type        MetricType `json:"type"`
		Dimensions  []string   `json:"dimensions"`
	}
	var metrics []metricInfo
	for _, met := range m.Metrics {
		metrics = append(metrics, metricInfo{met.Name, met.Label, met.Description, met.Synonyms, met.Type, m.AllowedDimensions(met.Name)})
	}
	return map[string]any{
		"metrics": metrics, "dimensions": m.Dimensions, "defaults": m.Defaults,
		"calendar": map[string]int{"fiscal_year_start_month": m.Calendar.FiscalYearStartMonth},
		"units":    "money in major units with exactly its currency's digits (1234.567 for BHD, 5000 for JPY); ratio as a fraction to 4 digits (0.25 = 25%); count as whole numbers",
	}
}

// tools builds the agent's three tools. Their schemas are reflected from the
// fixed argument types above, so construction either always succeeds or
// always fails; TestAgentToolSurface proves it succeeds, which is why the
// constructor errors are not handled at run time.
func (a *Agent) tools(s *Session, built **Report, createdBy string) []anthropic.BetaTool {
	list, _ := toolrunner.NewBetaToolFromJSONSchema("list_metrics",
		"List the governed metrics, dimensions, calendar and defaults for everyday words.",
		func(_ context.Context, _ listArgs) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return toolText(s.Model.catalog())
		})
	run, _ := toolrunner.NewBetaToolFromJSONSchema("run_query",
		"Run a semantic query. Returns query_id, columns, rows (money in major units), data_through and the metric definitions used.",
		func(_ context.Context, in queryArgs) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			q := Query{Measures: in.Measures, By: in.By, OrderBy: in.OrderBy, Desc: in.Desc, Limit: in.Limit}
			for _, f := range in.Where {
				q.Where = append(q.Where, Filter(f))
			}
			r, err := s.Run(q)
			if err != nil {
				return toolText("error: " + err.Error())
			}
			return toolText(r)
		})
	build, _ := toolrunner.NewBetaToolFromJSONSchema("build_dashboard",
		"Validate and publish the dashboard. Returns problems to fix, or the report id when it is accepted.",
		func(_ context.Context, in buildArgs) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			rep, problems := s.Build(in.Spec, createdBy)
			if len(problems) > 0 {
				return toolText("Not accepted. Fix these and call build_dashboard again:\n- " + strings.Join(problems, "\n- "))
			}
			*built = rep
			return toolText("Accepted as report " + rep.ID + ". Reply with a one or two sentence summary.")
		})
	return []anthropic.BetaTool{list, run, build}
}

// Answer is the agent's result: the accepted report and a short summary.
type Answer struct {
	Report  *Report
	Summary string
}

// Ask answers a question for the session's principal.
func (a *Agent) Ask(ctx context.Context, s *Session, question string) (*Answer, error) {
	var built *Report
	tools := a.tools(s, &built, "agent:analytics")
	today := time.Now
	if a.Today != nil {
		today = a.Today
	}
	runner := a.Client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(a.Model),
			MaxTokens: 16000,
			System: []anthropic.BetaTextBlockParam{{
				Text:         analystPrompt,
				CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			}},
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium},
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(fmt.Sprintf(
					"Today is %s. Question from %s:\n%s", today().Format("2 January 2006"), s.Principal.ID, question))),
			},
		},
		MaxIterations: 12,
	})
	msg, err := runner.RunToCompletion(ctx)
	if err != nil {
		return nil, fmt.Errorf("analytics: run agent: %w", err)
	}
	if msg.StopReason == anthropic.BetaStopReasonRefusal {
		return nil, fmt.Errorf("model declined: %s", msg.StopDetails.Explanation)
	}
	if built == nil {
		return nil, fmt.Errorf("the agent did not produce a dashboard")
	}
	var summary strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			summary.WriteString(t.Text)
		}
	}
	if built.Question == "" {
		built.Question = question
		built.Spec.Question = question
	}
	return &Answer{Report: built, Summary: strings.TrimSpace(summary.String())}, nil
}
