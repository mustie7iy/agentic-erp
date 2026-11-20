package einvoicing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestSendBounds: the status reads' backoff doubles from 30 seconds to an
// hour; a detail is clipped to its column at a character boundary; the
// submissions of a page are read in the request's transaction alone.
func TestSendBounds(t *testing.T) {
	for polls, want := range map[int32]time.Duration{0: 30 * time.Second, 1: time.Minute, 6: 32 * time.Minute, 7: time.Hour, 400: time.Hour} {
		if got := pollWait(polls); got != want {
			t.Errorf("pollWait(%d): got %v, want %v", polls, got, want)
		}
	}
	if got := clipBytes("short", 10); got != "short" {
		t.Errorf("clip of a short text: got %q, want it whole", got)
	}
	if got := clipBytes(strings.Repeat("é", 6), 4); got != "éé" {
		t.Errorf("clip on a boundary: got %q, want two characters", got)
	}
	if got := clipBytes(strings.Repeat("é", 6), 5); got != "éé" {
		t.Errorf("clip at a character boundary: got %q, want two characters", got)
	}
	if _, err := submissions(context.Background(), []uuid.UUID{uuid.New()}); err == nil {
		t.Error("submissions outside a request: got nil error, want an error")
	}
}

// TestRequestMade: a retry is made once the tax data document is reported
// and no longer TDD_FAILED, never on none reported; a withdrawal once it
// is WITHDRAW_SEND (As built E4).
func TestRequestMade(t *testing.T) {
	for _, c := range []struct {
		request, status string
		want            bool
	}{
		{TaxDataRetry, TaxDataFailed, false}, {TaxDataRetry, "", false}, {TaxDataRetry, TaxDataSent, true},
		{TaxDataWithdraw, TaxDataApproved, false}, {TaxDataWithdraw, "", false}, {TaxDataWithdraw, TaxDataWithdrawSent, true},
	} {
		t.Run(c.request+" "+c.status, func(t *testing.T) {
			if got := requestMade(c.request, c.status); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
