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

const keyARN = "arn:aws:kms:eu-west-1:111122223333:key/erp"

// TestS3Encryption: with Encryption every Put asks the service for that
// algorithm and key, so objects land under the bucket's own key rather
// than S3's AWS managed one; without, it asks nothing (ADR-0033).
func TestS3Encryption(t *testing.T) {
	srv := blobstoretest.New(t)
	ctx := context.Background()
	plain, sealed, managed := srv.Store(), srv.Store(), srv.Store()
	sealed.Encryption = blobstore.Encryption{Algorithm: blobstore.SSEKMS, KeyID: keyARN}
	managed.Encryption = blobstore.Encryption{Algorithm: blobstore.SSEKMS}
	for _, c := range []struct {
		name       string
		store      blobstore.S3
		key        string
		want, wkey string
	}{
		{"with the bucket's key", sealed, "a/sealed", "aws:kms", keyARN},
		{"with the AWS managed key", managed, "a/managed", "aws:kms", ""},
		{"without", plain, "a/plain", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.store.Put(ctx, c.key, []byte("x"), blobstore.PutOptions{}); err != nil {
				t.Fatal(err)
			}
			o, ok := srv.Object(c.key)
			if !ok {
				t.Fatalf("object %s: got none, want stored", c.key)
			}
			if o.Encryption != c.want {
				t.Errorf("x-amz-server-side-encryption: got %q, want %q", o.Encryption, c.want)
			}
			if o.KeyID != c.wkey {
				t.Errorf("x-amz-server-side-encryption-aws-kms-key-id: got %q, want %q", o.KeyID, c.wkey)
			}
		})
	}
}

// TestS3BucketEncryption: the bucket's default encryption must be KMS, so
// objects written past this store (presigned uploads) are encrypted too;
// it is returned, algorithm and key, for the store's own writes to ask the
// same. None, another algorithm, or a failed read is refused.
func TestS3BucketEncryption(t *testing.T) {
	for _, c := range []struct {
		name, bucket, key string
		fail              int
		want              blobstore.Encryption
		wantErr           string
	}{
		{"KMS with a key", "aws:kms", keyARN, 0, blobstore.Encryption{Algorithm: "aws:kms", KeyID: keyARN}, ""},
		{"KMS, AWS managed key", "aws:kms", "", 0, blobstore.Encryption{Algorithm: "aws:kms"}, ""},
		{"KMS, dual-layer", "aws:kms:dsse", keyARN, 0, blobstore.Encryption{Algorithm: "aws:kms:dsse", KeyID: keyARN}, ""},
		{"S3-managed keys", "AES256", "", 0, blobstore.Encryption{}, `bucket's default encryption is "AES256", want aws:kms`},
		{"none", "", "", 0, blobstore.Encryption{}, "ServerSideEncryptionConfigurationNotFoundError"},
		{"service failing", "aws:kms", "", http.StatusInternalServerError, blobstore.Encryption{}, "InternalError"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := blobstoretest.New(t)
			srv.BucketEncryption, srv.BucketKeyID, srv.Fail = c.bucket, c.key, c.fail
			got, err := srv.Store().BucketEncryption(context.Background())
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("got %v, want an error containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestS3BucketEncryptionFailures: a service that cannot be reached, or
// that answers with something other than the configuration, fails.
func TestS3BucketEncryptionFailures(t *testing.T) {
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
			if _, err := s.BucketEncryption(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}
