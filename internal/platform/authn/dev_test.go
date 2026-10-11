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
	v, err := authn.New(context.Background(), nil, dev.Static())
	if err != nil {
		t.Fatal(err)
	}
	ten := uuid.New()
	claims, err := v.Verify(context.Background(), dev.Token(ten, "alice"))
	if err != nil || claims.TenantID != ten || claims.Subject != "alice" || claims.Issuer != authn.DevIssuerURL {
		t.Fatalf("%+v %v", claims, err)
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
	json.Unmarshal(rec.Body.Bytes(), &answer)
	if rec.Code != 200 || answer.ExpiresIn != 3600 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if claims, err := v.Verify(context.Background(), answer.Token); err != nil || claims.Subject != "bob" {
		t.Fatalf("%+v %v", claims, err)
	}
	for _, body := range []string{`{`, `{"tenant_id": "x", "subject": "bob"}`, `{"tenant_id": "` + ten.String() + `"}`} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
}
