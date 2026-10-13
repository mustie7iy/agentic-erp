package blobstore

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
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
	// Credentials, when set, sign instead of AccessKey and SecretKey: the
	// ambient credentials of the deployment (awscreds.Default).
	Credentials awscreds.Provider
	Client      *http.Client     // nil: http.DefaultClient
	Now         func() time.Time // nil: time.Now
}

// creds are the credentials to sign with.
func (s S3) creds(ctx context.Context) (sigv4.Credentials, error) {
	if s.Credentials == nil {
		return sigv4.Credentials{AccessKey: s.AccessKey, SecretKey: s.SecretKey}, nil
	}
	c, err := s.Credentials.Credentials(ctx)
	if err != nil {
		return sigv4.Credentials{}, fmt.Errorf("blobstore: credentials: %w", err)
	}
	return sigv4.Credentials{AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken}, nil
}

func (s S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s S3) now() time.Time {
	return clock.Now(s.Now)
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
	creds, err := s.creds(ctx)
	if err != nil {
		return nil, err
	}
	sigv4.SignWith(req, creds, s.Region, "s3", sigv4.PayloadHash(body), s.now())
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

// listResult is the body of ListObjectsV2.
type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List implements Eraser with ListObjectsV2, following continuation tokens.
func (s S3) List(ctx context.Context, prefix string) ([]string, error) {
	if err := checkKey(strings.TrimSuffix(prefix, "/")); err != nil {
		return nil, err
	}
	var keys []string
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.Endpoint, "/")+"/"+s.Bucket+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("blobstore: %w", err)
		}
		creds, err := s.creds(ctx)
		if err != nil {
			return nil, err
		}
		sigv4.SignWith(req, creds, s.Region, "s3", sigv4.PayloadHash(nil), s.now())
		resp, err := s.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("blobstore: LIST %s: %w", prefix, err)
		}
		var page listResult
		if resp.StatusCode >= 300 {
			err = s.failure(resp, "LIST", prefix)
		} else if derr := xml.NewDecoder(resp.Body).Decode(&page); derr != nil {
			err = fmt.Errorf("blobstore: LIST %s: %w", prefix, derr)
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		if !page.IsTruncated || page.NextContinuationToken == "" {
			return keys, nil
		}
		token = page.NextContinuationToken
	}
}

// Delete implements Eraser: an object under retention (Object Lock) is
// refused by the service, which is ErrLocked here.
func (s S3) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrLocked, s.failure(resp, http.MethodDelete, key))
	case resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound:
		return s.failure(resp, http.MethodDelete, key)
	}
	return nil
}

// Stat implements Store with a HEAD request.
func (s S3) Stat(ctx context.Context, key string) (int64, string, error) {
	if err := checkKey(key); err != nil {
		return 0, "", err
	}
	resp, err := s.do(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return 0, "", ErrNotFound
	case resp.StatusCode >= 300:
		return 0, "", s.failure(resp, http.MethodHead, key)
	}
	return resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

// PresignPut implements Presigner: a query-signed PUT of the key, bound to
// the content type, valid for expires.
func (s S3) PresignPut(ctx context.Context, key, contentType string, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPut, s.url(key), nil)
	if err != nil {
		return "", fmt.Errorf("blobstore: %w", err)
	}
	creds, err := s.creds(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	return sigv4.PresignWith(req, creds, s.Region, "s3", expires, s.now()), nil
}

// encodePath URI-encodes each segment of a key the way SigV4 canonicalizes
// it (RFC 3986 unreserved characters stay, '/' separates segments).
func encodePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = sigv4.Escape(p)
	}
	return strings.Join(parts, "/")
}
