package fieldcrypt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/platform/awscreds"
	"github.com/mustie7iy/agentic-erp/internal/platform/clock"
	"github.com/mustie7iy/agentic-erp/internal/platform/sigv4"
)

// KMS is a master key held by AWS KMS, or a service speaking its protocol
// (T15, ADR-0019): tenant keys are wrapped with Encrypt and unwrapped with
// Decrypt under the KMS key, with the tenant id as encryption context, so a
// wrapped key only unwraps for its tenant. The master never leaves the
// service; the deployment holds credentials that may call it. Tenant keys
// are unwrapped once per replica (Keyring caches them), so KMS is on the
// path of a tenant's first request and of rotations, not of every seal.
type KMS struct {
	Name      string // the master's id in the ring and in tenancy.tenant_key (default kms)
	KeyID     string // the KMS key: id, ARN or alias/name
	Endpoint  string // https://kms.eu-west-1.amazonaws.com, or a compatible service
	Region    string
	AccessKey string
	SecretKey string
	// Credentials, when set, sign instead of AccessKey and SecretKey: the
	// ambient credentials of the deployment (awscreds.Default).
	Credentials awscreds.Provider
	Client      *http.Client     // nil: a client with a 10 s timeout
	Now         func() time.Time // nil: time.Now
}

func (k KMS) creds(ctx context.Context) (sigv4.Credentials, error) {
	if k.Credentials == nil {
		return sigv4.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey}, nil
	}
	c, err := k.Credentials.Credentials(ctx)
	if err != nil {
		return sigv4.Credentials{}, fmt.Errorf("fieldcrypt: kms credentials: %w", err)
	}
	return sigv4.Credentials{AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken}, nil
}

// ID implements Master.
func (k KMS) ID() string {
	if k.Name == "" {
		return "kms"
	}
	return k.Name
}

// Wrap implements Master with KMS Encrypt.
func (k KMS) Wrap(tenant uuid.UUID, key []byte) ([]byte, error) {
	var out struct {
		CiphertextBlob []byte
	}
	if err := k.call("Encrypt", map[string]any{"KeyId": k.KeyID, "Plaintext": key, "EncryptionContext": k.context(tenant)}, &out); err != nil {
		return nil, err
	}
	return out.CiphertextBlob, nil
}

// Unwrap implements Master with KMS Decrypt; a blob KMS refuses (not this
// key's, not this tenant's, tampered) is ErrCorrupt.
func (k KMS) Unwrap(tenant uuid.UUID, wrapped []byte) ([]byte, error) {
	var out struct {
		Plaintext []byte
	}
	err := k.call("Decrypt", map[string]any{"KeyId": k.KeyID, "CiphertextBlob": wrapped, "EncryptionContext": k.context(tenant)}, &out)
	var kmsErr *kmsError
	if errors.As(err, &kmsErr) && kmsErr.Type == "InvalidCiphertextException" {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if err != nil {
		return nil, err
	}
	return out.Plaintext, nil
}

func (k KMS) context(tenant uuid.UUID) map[string]string {
	return map[string]string{"tenant": tenant.String()}
}

func (k KMS) client() *http.Client {
	if k.Client != nil {
		return k.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (k KMS) now() time.Time {
	return clock.Now(k.Now)
}

// kmsError is a failed KMS call: the service's exception type and message.
type kmsError struct {
	Action  string
	Status  int
	Type    string
	Message string
}

func (e *kmsError) Error() string {
	return fmt.Sprintf("fieldcrypt: kms %s: %d %s: %s", e.Action, e.Status, e.Type, e.Message)
}

// call makes one request of the AWS JSON 1.1 protocol KMS speaks
// (X-Amz-Target: TrentService.<Action>) and decodes the answer into out.
func (k KMS) call(action string, in map[string]any, out any) error {
	body, _ := json.Marshal(in) // strings, bytes and a map of strings always encode
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimRight(k.Endpoint, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("fieldcrypt: kms: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService."+action)
	creds, err := k.creds(req.Context())
	if err != nil {
		return err
	}
	sigv4.SignWith(req, creds, k.Region, "kms", sigv4.PayloadHash(body), k.now())
	resp, err := k.client().Do(req)
	if err != nil {
		return fmt.Errorf("fieldcrypt: kms %s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()               // only read from: closing frees the connection and cannot change the answer
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // a body cut short fails to decode below, or shortens a refusal's message
	if resp.StatusCode != http.StatusOK {
		e := &kmsError{Action: action, Status: resp.StatusCode}
		var body struct {
			Type    string `json:"__type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &body) == nil && body.Type != "" {
			e.Type, e.Message = strings.TrimPrefix(body.Type, "com.amazonaws.kms#"), body.Message
		} else {
			e.Type, e.Message = "unexpected answer", strings.TrimSpace(string(raw))
		}
		return e
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("fieldcrypt: kms %s: answer is not JSON: %w", action, err)
	}
	return nil
}
