package jsonl_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/jsonl"
)

// TestWriter: records go into parts of at most the records and bytes
// given, a part full on its last record handed on at once and no empty
// part after it; nothing written is one empty part.
func TestWriter(t *testing.T) {
	for _, c := range []struct {
		name             string
		records, maxRecs int
		maxBytes         int
		want             string
	}{
		{"none", 0, 2, 1000, "0:"},
		{"one part", 1, 2, 1000, `1:"<a&>"|`},
		{"full parts", 4, 2, 1000, `2:"<a&>"|"<a&>"|,2:"<a&>"|"<a&>"|`},
		{"a last part", 3, 2, 1000, `2:"<a&>"|"<a&>"|,1:"<a&>"|`},
		{"by bytes", 2, 10, 5, `1:"<a&>"|,1:"<a&>"|`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var parts []string
			w := jsonl.New(c.maxRecs, c.maxBytes, func(content []byte, records int32) error {
				parts = append(parts, fmt.Sprintf("%d:%s", records, strings.ReplaceAll(string(content), "\n", "|")))
				return nil
			})
			for range c.records {
				if err := w.Add("<a&>"); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(parts, ","); got != c.want {
				t.Errorf("parts: got %s, want %s", got, c.want)
			}
			if w.Parts() != len(parts) {
				t.Errorf("Parts: got %d, want %d", w.Parts(), len(parts))
			}
		})
	}
}

// TestWriterFailures: a record that does not encode and a part the flush
// does not take fail the write with their error.
func TestWriterFailures(t *testing.T) {
	down := errors.New("store down")
	w := jsonl.New(1, 1000, func([]byte, int32) error { return down })
	if err := w.Add(make(chan int)); err == nil || !strings.Contains(err.Error(), "encode") {
		t.Errorf("an unencodable record: got %v, want the encoder's error", err)
	}
	if err := w.Add(1); !errors.Is(err, down) {
		t.Errorf("a part not taken: got %v, want %v", err, down)
	}
	if err := w.Close(); !errors.Is(err, down) {
		t.Errorf("the last part not taken: got %v, want %v", err, down)
	}
}
