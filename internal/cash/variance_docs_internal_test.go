package cash

import (
	"fmt"
	"testing"
)

// TestRefOf: a matching key names its document, a series' occurrence its
// day; a key naming none, as a snapshot line without a document has, or
// one not written by the matching, names nothing.
func TestRefOf(t *testing.T) {
	for key, want := range map[string]string{
		"payment:01a1aa00-0000-7000-8000-000000000c01":                "payment 01a1aa00-0000-7000-8000-000000000c01 <nil> true",
		"memo_record:01a1aa00-0000-7000-8000-000000000c02@2026-04-24": "memo_record 01a1aa00-0000-7000-8000-000000000c02 2026-04-24 true",
		"":                   " 00000000-0000-0000-0000-000000000000 <nil> false",
		"journal_entry:nope": " 00000000-0000-0000-0000-000000000000 <nil> false",
		"memo_record:01a1aa00-0000-7000-8000-000000000c02@later": "memo_record 01a1aa00-0000-7000-8000-000000000c02 <nil> true",
	} {
		t.Run(key, func(t *testing.T) {
			ref, ok := refOf(key)
			day := "<nil>"
			if ref.Occurrence != nil {
				day = ref.Occurrence.Format("2006-01-02")
			}
			if got := fmt.Sprint(ref.Kind, " ", ref.ID, " ", day, " ", ok); got != want {
				t.Errorf("got %s, want %s", got, want)
			}
		})
	}
}

// TestInKeyOrder: the entries are sorted by key, and two of one key are
// refused, as paging after a key would list one of them never or the page
// for ever.
func TestInKeyOrder(t *testing.T) {
	entries := []VarianceEntry{{key: "1|b"}, {key: "0|a"}, {key: "1|a"}}
	if err := inKeyOrder(entries); err != nil {
		t.Fatalf("got %v, want no error", err)
	}
	if got := fmt.Sprint(entries[0].key, " ", entries[1].key, " ", entries[2].key); got != "0|a 1|a 1|b" {
		t.Errorf("got %s, want 0|a 1|a 1|b", got)
	}
	err := inKeyOrder([]VarianceEntry{{key: "1|b"}, {key: "0|a"}, {key: "1|b"}})
	if got, want := fmt.Sprint(err), "cash: two documents behind a variance have one cursor key 1|b"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}
