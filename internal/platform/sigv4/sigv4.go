// Package sigv4 signs HTTP requests with AWS Signature Version 4, for the
// S3-compatible object store (blobstore) and the KMS master key (fieldcrypt).
// Written here rather than taken from the AWS SDK: two calls, a signer and
// a presigner are all the platform needs, and the signer is checked against
// the AWS test vectors.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// UnsignedPayload is the payload hash of a presigned request: the body is
// not known when the URL is made.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// Sign signs a request (header auth): the host, every x-amz-* header and
// Content-Type present are signed, and X-Amz-Date is set from now. For the
// s3 service X-Amz-Content-Sha256 carries payloadHash too. Exported so a
// test double of a service can check signatures.
func Sign(req *http.Request, accessKey, secretKey, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", amzDate)
	if service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	canon := map[string]string{}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			canon[lower] = strings.Join(values, ",")
		}
	}
	scope := amzDate[:8] + "/" + region + "/" + service + "/aws4_request"
	signed, signature := sign(req, canon, req.URL.Query(), payloadHash, secretKey, scope, amzDate)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, signed, signature))
}

// Presign signs a request by query string, for a URL a client sends later
// without credentials and without the payload being known: the host and the
// Content-Type header, when the request has one, are signed, so the client
// must send that same Content-Type. It returns the URL with the signature,
// valid for expires from now. Exported so a test double can check it.
func Presign(req *http.Request, accessKey, secretKey, region, service string, expires time.Duration, now time.Time) string {
	amzDate := now.UTC().Format("20060102T150405Z")
	scope := amzDate[:8] + "/" + region + "/" + service + "/aws4_request"
	canon := map[string]string{}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		canon["content-type"] = ct
	}
	q := req.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", accessKey+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(expires/time.Second)))
	q.Set("X-Amz-SignedHeaders", signedNames(req, canon))
	_, signature := sign(req, canon, q, UnsignedPayload, secretKey, scope, amzDate)
	q.Set("X-Amz-Signature", signature)
	u := *req.URL
	u.RawQuery = CanonicalQuery(q)
	return u.String()
}

// Verify recomputes the signature of a presigned request as the service
// would and reports whether it matches, with the time the URL was made and
// how long it was valid. The content-type header must be the one signed.
func Verify(req *http.Request, accessKey, secretKey, region, service string) (bool, time.Time, time.Duration) {
	q := req.URL.Query()
	got := q.Get("X-Amz-Signature")
	made, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	secs, _ := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || got == "" || q.Get("X-Amz-Credential") != accessKey+"/"+made.Format("20060102")+"/"+region+"/"+service+"/aws4_request" {
		return false, made, 0
	}
	canon := map[string]string{}
	if strings.Contains(q.Get("X-Amz-SignedHeaders"), "content-type") {
		canon["content-type"] = req.Header.Get("Content-Type")
	}
	q.Del("X-Amz-Signature")
	_, want := sign(req, canon, q, UnsignedPayload, secretKey, q.Get("X-Amz-Credential")[len(accessKey)+1:], q.Get("X-Amz-Date"))
	return hmac.Equal([]byte(want), []byte(got)), made, time.Duration(secs) * time.Second
}

// signedNames lists the signed headers: host and those in canon, sorted.
func signedNames(req *http.Request, canon map[string]string) string {
	names := []string{"host"}
	for name := range canon {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ";")
}

// sign builds the canonical request from the signed headers (host and
// canon), the query and the payload hash, and returns the signed header
// list and the signature.
func sign(req *http.Request, canon map[string]string, q url.Values, payloadHash, secretKey, scope, amzDate string) (string, string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	all := map[string]string{"host": host}
	for k, v := range canon {
		all[k] = v
	}
	names := strings.Split(signedNames(req, canon), ";")
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + strings.TrimSpace(all[name]) + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, CanonicalQuery(q), headers.String(), signed, payloadHash}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	key := []byte("AWS4" + secretKey)
	for _, part := range strings.Split(scope, "/") {
		key = hmacSHA256(key, part)
	}
	return signed, hex.EncodeToString(hmacSHA256(key, toSign))
}

// Escape URI-encodes a string the way SigV4 canonicalizes it: RFC 3986
// unreserved characters stay, everything else (including '/') is encoded.
func Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// CanonicalQuery is the canonical query string: parameters sorted by name
// and value, each escaped.
func CanonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, Escape(k)+"="+Escape(v))
		}
	}
	return strings.Join(parts, "&")
}

// PayloadHash is the hex SHA-256 of a request body, as SigV4 wants it.
func PayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
