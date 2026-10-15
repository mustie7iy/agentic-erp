package sigv4_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
)

const (
	accessKey = "AKIDEXAMPLE"
	secretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var at = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

// TestKnownAnswer checks the signer against the AWS Signature Version 4
// test suite (region us-east-1, service "service", 2015-08-30T12:36:00Z).
func TestKnownAnswer(t *testing.T) {
	// get-vanilla
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://example.amazonaws.com/", nil)
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", emptyHash, at)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// get-vanilla-query-order-key-case: the query string is sorted and encoded.
	req, _ = http.NewRequestWithContext(t.Context(), "GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", nil)
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", emptyHash, at)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500") {
		t.Fatalf("query: %s", got)
	}
	// post-x-www-form-urlencoded: Content-Type is signed and the payload hash counts.
	body := "Param1=value1"
	req, _ = http.NewRequestWithContext(t.Context(), "POST", "https://example.amazonaws.com/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", sigv4.PayloadHash([]byte(body)), at)
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "SignedHeaders=content-type;host;x-amz-date") ||
		!strings.HasSuffix(got, "Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a") {
		t.Fatalf("form: %s", got)
	}
}

// TestDefaults signs a request built by hand, without Host or path, and
// for s3 sets the content hash header.
func TestDefaults(t *testing.T) {
	req := &http.Request{Method: "GET", URL: &url.URL{Scheme: "https", Host: "example.amazonaws.com"}, Header: http.Header{}}
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", emptyHash, at)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31") {
		t.Fatalf("%s", got)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != "" {
		t.Fatal("content hash header outside s3")
	}
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "s3", emptyHash, at)
	if req.Header.Get("X-Amz-Content-Sha256") != emptyHash {
		t.Fatal("content hash header missing for s3")
	}
}

func TestEncoding(t *testing.T) {
	if got := sigv4.Escape("a b/é~x"); got != "a%20b%2F%C3%A9~x" {
		t.Fatalf("Escape: %s", got)
	}
	if got := sigv4.CanonicalQuery(url.Values{"b": {"2", "1"}, "a": {"x y"}}); got != "a=x%20y&b=1&b=2" {
		t.Fatalf("CanonicalQuery: %s", got)
	}
	if got := sigv4.PayloadHash(nil); got != emptyHash {
		t.Fatalf("PayloadHash: %s", got)
	}
}

// TestPresign checks the presigner against the example of the S3 developer
// guide (query-parameter authentication, 2013-05-24, 86400 s) and the
// verifier against the presigner.
func TestPresign(t *testing.T) {
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	made := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	got := sigv4.Presign(req, "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "us-east-1", "s3", 24*time.Hour, made)
	want := "https://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404&X-Amz-SignedHeaders=host"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// A PUT with a signed Content-Type verifies with that type only.
	req, _ = http.NewRequestWithContext(t.Context(), "PUT", "https://s3.example.com/bucket/tenants/x/uploads/1", nil)
	req.Header.Set("Content-Type", "application/pdf")
	u := sigv4.Presign(req, accessKey, secretKey, "us-east-1", "s3", 15*time.Minute, made)
	sent, _ := http.NewRequestWithContext(t.Context(), "PUT", u, nil)
	sent.Header.Set("Content-Type", "application/pdf")
	ok, at, ttl := sigv4.Verify(sent, accessKey, secretKey, "us-east-1", "s3")
	if !ok {
		t.Fatal("verify: got not ok, want ok")
	}
	if !at.Equal(made) {
		t.Errorf("verify signing time: got %v, want %v", at, made)
	}
	if ttl != 15*time.Minute {
		t.Errorf("verify ttl: got %v, want %v", ttl, 15*time.Minute)
	}
	sent.Header.Set("Content-Type", "image/png")
	if ok, _, _ := sigv4.Verify(sent, accessKey, secretKey, "us-east-1", "s3"); ok {
		t.Fatal("another content type verified")
	}
	sent.Header.Set("Content-Type", "application/pdf")
	for name, mutate := range map[string]func(q url.Values){
		"wrong secret":     func(url.Values) {},
		"no signature":     func(q url.Values) { q.Del("X-Amz-Signature") },
		"bad date":         func(q url.Values) { q.Set("X-Amz-Date", "yesterday") },
		"other credential": func(q url.Values) { q.Set("X-Amz-Credential", "AKIDOTHER/20130524/us-east-1/s3/aws4_request") },
		"tampered":         func(q url.Values) { q.Set("X-Amz-Expires", "999999") },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *sent
			bad.URL = new(url.URL)
			*bad.URL = *sent.URL
			q := bad.URL.Query()
			mutate(q)
			bad.URL.RawQuery = q.Encode()
			secret := secretKey
			if name == "wrong secret" {
				secret = "nope"
			}
			if ok, _, _ := sigv4.Verify(&bad, accessKey, secret, "us-east-1", "s3"); ok {
				t.Error("got verified, want refused")
			}
		})
	}
}

// TestSessionToken: temporary credentials sign and send their token, in the
// header for header auth and in the query for a presigned URL.
func TestSessionToken(t *testing.T) {
	creds := sigv4.Credentials{AccessKey: accessKey, SecretKey: secretKey, SessionToken: "FwoGZXIvYXdzEBYaD"}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://example.amazonaws.com/", nil)
	sigv4.SignWith(req, creds, "us-east-1", "service", emptyHash, at)
	if got := req.Header.Get("X-Amz-Security-Token"); got != creds.SessionToken {
		t.Errorf("X-Amz-Security-Token: got %q, want %q", got, creds.SessionToken)
	}
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "SignedHeaders=host;x-amz-date;x-amz-security-token") {
		t.Errorf("Authorization: got %s, want the security token signed", got)
	}
	req, _ = http.NewRequestWithContext(t.Context(), "PUT", "https://s3.example.com/bucket/key", nil)
	u := sigv4.PresignWith(req, creds, "us-east-1", "s3", time.Minute, at)
	if !strings.Contains(u, "X-Amz-Security-Token=FwoGZXIvYXdzEBYaD") {
		t.Fatalf("%s", u)
	}
	sent, _ := http.NewRequestWithContext(t.Context(), "PUT", u, nil)
	if ok, _, _ := sigv4.Verify(sent, accessKey, secretKey, "us-east-1", "s3"); !ok {
		t.Fatal("a presigned URL with a session token did not verify")
	}
}
