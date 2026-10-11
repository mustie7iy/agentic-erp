package blobstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 stores objects in a bucket of an S3-compatible service, path-style
// (https://endpoint/bucket/key), signed with Signature Version 4. Put sends
// If-None-Match: * so the service refuses an existing key (ErrExists), and
// the object-lock headers when RetainUntil is set; the bucket must have
// Object Lock enabled for those to be honoured.
type S3 struct {
	Endpoint  string // https://s3.eu-west-1.amazonaws.com or http://localhost:9000
	Bucket    string
	Region    string // us-east-1 when the service does not care
	AccessKey string
	SecretKey string
	Client    *http.Client     // nil: http.DefaultClient
	Now       func() time.Time // nil: time.Now
}

func (s S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s S3) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s S3) url(key string) string {
	return strings.TrimRight(s.Endpoint, "/") + "/" + s.Bucket + "/" + encodePath(key)
}

// s3Error is the body of a failed S3 call.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (s S3) do(ctx context.Context, method, key string, body []byte, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.url(key), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("blobstore: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	sum := sha256.Sum256(body)
	SignV4(req, s.AccessKey, s.SecretKey, s.Region, "s3", hex.EncodeToString(sum[:]), s.now())
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("blobstore: %s %s: %w", method, key, err)
	}
	return resp, nil
}

func (s S3) failure(resp *http.Response, method, key string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e s3Error
	_ = xml.Unmarshal(raw, &e) // a body that is not the S3 error document leaves the fields empty
	if e.Code == "" {
		e.Code = resp.Status
	}
	return fmt.Errorf("blobstore: %s %s: %s %s", method, key, e.Code, strings.TrimSpace(e.Message))
}

// Put implements Store.
func (s S3) Put(ctx context.Context, key string, content []byte, opts PutOptions) error {
	if err := checkKey(key); err != nil {
		return err
	}
	headers := map[string]string{"Content-Type": contentType(opts), "If-None-Match": "*"}
	if !opts.RetainUntil.IsZero() {
		headers["x-amz-object-lock-mode"] = "COMPLIANCE"
		headers["x-amz-object-lock-retain-until-date"] = opts.RetainUntil.UTC().Format(time.RFC3339)
	}
	resp, err := s.do(ctx, http.MethodPut, key, content, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPreconditionFailed:
		return ErrExists
	case resp.StatusCode >= 300:
		return s.failure(resp, http.MethodPut, key)
	}
	return nil
}

// Get implements Store.
func (s S3) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := checkKey(key); err != nil {
		return nil, "", err
	}
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", ErrNotFound
	case resp.StatusCode >= 300:
		return nil, "", s.failure(resp, http.MethodGet, key)
	}
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("blobstore: GET %s: %w", key, err)
	}
	// S3 always answers with a Content-Type (the one given at Put, else binary/octet-stream).
	return content, resp.Header.Get("Content-Type"), nil
}

// encodePath URI-encodes each segment of a key the way SigV4 canonicalizes
// it (RFC 3986 unreserved characters stay, '/' separates segments).
func encodePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = encodeSegment(p)
	}
	return strings.Join(parts, "/")
}

func encodeSegment(s string) string {
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

// SignV4 signs a request with AWS Signature Version 4 (header auth): the
// host, every x-amz-* header and Content-Type present are signed, and
// X-Amz-Date and X-Amz-Content-Sha256 are set from now and payloadHash.
// Exported so a test double of the service can check signatures.
func SignV4(req *http.Request, accessKey, secretKey, region, service, payloadHash string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	if service == "s3" {
		req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	}
	var names []string
	canon := map[string]string{"host": req.Host}
	if canon["host"] == "" {
		canon["host"] = req.URL.Host
	}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
			canon[lower] = strings.Join(values, ",")
		}
	}
	for name := range canon {
		names = append(names, name)
	}
	sort.Strings(names)
	var headers strings.Builder
	for _, name := range names {
		headers.WriteString(name + ":" + strings.TrimSpace(canon[name]) + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), headers.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	key := []byte("AWS4" + secretKey)
	for _, part := range []string{date, region, service, "aws4_request"} {
		key = hmacSHA256(key, part)
	}
	signature := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, signed, signature))
}

func canonicalQuery(q url.Values) string {
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
			parts = append(parts, encodeSegment(k)+"="+encodeSegment(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
