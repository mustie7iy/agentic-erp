package pgtest

import (
	"bytes"
	"sync"
)

// SyncBuffer is a buffer a running worker or server can log into while the
// test reads it: the reads that Eventually polls race the worker's writes on
// a plain bytes.Buffer. It sits beside Eventually for that reason.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p.
func (s *SyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.buf.Write(p) // bytes.Buffer.Write's error is always nil: it panics when out of memory instead
	return len(p), nil
}

// String returns everything written so far.
func (s *SyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
