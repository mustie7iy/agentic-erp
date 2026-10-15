package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Vault resolves vault: references against a HashiCorp Vault KV version 2
// engine: `vault:<mount>/<path>#<field>` reads field from the secret at
// path under mount. It authenticates with Token when set, else by the
// Kubernetes method with the replica's service account token (K8sRole,
// K8sJWT); a login token that stops working is renewed once per request.
type Vault struct {
	Address   string       // https://vault.example.com:8200
	Token     string       // a token, when not logging in
	Namespace string       // Vault Enterprise namespace, optional
	K8sRole   string       // role of the kubernetes auth method
	K8sJWT    string       // the service account token (not its path)
	Client    *http.Client // nil: http.DefaultClient

	mu    sync.Mutex
	login string // token obtained by logging in
}

func (v *Vault) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return http.DefaultClient
}

// vaultError is the body of a failed Vault call.
type vaultError struct {
	Errors []string `json:"errors"`
}

func (v *Vault) call(ctx context.Context, method, path, token string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		b, _ := json.Marshal(body) // strings only
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(v.Address, "/")+"/v1/"+path, payload)
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: vault: %w", err)
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.Namespace)
	}
	resp, err := v.client().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: vault: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: vault: %w", err)
	}
	return resp.StatusCode, raw, nil
}

func failure(status int, raw []byte) error {
	var e vaultError
	_ = json.Unmarshal(raw, &e) // a body that is not Vault's error document leaves no messages
	msg := strings.Join(e.Errors, "; ")
	if msg == "" {
		msg = http.StatusText(status)
	}
	return fmt.Errorf("secrets: vault answered %d: %s", status, msg)
}

// token returns the token to use, logging in when needed.
func (v *Vault) token(ctx context.Context, renew bool) (string, error) {
	if v.Token != "" {
		return v.Token, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.login != "" && !renew {
		return v.login, nil
	}
	if v.K8sRole == "" || v.K8sJWT == "" {
		return "", fmt.Errorf("secrets: vault: no token and no kubernetes role to log in with")
	}
	status, raw, err := v.call(ctx, http.MethodPost, "auth/kubernetes/login", "", map[string]string{"role": v.K8sRole, "jwt": v.K8sJWT})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("secrets: vault login: %w", failure(status, raw))
	}
	var answer struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &answer) != nil || answer.Auth.ClientToken == "" {
		return "", fmt.Errorf("secrets: vault login: no client token in the answer")
	}
	v.login = answer.Auth.ClientToken
	return v.login, nil
}

// Resolve implements Resolver.
func (v *Vault) Resolve(ctx context.Context, ref string) (string, error) {
	scheme, rest, _ := strings.Cut(ref, ":")
	location, field, ok := strings.Cut(rest, "#")
	mount, path, hasPath := strings.Cut(strings.Trim(location, "/"), "/")
	if scheme != SchemeVault || !ok || field == "" || !hasPath || path == "" {
		return "", fmt.Errorf("secrets: %s: a vault reference is vault:<mount>/<path>#<field>", ref)
	}
	for attempt := 0; ; attempt++ {
		token, err := v.token(ctx, attempt > 0)
		if err != nil {
			return "", err
		}
		status, raw, err := v.call(ctx, http.MethodGet, mount+"/data/"+path, token, nil)
		if err != nil {
			return "", err
		}
		switch {
		case status == http.StatusForbidden && v.Token == "" && attempt == 0:
			continue // the login token expired: log in again, once
		case status == http.StatusNotFound:
			return "", fmt.Errorf("secrets: %s: %w", ref, ErrNotFound)
		case status != http.StatusOK:
			return "", fmt.Errorf("secrets: %s: %w", ref, failure(status, raw))
		}
		var answer struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &answer) != nil {
			return "", fmt.Errorf("secrets: %s: vault answered with a body that is not a KV v2 secret", ref)
		}
		value, found := answer.Data.Data[field].(string)
		if !found || value == "" {
			return "", fmt.Errorf("secrets: %s: field %s: %w", ref, field, ErrNotFound)
		}
		return value, nil
	}
}
