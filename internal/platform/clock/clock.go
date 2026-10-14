// Package clock reads the time through an injectable clock: workers and
// services hold a Now field, nil in production, that tests set to a fixed or
// advancing time instead of sleeping (the fake clock of
// 06-engineering-process §6.6; CLAUDE.md, tests). It replaces the
// `if x.Now != nil` fallback each of them used to repeat.
package clock

import "time"

// Now returns now() when a clock was injected and the wall clock otherwise.
func Now(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}
