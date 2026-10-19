package api

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func readFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestEventChangesOfTheContract: the contract compared with itself has no
// change, and every message is keyed by its own event type.
func TestEventChangesOfTheContract(t *testing.T) {
	src := []byte(readFixture(t, "asyncapi.yaml"))
	changes, err := EventChanges(src, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("got changes %v, want none", changes)
	}
	ev, err := parseEvents(src)
	if err != nil {
		t.Fatal(err)
	}
	for typ := range ev.payloads {
		if !strings.HasPrefix(typ, "com.example.erp.") {
			t.Errorf("event %q: got no CloudEvents type, want one from its type header", typ)
		}
	}
}

// TestEventChanges edits the fixture one way at a time and expects exactly
// the breaking changes of each edit (ADR-0041: additive only within .vN).
func TestEventChanges(t *testing.T) {
	base := readFixture(t, "testdata/events/base.yaml")
	const typ = "com.example.erp.things.thing_done.v1"
	for _, c := range []struct {
		name, from, to string
		want           []string
	}{
		{"unchanged", "", "", nil},
		{"optional field added", "        note:", "        added: {type: string}\n        note:", nil},
		{"null order", "[string, 'null']", "['null', string]", nil},
		{"field removed", "        note: {type: [string, 'null']}\n", "", []string{typ + ".note: field removed"}},
		{"type changed", "qty: {type: integer}", "qty: {type: string}", []string{typ + ".lines[].qty: type changed from integer to string"}},
		{"newly required", "required: [thing_id]", "required: [thing_id, note]", []string{typ + ".note: field newly required"}},
		{"ref retyped", "Uuid: {type: string, format: uuid}", "Uuid: {type: integer}", []string{typ + ".thing_id: type changed from string to integer"}},
		{"items removed", "          items:\n            type: object\n            properties:\n              qty: {type: integer}\n", "", []string{typ + ".lines[]: schema removed"}},
		{"variant removed", "            - {type: integer}\n", "", []string{typ + ".party.oneOf[1]: schema removed"}},
		{"message renamed", "    ThingDone:", "    ThingFinished:", nil},
		{"event removed", "thing_done.v1}", "thing_done.v2}", []string{typ + ": event type removed"}},
		{"untyped removed", "    Untyped:", "    Renamed:", []string{"Untyped: event type removed"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			head := strings.Replace(base, c.from, c.to, 1)
			if c.from != "" && head == base {
				t.Fatalf("fixture edit %q matched nothing", c.from)
			}
			got, err := EventChanges([]byte(base), []byte(head))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestEventChangesRefusesUnreadableDocuments(t *testing.T) {
	base := readFixture(t, "testdata/events/base.yaml")
	for _, c := range []struct{ name, base, head string }{
		{"base not yaml", "components: [", base},
		{"head not yaml", base, "components: ["},
		{"unresolved ref", base, strings.Replace(base, "#/components/schemas/Uuid'", "#/components/schemas/Missing'", 1)},
		{"external ref", base, strings.Replace(base, "#/components/schemas/Uuid'", "other.yaml#/Uuid'", 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := EventChanges([]byte(c.base), []byte(c.head)); err == nil {
				t.Error("got no error, want the document refused")
			}
		})
	}
}

// TestEventChangesEndsOnRecursion: a schema that refers to itself is
// followed only so deep.
func TestEventChangesEndsOnRecursion(t *testing.T) {
	src := []byte("components:\n  messages:\n    Loop:\n      payload: {$ref: '#/components/schemas/Node'}\n" +
		"    Cycle:\n      payload: {$ref: '#/components/schemas/Alias'}\n" +
		"  schemas:\n    Node:\n      type: object\n      properties:\n        next: {$ref: '#/components/schemas/Node'}\n" +
		"    Self: {$ref: '#/components/schemas/Self'}\n    Alias: {$ref: '#/components/schemas/Self'}\n")
	changes, err := EventChanges(src, src)
	if err != nil || len(changes) != 0 {
		t.Errorf("got %v, %v; want no change and no error", changes, err)
	}
}
