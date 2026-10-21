// Package webhook signs and verifies webhooks as Standard Webhooks
// defines them (ADR-0041): webhook-id, webhook-timestamp and
// webhook-signature, the signature `v1,<base64 HMAC-SHA256 of
// id.timestamp.body>` under a `whsec_` secret, several separated by spaces
// while a secret is rotated. The vectors in api/webhooks-test-vectors.json
// fix the format for the ERP and for subscribers.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
)

// The headers of a signed webhook.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// DefaultTolerance is how far a webhook's timestamp may be from the
// verifier's clock, either way: the replay window of Standard Webhooks.
const DefaultTolerance = 5 * time.Minute

// secretPrefix marks a Standard Webhooks secret; the rest is its key in
// standard base64.
const secretPrefix = "whsec_"

// The key lengths Standard Webhooks allows.
const (
	minKey = 24
	maxKey = 64
)

// Why a webhook is not verified.
var (
	ErrHeaders   = errors.New("webhook: id, timestamp or signature header missing")
	ErrTimestamp = errors.New("webhook: timestamp invalid or outside the tolerance")
	ErrSignature = errors.New("webhook: no signature matches")
)

// Secret is the key of one subscription's secret.
type Secret struct{ key []byte }

// ParseSecret reads a `whsec_` secret.
func ParseSecret(s string) (Secret, error) {
	encoded, ok := strings.CutPrefix(s, secretPrefix)
	if !ok {
		return Secret{}, errors.New("webhook: a secret starts with whsec_")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return Secret{}, fmt.Errorf("webhook: secret is not base64: %w", err)
	}
	if len(key) < minKey || len(key) > maxKey {
		return Secret{}, fmt.Errorf("webhook: secret has %d bytes, want %d to %d", len(key), minKey, maxKey)
	}
	return Secret{key: key}, nil
}

// String is the secret as subscribers are given it.
func (s Secret) String() string {
	return secretPrefix + base64.StdEncoding.EncodeToString(s.key)
}

// mac is the raw signature of id, timestamp and body under s.
func (s Secret) mac(id, timestamp string, body []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(id + "." + timestamp + ".")) // hash writes never fail
	m.Write(body)
	return m.Sum(nil)
}

// Sign is the webhook-signature value for id, at, body: one v1 signature
// per secret, in order, so during a rotation a subscriber holding either
// secret verifies it. The caller passes at least one secret: with none the
// value is empty and every verifier refuses it.
func Sign(id string, at time.Time, body []byte, secrets ...Secret) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	sigs := make([]string, len(secrets))
	for i, s := range secrets {
		sigs[i] = "v1," + base64.StdEncoding.EncodeToString(s.mac(id, ts, body))
	}
	return strings.Join(sigs, " ")
}

// SetHeaders sets the three headers of a webhook signed with secrets.
func SetHeaders(h http.Header, id string, at time.Time, body []byte, secrets ...Secret) {
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, strconv.FormatInt(at.Unix(), 10))
	h.Set(HeaderSignature, Sign(id, at, body, secrets...))
}

// Verifier checks webhooks against the secrets a subscriber holds.
type Verifier struct {
	Secrets   []Secret
	Tolerance time.Duration    // 0: DefaultTolerance
	Now       func() time.Time // nil: the wall clock
}

// Verify accepts a webhook whose timestamp is within the tolerance and one
// of whose v1 signatures is made with one of the secrets; signatures of
// other versions are ignored.
func (v Verifier) Verify(h http.Header, body []byte) error {
	id, ts, sigs := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || ts == "" || sigs == "" {
		return ErrHeaders
	}
	if err := v.checkTimestamp(ts); err != nil {
		return err
	}
	for _, s := range v.Secrets {
		want := s.mac(id, ts, body)
		for _, sig := range strings.Fields(sigs) {
			if got, ok := decodeV1(sig); ok && hmac.Equal(got, want) {
				return nil
			}
		}
	}
	return ErrSignature
}

// checkTimestamp accepts decimal Unix seconds within the tolerance of now.
func (v Verifier) checkTimestamp(ts string) error {
	if strings.TrimLeft(ts, "0123456789") != "" {
		return ErrTimestamp
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrTimestamp
	}
	tolerance := v.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	d := clock.Now(v.Now).Sub(time.Unix(sec, 0))
	if d > tolerance || d < -tolerance {
		return ErrTimestamp
	}
	return nil
}

// decodeV1 is the raw signature of a `v1,<base64>` entry.
func decodeV1(sig string) ([]byte, bool) {
	encoded, ok := strings.CutPrefix(sig, "v1,")
	if !ok {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return raw, err == nil
}
