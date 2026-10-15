package authn_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/authn"
)

func TestDevelopmentIssuer(t *testing.T) {
	dev := authn.NewDevIssuer()
	v, err := authn.New(context.Background(), authn.Development, nil, dev.Static())
	if err != nil {
		t.Fatal(err)
	}
	ten := uuid.New()
	claims, err := v.Verify(context.Background(), dev.Token(ten, "alice"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.TenantID != ten {
		t.Errorf("tenant: got %v, want %v", claims.TenantID, ten)
	}
	if claims.Subject != "alice" {
		t.Errorf("subject: got %s, want alice", claims.Subject)
	}
	if claims.Issuer != authn.DevIssuerURL {
		t.Errorf("issuer: got %s, want %s", claims.Issuer, authn.DevIssuerURL)
	}
	// Another dev issuer's key is not trusted; an expired token is refused.
	other := authn.NewDevIssuer()
	if _, err := v.Verify(context.Background(), other.Token(ten, "alice")); err == nil {
		t.Fatal("foreign key accepted")
	}
	dev.Now = func() time.Time { return time.Now().Add(-2 * authn.DevTokenTTL) }
	if _, err := v.Verify(context.Background(), dev.Token(ten, "alice")); err == nil {
		t.Fatal("expired token accepted")
	}
	dev.Now = nil

	// The token endpoint.
	h := dev.Handler()
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/dev/token", strings.NewReader(body)))
		return rec
	}
	rec := post(`{"tenant_id": "` + ten.String() + `", "subject": "bob"}`)
	var answer struct {
		Token     string
		ExpiresIn int `json:"expires_in"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if rec.Code != 200 || answer.ExpiresIn != 3600 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if claims, err := v.Verify(context.Background(), answer.Token); err != nil || claims.Subject != "bob" {
		t.Fatalf("%+v %v", claims, err)
	}
	for name, body := range map[string]string{
		"not json":        `{`,
		"tenant not uuid": `{"tenant_id": "x", "subject": "bob"}`,
		"no subject":      `{"tenant_id": "` + ten.String() + `"}`,
		"empty subject":   `{"subject": ""}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(body); rec.Code != http.StatusBadRequest {
				t.Errorf("%s: got %d, want %d", body, rec.Code, http.StatusBadRequest)
			}
		})
	}
	// Without a tenant the token is an operator's.
	rec = post(`{"subject": "ops"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if c, err := v.VerifyOperator(context.Background(), answer.Token); err != nil || c.Subject != "ops" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := v.Verify(context.Background(), answer.Token); err == nil {
		t.Fatal("operator token accepted as a tenant's")
	}
	if _, err := v.VerifyOperator(context.Background(), dev.Token(ten, "alice")); err == nil {
		t.Fatal("tenant token accepted as an operator's")
	}
}
