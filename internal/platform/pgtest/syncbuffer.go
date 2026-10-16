package pgtest

import (
	"bytes"
	"sync"
)

// SyncBuffer is a buffer a running worker or server can log into while the
// test reads it: the reads that Eventually polls race the worker's writes on
// a plain bytes.Buffer. It sits beside Eventually for that reason.
//
// Limit, when above zero, bounds what it keeps for a server that logs for
// hours (the soak): once the buffer holds more than twice Limit, it drops
// all but the last Limit bytes, from the first line that starts in them.
// Dropping in halves keeps the cost of a write constant on average. It then
// holds at most twice Limit, in a backing array of about four times Limit.
type SyncBuffer struct {
	Limit int
	mu    sync.Mutex
	buf   bytes.Buffer
}

// Write appends p.
func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.buf.Write(p) // bytes.Buffer.Write's error is always nil: it panics when out of memory instead
	if s.Limit > 0 && s.buf.Len() > 2*s.Limit {
		b := s.buf.Bytes()
		cut := len(b) - s.Limit
		// Move the cut to the next line start. A tail in which no line
		// starts (one line longer than Limit) is kept from the cut, so the
		// latest write is never dropped.
		if i := bytes.IndexByte(b[cut-1:], '\n'); i >= 0 && cut+i < len(b) {
			cut += i
		}
		s.buf.Next(cut)
	}
	return len(p), nil
}

// String returns what was written, or its latest part under a Limit.
func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
