package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPayingBankOfNeither: a payment that names no house bank in a run
// without one is broken data (a run proposed without a bank names one on
// each payment), an error rather than a guess; the database is not asked.
func TestPayingBankOfNeither(t *testing.T) {
	_, err := payingBank(context.Background(), nil, Run{ID: uuid.New()}, Payment{ID: uuid.New()})
	if err == nil || !strings.Contains(err.Error(), "names no house bank") {
		t.Errorf("got %v, want the payment refused", err)
	}
}

// TestMessageIDsOfLongCodes: with the longest codes a run's files still
// have message ids of their own within pain.001's 35 characters, ending in
// their bank and method, and two runs whose ids share a long prefix differ.
func TestMessageIDsOfLongCodes(t *testing.T) {
	transfer, cheque := "T", "S"
	bank := houseBank{Code: "HSBC1"}
	ids := map[string]bool{}
	for _, number := range []string{"PAYRUN-26-1234567", "PAYRUN-26-1234568"} {
		for _, m := range []*string{&transfer, &cheque} {
			id := fileGroup{bank: bank, v: via{method: m}}.messageID("ACMEGMBH01", Run{DocumentNumber: number})
			if len(id) > maxMessageID {
				t.Errorf("%s: got %d characters, want at most %d", id, len(id), maxMessageID)
			}
			if !strings.HasSuffix(id, "-HSBC1-"+*m) {
				t.Errorf("%s: got no bank and method at its end, want -HSBC1-%s", id, *m)
			}
			ids[id] = true
		}
	}
	if len(ids) != 4 {
		t.Errorf("message ids: got %d distinct, want 4: %v", len(ids), ids)
	}
	if got := (fileGroup{bank: bank, v: via{method: &transfer}}).messageID("2000", Run{DocumentNumber: "PR1"}); got != "2000-PR1-HSBC1-T" {
		t.Errorf("short id: got %s, want 2000-PR1-HSBC1-T", got)
	}
}
