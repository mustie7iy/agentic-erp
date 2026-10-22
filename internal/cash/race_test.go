//go:build race

package cash_test

// raceDetector is set when the tests run under the race detector, which
// slows Go code down too much for performance budgets to mean anything.
const raceDetector = true
