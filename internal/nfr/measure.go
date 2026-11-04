package nfr

import (
	"os"
	"testing"
)

// MeasureEnv is the variable scripts/budgets.sh sets: the performance
// budget tests measure only when it is set.
const MeasureEnv = "ERP_MEASURE_BUDGETS"

// Measure skips a performance budget test unless the budgets gate runs it:
// scripts/budgets.sh (make perf, ci-local, CI's full run on main), which
// sets MeasureEnv and runs the budget tests one package at a time, at the
// gate's ERP_BUDGET_SCALE. A wall-clock budget measured anywhere else, in
// a plain go test of a package beside others at a load average of 25,
// fails on the machine and not on the code (TestReportBudgets: p95 47 ms
// against 30 ms; batch 52). -short and the race detector skip it as well:
// the race detector slows Go code down too much for a budget to mean
// anything.
func Measure(tb testing.TB) {
	tb.Helper()
	switch {
	case testing.Short() || raceDetector:
		tb.Skip("performance budget: runs without -short and without the race detector (make perf, CI)")
	case os.Getenv(MeasureEnv) == "":
		tb.Skip("performance budget: measured only by scripts/budgets.sh (make perf, ci-local, CI), one package at a time")
	}
}
