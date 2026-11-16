// Package jsonl writes records as JSON Lines in parts of bounded size, so
// that a file of any length is built with one part in memory at a time:
// the audit context's evidence exports (ADR-0011 As built) and the records
// context's archive files (ADR-0079 §7) write each part to object storage
// as it fills, and serve it as a file of its own.
package jsonl

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Writer encodes records, one JSON value a line, HTML characters as they
// are, into a part until it holds MaxRecords records or MaxBytes bytes;
// then it hands the part to Flush and starts the next. Flush gets the
// part's bytes, valid until it returns, and its record count.
type Writer struct {
	MaxRecords, MaxBytes int
	Flush                func(content []byte, records int32) error
	buf                  bytes.Buffer
	enc                  *json.Encoder
	n                    int32
	parts                int
}

// New is a Writer of parts of at most maxRecords records and maxBytes
// bytes (a part whose last record passes maxBytes ends with it).
func New(maxRecords, maxBytes int, flush func(content []byte, records int32) error) *Writer {
	w := &Writer{MaxRecords: maxRecords, MaxBytes: maxBytes, Flush: flush}
	w.enc = json.NewEncoder(&w.buf)
	w.enc.SetEscapeHTML(false)
	return w
}

// Add writes a record, and hands the part on once it is full.
func (w *Writer) Add(v any) error {
	if err := w.enc.Encode(v); err != nil {
		return fmt.Errorf("jsonl: encode a record: %w", err)
	}
	w.n++
	if int(w.n) >= w.MaxRecords || w.buf.Len() >= w.MaxBytes {
		return w.flush()
	}
	return nil
}

// Close hands on the last part: the records not handed on yet, or, when
// no record was written at all, one empty part.
func (w *Writer) Close() error {
	if w.n == 0 && w.parts > 0 {
		return nil
	}
	return w.flush()
}

// Parts is how many parts were handed on.
func (w *Writer) Parts() int { return w.parts }

func (w *Writer) flush() error {
	if err := w.Flush(w.buf.Bytes(), w.n); err != nil {
		return err
	}
	w.parts++
	w.buf.Reset()
	w.n = 0
	return nil
}
