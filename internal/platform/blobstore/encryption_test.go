package blobstore_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore"
	"github.com/mustie7iy/agentic-erp/internal/platform/blobstore/blobstoretest"
)

// TestS3Encryption: with Encryption every Put asks the service to encrypt
// the object under KMS; without, it asks nothing (ADR-0033).
func TestS3Encryption(t *testing.T) {
	srv := blobstoretest.New(t)
	ctx := context.Background()
	plain, sealed := srv.Store(), srv.Store()
	sealed.Encryption = blobstore.SSEKMS
	for _, c := range []struct {
		name  string
		store blobstore.S3
		key   string
		want  string
	}{
		{"with encryption", sealed, "a/sealed", "aws:kms"},
		{"without", plain, "a/plain", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.store.Put(ctx, c.key, []byte("x"), blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			o, _ := srv.Object(c.key)
			if o.Encryption != c.want {
				t.Errorf("x-amz-server-side-encryption: got %q, want %q", o.Encryption, c.want)
			}
		})
	}
}

// TestS3CheckEncryption: the bucket's default encryption must be KMS, so
// objects written past this store (presigned uploads) are encrypted too;
// none, another algorithm, or a failed read is refused.
func TestS3CheckEncryption(t *testing.T) {
	for _, c := range []struct {
		name, bucket string
		fail         int
		want         string // the error; empty: accepted
	}{
		{"KMS", "aws:kms", 0, ""},
		{"KMS, dual-layer", "aws:kms:dsse", 0, ""},
		{"S3-managed keys", "AES256", 0, `bucket's default encryption is "AES256", want aws:kms`},
		{"none", "", 0, "ServerSideEncryptionConfigurationNotFoundError"},
		{"service failing", "aws:kms", http.StatusInternalServerError, "InternalError"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := blobstoretest.New(t)
			srv.BucketEncryption, srv.Fail = c.bucket, c.fail
			err := srv.Store().CheckEncryption(context.Background())
			switch {
			case c.want == "" && err != nil:
				t.Errorf("got %v, want accepted", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// TestS3CheckEncryptionFailures: a service that cannot be reached, or that
// answers with something other than the configuration, fails the check.
func TestS3CheckEncryptionFailures(t *testing.T) {
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("not xml")) }))
	defer junk.Close()
	for _, c := range []struct {
		name, endpoint, want string
	}{
		{"unreachable", "http://127.0.0.1:1", "GET encryption"},
		{"not the configuration", junk.URL, "GET encryption"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := blobstore.S3{Endpoint: c.endpoint, Bucket: "b", Region: "us-east-1", AccessKey: "a", SecretKey: "s"}
			if err := s.CheckEncryption(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}
