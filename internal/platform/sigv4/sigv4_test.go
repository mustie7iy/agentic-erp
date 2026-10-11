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
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", emptyHash, at)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// get-vanilla-query-order-key-case: the query string is sorted and encoded.
	req, _ = http.NewRequest("GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", nil)
	sigv4.Sign(req, accessKey, secretKey, "us-east-1", "service", emptyHash, at)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500") {
		t.Fatalf("query: %s", got)
	}
	// post-x-www-form-urlencoded: Content-Type is signed and the payload hash counts.
	body := "Param1=value1"
	req, _ = http.NewRequest("POST", "https://example.amazonaws.com/", strings.NewReader(body))
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
