package webhook_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
)

// vectors is api/webhooks-test-vectors.json.
type vectors struct {
	ToleranceSeconds int64 `json:"tolerance_seconds"`
	Verify           []struct {
		Name      string   `json:"name"`
		Secrets   []string `json:"secrets"`
		ID        string   `json:"id"`
		Timestamp string   `json:"timestamp"`
		Payload   string   `json:"payload"`
		Now       int64    `json:"now"`
		Signature string   `json:"signature"`
		Valid     bool     `json:"valid"`
		Error     string   `json:"error"`
	} `json:"verify"`
	Sign []struct {
		Name      string   `json:"name"`
		Secrets   []string `json:"secrets"`
		ID        string   `json:"id"`
		Timestamp int64    `json:"timestamp"`
		Payload   string   `json:"payload"`
		Signature string   `json:"signature"`
	} `json:"sign"`
}

func readVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("../../../api/webhooks-test-vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(v.Verify) == 0 || len(v.Sign) == 0 {
		t.Fatal("vectors: got none, want verify and sign cases")
	}
	return v
}

func secrets(t *testing.T, ss []string) []webhook.Secret {
	t.Helper()
	out := make([]webhook.Secret, len(ss))
	for i, s := range ss {
		sec, err := webhook.ParseSecret(s)
		if err != nil {
			t.Fatalf("secret %d: %v", i, err)
		}
		if sec.String() != s {
			t.Errorf("secret %d: got %q back, want %q", i, sec.String(), s)
		}
		out[i] = sec
	}
	return out
}

var reasons = map[string]error{"headers": webhook.ErrHeaders, "timestamp": webhook.ErrTimestamp, "signature": webhook.ErrSignature}

// TestVectorsVerify: every committed vector verifies, or fails for its
// reason.
func TestVectorsVerify(t *testing.T) {
	v := readVectors(t)
	for _, c := range v.Verify {
		t.Run(c.Name, func(t *testing.T) {
			h := http.Header{}
			h.Set(webhook.HeaderID, c.ID)
			h.Set(webhook.HeaderTimestamp, c.Timestamp)
			h.Set(webhook.HeaderSignature, c.Signature)
			ver := webhook.Verifier{Secrets: secrets(t, c.Secrets), Tolerance: time.Duration(v.ToleranceSeconds) * time.Second,
				Now: func() time.Time { return time.Unix(c.Now, 0) }}
			err := ver.Verify(h, []byte(c.Payload))
			var want error
			if !c.Valid {
				want = reasons[c.Error]
			}
			if !errors.Is(err, want) || (want == nil) != c.Valid {
				t.Errorf("got %v, want %v", err, want)
			}
		})
	}
}

// TestVectorsSign: signing reproduces every committed signature, one per
// secret while rotating, and SetHeaders sets the three headers.
func TestVectorsSign(t *testing.T) {
	for _, c := range readVectors(t).Sign {
		t.Run(c.Name, func(t *testing.T) {
			at := time.Unix(c.Timestamp, 0)
			s := secrets(t, c.Secrets)
			if got := webhook.Sign(c.ID, at, []byte(c.Payload), s...); got != c.Signature {
				t.Errorf("signature: got %q, want %q", got, c.Signature)
			}
			h := http.Header{}
			webhook.SetHeaders(h, c.ID, at, []byte(c.Payload), s...)
			ver := webhook.Verifier{Secrets: s[len(s)-1:], Now: func() time.Time { return at.Add(webhook.DefaultTolerance) }}
			if err := ver.Verify(h, []byte(c.Payload)); err != nil {
				t.Errorf("verify own headers: got %v, want nil", err)
			}
		})
	}
}

// TestParseSecret: only whsec_ secrets of 24 to 64 base64 bytes.
func TestParseSecret(t *testing.T) {
	for _, c := range []struct{ name, secret, want string }{
		{"no prefix", "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "starts with whsec_"},
		{"not base64", "whsec_***", "not base64"},
		{"too short", "whsec_" + strings.Repeat("A", 28), "want 24 to 64"},
		{"too long", "whsec_" + strings.Repeat("A", 88), "want 24 to 64"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := webhook.ParseSecret(c.secret); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// TestVerifyTimestamp: a timestamp is plain decimal seconds that fit, and
// the wall clock and default tolerance apply when none is set.
func TestVerifyTimestamp(t *testing.T) {
	s, err := webhook.ParseSecret("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	now := time.Now()
	for _, c := range []struct {
		name, ts string
		want     error
	}{
		{"now, wall clock", "", nil},
		{"signed", "+1614265330", webhook.ErrTimestamp},
		{"overflows", "99999999999999999999", webhook.ErrTimestamp},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			webhook.SetHeaders(h, "msg_1", now, nil, s)
			if c.ts != "" {
				h.Set(webhook.HeaderTimestamp, c.ts)
			}
			if err := (webhook.Verifier{Secrets: []webhook.Secret{s}}).Verify(h, nil); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

// FuzzVerify: no header value makes Verify panic, and a webhook it accepts
// carries a v1 signature.
func FuzzVerify(f *testing.F) {
	f.Add("msg_1", "1614265330", "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=", []byte(`{"test": 2432232314}`))
	f.Add("msg_1", "1614265330", "v1a,abc v2,xyz v1,", []byte{})
	f.Add("", "-1", "v1,====  v1,AAAA", []byte("x"))
	s, err := webhook.ParseSecret("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if err != nil {
		f.Fatalf("secret: %v", err)
	}
	ver := webhook.Verifier{Secrets: []webhook.Secret{s}, Now: func() time.Time { return time.Unix(1614265330, 0) }}
	f.Fuzz(func(t *testing.T, id, ts, sig string, body []byte) {
		h := http.Header{}
		h.Set(webhook.HeaderID, id)
		h.Set(webhook.HeaderTimestamp, ts)
		h.Set(webhook.HeaderSignature, sig)
		if ver.Verify(h, body) == nil && !strings.Contains(sig, "v1,") {
			t.Errorf("accepted %q without a v1 signature", sig)
		}
	})
}
