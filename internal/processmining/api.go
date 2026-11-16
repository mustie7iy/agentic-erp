package processmining

import (
	"bytes"
	"context"
	"encoding/csv"
	"strconv"
	"time"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/platform/apitypes"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// csvHeader is the CSV form's header row.
var csvHeader = []string{"case_id", "case_kind", "case_document_number", "activity", "occurred_at", "actor_kind", "object_type", "object_id",
	"object_document_number", "seq"}

// GetProcessEventLog implements GET /v1/process-event-logs: a page of the
// log as JSON or CSV.
func (e *Endpoints) GetProcessEventLog(ctx context.Context, params gen.GetProcessEventLogParams) (gen.GetProcessEventLogRes, error) {
	format := params.Format.Or("json")
	if format != "json" && format != "csv" {
		return nil, problem.Unprocessable("request.validation_failed", "The request is not valid: the format is json or csv.",
			problem.FieldError{Field: "format", Code: "invalid", Message: "json|csv"})
	}
	l, err := e.Read(ctx, Query{Process: params.Process, LegalEntity: params.LegalEntityID, From: params.From, To: params.To,
		Size: params.PageSize.Value, SizeSet: params.PageSize.Set, Token: params.PageToken.Value})
	if err != nil {
		return nil, err
	}
	next := apitypes.OptText(l.NextPageToken)
	if format == "csv" {
		return &gen.GetProcessEventLogOKTextCsvHeaders{NextPageToken: next, Response: gen.GetProcessEventLogOKTextCsv{Data: bytes.NewReader(logCSV(l))}}, nil
	}
	return &gen.ProcessEventLogHeaders{NextPageToken: next, Response: logToAPI(l)}, nil
}

// logToAPI is a page of the log in the API's JSON form.
func logToAPI(l Log) gen.ProcessEventLog {
	out := gen.ProcessEventLog{Process: l.Process, LegalEntityID: l.LegalEntity, CompanyCode: l.CompanyCode, From: l.From, To: l.To,
		CaseCount: int32(l.CaseCount), Cases: make([]gen.ProcessCase, len(l.Cases)), NextPageToken: apitypes.OptText(l.NextPageToken)} // #nosec G115 -- a count of one company's documents of at most 371 days
	for i, c := range l.Cases {
		events := make([]gen.ProcessEvent, len(c.Events))
		for j, ev := range c.Events {
			events[j] = gen.ProcessEvent{Activity: ev.Activity, OccurredAt: ev.OccurredAt, ActorKind: ev.ActorKind, ObjectType: ev.Object.Type, ObjectID: ev.Object.ID,
				DocumentNumber: ev.DocumentNumber, Seq: ev.Seq}
		}
		out.Cases[i] = gen.ProcessCase{CaseID: c.ID, CaseKind: c.Kind, DocumentNumber: c.DocumentNumber, DocumentDate: c.DocumentDate, Events: events}
	}
	return out
}

// logCSV is a page of the log as CSV: the header row, then a row per
// event, case by case.
func logCSV(l Log) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.Write(csvHeader) // a bytes.Buffer takes every write
	for _, c := range l.Cases {
		for _, ev := range c.Events {
			_ = w.Write([]string{c.ID.String(), c.Kind, c.DocumentNumber, ev.Activity, ev.OccurredAt.UTC().Format(time.RFC3339Nano), ev.ActorKind, // a bytes.Buffer takes every write
				ev.Object.Type, ev.Object.ID.String(), ev.DocumentNumber, strconv.FormatInt(ev.Seq, 10)})
		}
	}
	w.Flush()
	return b.Bytes()
}

// GetProcessMap implements GET /v1/process-maps: the map of the process
// for the company and period.
func (e *Endpoints) GetProcessMap(ctx context.Context, params gen.GetProcessMapParams) (gen.GetProcessMapRes, error) {
	m, err := e.ReadMap(ctx, MapQuery{Process: params.Process, LegalEntity: params.LegalEntityID, From: params.From, To: params.To})
	if err != nil {
		return nil, err
	}
	out := &gen.ProcessMap{Process: m.Process, LegalEntityID: m.LegalEntity, CompanyCode: m.CompanyCode, From: m.From, To: m.To,
		CaseCount: int32(m.CaseCount), OpenCaseCount: int32(m.Open), VariantCount: int32(m.VariantCount), // #nosec G115 -- counts of one company's documents of at most 371 days
		Activities: make([]gen.ProcessMapActivity, len(m.Activities)), Pairs: make([]gen.ProcessMapPair, len(m.Pairs)), Variants: make([]gen.ProcessMapVariant, len(m.Variants))}
	for i, a := range m.Activities {
		out.Activities[i] = gen.ProcessMapActivity{Activity: a.Activity, EventCount: int32(a.Events), CaseCount: int32(a.CaseCount)} // #nosec G115 -- as above
	}
	for i, p := range m.Pairs {
		out.Pairs[i] = gen.ProcessMapPair{From: p.From, To: p.To, Count: int32(p.Count), MedianSeconds: p.MedianSeconds} // #nosec G115 -- as above
	}
	for i, v := range m.Variants {
		out.Variants[i] = gen.ProcessMapVariant{Activities: v.Activities, CaseCount: int32(v.CaseCount), OpenCaseCount: int32(v.Open), // #nosec G115 -- as above
			SharePercent: v.SharePercent}
	}
	return out, nil
}
