// Package pagination implements cursor pagination (04-api-design §4.5):
// opaque, signed page tokens that encode the sort keys of the last row and
// are bound to the filters and order they were issued for.
package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

const (
	DefaultSize = 50
	MaxSize     = 500
)

// Size validates a requested page size; zero means the default.
func Size(requested int32, set bool) (int, error) {
	if !set {
		return DefaultSize, nil
	}
	if requested < 1 || requested > MaxSize {
		return 0, problem.BadRequest("request.invalid_page_size", "page_size must be between 1 and 500.",
			problem.FieldError{Field: "page_size", Code: "out_of_range", Message: "between 1 and 500"})
	}
	return int(requested), nil
}

// Codec signs and verifies page tokens.
type Codec struct{ key []byte }

// NewCodec needs a secret of at least 32 bytes, shared by all replicas.
func NewCodec(key []byte) (*Codec, error) {
	if len(key) < 32 {
		return nil, errors.New("pagination: key must be at least 32 bytes")
	}
	return &Codec{key: key}, nil
}

func (c *Codec) sign(scope string, payload []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(scope))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)
}

// Encode makes a token for cursor v. scope names the listing and its
// filters, so a token cannot be replayed against different ones.
func (c *Codec) Encode(scope string, v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(c.sign(scope, payload)), nil
}

// Decode verifies a token for scope and decodes its cursor into v.
func (c *Codec) Decode(scope, token string, v any) error {
	bad := problem.BadRequest("request.invalid_page_token",
		"page_token is not valid for this listing; start again without it.",
		problem.FieldError{Field: "page_token", Code: "invalid", Message: "unknown, tampered or issued for other filters"})
	p64, s64, ok := strings.Cut(token, ".")
	enc := base64.RawURLEncoding
	payload, err1 := enc.DecodeString(p64)
	sig, err2 := enc.DecodeString(s64)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(sig, c.sign(scope, payload)) {
		return bad
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return bad.WithCause(err)
	}
	return nil
}
