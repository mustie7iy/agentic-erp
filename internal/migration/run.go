package migration

import (
	"context"
	"fmt"

	"github.com/mustie7iy/agentic-erp/internal/core"
	"github.com/mustie7iy/agentic-erp/internal/sap"
)

type Options struct {
	Mode    Mode
	Mapping Mapping
	// Force loads even when data-quality errors were found (rehearsals only).
	Force bool
}

type Result struct {
	Source       string         `json:"source"`
	Scope        sap.Scope      `json:"scope"`
	Mode         Mode           `json:"mode"`
	ExtractCount map[string]int `json:"extractCount"`
	Quality      []Issue        `json:"quality"`
	Transform    []Issue        `json:"transform"`
	Load         []Issue        `json:"load"`
	Loaded       bool           `json:"loaded"`
	Report       *Report        `json:"report,omitempty"`
}

// ErrQualityGate is returned when data-quality errors block the load.
var ErrQualityGate = fmt.Errorf("data-quality errors found; fix them in SAP or rerun with Force")

// Run executes extract -> quality gate -> transform -> load -> reconcile.
// A scope may only be cut over when the returned report has Passed.
func Run(ctx context.Context, src sap.Source, scope sap.Scope, opts Options, s *core.Store) (*Result, error) {
	if opts.Mode == "" {
		opts.Mode = ModeDocuments
	}
	if opts.Mapping.AccountTypes == nil {
		opts.Mapping = DefaultMapping()
	}
	res := &Result{Source: src.Name(), Scope: scope, Mode: opts.Mode}

	ext, err := src.Extract(ctx, scope)
	if err != nil {
		return res, fmt.Errorf("extract: %w", err)
	}
	res.ExtractCount = ext.Counts()

	res.Quality = CheckQuality(ext, opts.Mapping)
	if HasErrors(res.Quality) && !opts.Force {
		return res, ErrQualityGate
	}

	ds, issues := Transform(ext, opts.Mapping, opts.Mode)
	res.Transform = issues
	res.Load = Load(s, ds)
	res.Loaded = true
	res.Report = Reconcile(ext, s, scope, opts.Mapping, opts.Mode)
	return res, nil
}
