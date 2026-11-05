package closeagent

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// TestRecord: an accepted answer is kept once by its proposal's id; a
// refusal, an answer that is no proposal and one without an id are counted
// refused.
func TestRecord(t *testing.T) {
	var r Result
	for _, resp := range []apitools.Response{
		{Status: 201, Body: []byte(`{"id": "p1", "parked_entry_id": "e1"}`)},
		{Status: 200, Body: []byte(`{"id": "p1", "parked_entry_id": "e1"}`)},
		{Status: 409, Body: []byte(`{"code": "closing.accrual_duplicate"}`)},
		{Status: 201, Body: []byte(`not json`)},
		{Status: 201, Body: []byte(`{}`)},
	} {
		r.record(resp)
	}
	if len(r.Proposals) != 1 {
		t.Fatalf("proposals: got %v, want one", r.Proposals)
	}
	if got, want := r.Proposals[0], (Proposal{ID: "p1", ParkedEntryID: "e1"}); got != want {
		t.Errorf("proposal: got %v, want %v", got, want)
	}
	if r.Refused != 3 {
		t.Errorf("refused: got %d, want 3", r.Refused)
	}
}
