package procurement

import (
	"strings"
	"testing"
)

// TestSourcedKeys: a requisition item keeps its catalog key as the RFQ
// item's material key when no other item has it; one without a key, or
// sharing it, is keyed by the requisition's number and its item number,
// so each key is once in the RFQ (T82e).
func TestSourcedKeys(t *testing.T) {
	r := Requisition{DocumentNumber: "PR100007", Items: []Item{
		{No: 1, MaterialKey: "CHAIR"}, {No: 2}, {No: 3, MaterialKey: "DESK"}, {No: 4, MaterialKey: "DESK"},
	}}
	var keys []string
	for _, in := range sourcedInputs(r) {
		keys = append(keys, in.MaterialKey)
	}
	if got, want := strings.Join(keys, " "), "CHAIR PR100007/2 PR100007/3 PR100007/4"; got != want {
		t.Errorf("material keys: got %s, want %s", got, want)
	}
}
