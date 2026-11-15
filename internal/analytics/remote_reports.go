package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// ReportsPath is erp-server's saved reports (createReport, ADR-0076).
const ReportsPath = "/v1/reports"

// SavedReport is a report as erp-server saved it: its id there and its
// specification's title.
type SavedReport struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// SaveReport saves rep's specification and queries in erp-server as the
// API client's principal's report (createReport), which checks them as
// Build does.
func (r Remote) SaveReport(ctx context.Context, rep *Report) (SavedReport, error) {
	resp, err := r.API.Do(ctx, http.MethodPost, ReportsPath, nil, map[string]any{"spec": rep.Spec, "queries": rep.Queries}, "")
	if err != nil {
		return SavedReport{}, fmt.Errorf("analytics: save report: %w", err)
	}
	var out SavedReport
	if err := resp.Decode(&out); err != nil {
		return SavedReport{}, fmt.Errorf("analytics: save report: %w", err)
	}
	return out, nil
}

// RunReport runs the principal's report saved in erp-server under id on
// current data (runReport): the report as answered, with its results by
// the ids its specification names them by, and the latest date in their
// data.
func (r Remote) RunReport(ctx context.Context, id string) (*Report, map[string]*Result, string, error) {
	path := ReportsPath + "/" + url.PathEscape(id)
	resp, err := r.API.Do(ctx, http.MethodGet, path, nil, nil, "")
	if err == nil {
		err = resp.Err()
	}
	if err == nil {
		resp, err = r.API.Do(ctx, http.MethodPost, path+":run", nil, nil, resp.ETag)
	}
	if err == nil {
		err = resp.Err()
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("analytics: run report %s: %w", id, err)
	}
	var out struct {
		Title       string            `json:"title"`
		Spec        Spec              `json:"spec"`
		Results     []json.RawMessage `json:"results"`
		DataThrough string            `json:"data_through"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, nil, "", fmt.Errorf("analytics: decode report %s: %w", id, err)
	}
	rep := &Report{ID: id, Question: out.Spec.Question, Spec: out.Spec, Queries: map[string]Query{}}
	results := map[string]*Result{}
	for _, raw := range out.Results {
		res, err := decodeResult(raw)
		if err != nil {
			return nil, nil, "", fmt.Errorf("analytics: report %s: %w", id, err)
		}
		results[res.ID] = res
	}
	return rep, results, out.DataThrough, nil
}
