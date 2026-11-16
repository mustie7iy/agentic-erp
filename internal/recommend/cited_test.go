package recommend_test

import (
	"fmt"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/recommend"
)

// TestCited: a run keeps the identifiers its reads' answers hold at the
// paths an agent names, under their labels: a whole number as written,
// a UUID in any case, an array's every element; an answer that is not a
// success or not JSON keeps nothing. A content's citations are its labels
// followed by an identifier, in its order, and those no read returned are
// unknown (T105f, T105g).
func TestCited(t *testing.T) {
	c := recommend.NewCited()
	c.Keep("audit event", "items[].seq")(apitools.Response{Status: 200, Body: []byte(`{"items": [{"seq": 41}, {"seq": 12345678901234}, {"seq": null}]}`)})
	c.Keep("control test", "items[].last_test.id", "last_test.id", "items.id", "none[].id")(apitools.Response{Status: 200,
		Body: []byte(`{"items": [{"last_test": {"id": "01A1AA00-0000-7000-8000-000000000E01"}}, {"code": "never tested"}], "last_test": {"id": "01a1aa00-0000-7000-8000-000000000e02"}, "none": 5}`)})
	c.Keep("control test", "id")(apitools.Response{Status: 404, Body: []byte(`{"id": "01a1aa00-0000-7000-8000-000000000e03"}`)})
	c.Keep("control test", "id")(apitools.Response{Status: 200, Body: []byte(`not JSON 01a1aa00-0000-7000-8000-000000000e04`)})
	text := "Control test 01a1aa00-0000-7000-8000-000000000E02 failed after audit event 41 and AUDIT EVENT 12345678901234; " +
		"audit event 7 and control test 01a1aa00-0000-7000-8000-000000000e03, control test 01a1aa00-0000-7000-8000-000000000e04 and " +
		"control test 01a1aa00-0000-7000-8000-000000000e01; an audit event, a control test of none."
	cites := recommend.Cites(text, "audit event", "control test")
	want := "[control test 01a1aa00-0000-7000-8000-000000000e02 audit event 41 audit event 12345678901234 audit event 7 " +
		"control test 01a1aa00-0000-7000-8000-000000000e03 control test 01a1aa00-0000-7000-8000-000000000e04 control test 01a1aa00-0000-7000-8000-000000000e01]"
	if got := fmt.Sprint(cites); got != want {
		t.Errorf("citations: got %s, want %s", got, want)
	}
	want = "[audit event 7 control test 01a1aa00-0000-7000-8000-000000000e03 control test 01a1aa00-0000-7000-8000-000000000e04]"
	if got := fmt.Sprint(c.Unknown(cites)); got != want {
		t.Errorf("unknown: got %s, want %s", got, want)
	}
	if got := recommend.Cites("no citation", "audit event"); len(got) != 0 {
		t.Errorf("citations of a text without: got %v, want none", got)
	}
}
