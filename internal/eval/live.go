package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
	"github.com/mustie7iy/agentic-erp/internal/llm"
	"github.com/mustie7iy/agentic-erp/internal/platform/errs"
)

// Mint gives a token for a subject of a tenant (its id; empty: an
// operator's).
type Mint func(ctx context.Context, tenant, subject string) (string, error)

// ControlPlane makes throwaway tenants on a cell's control plane for live
// runs (ADR-0039): each is created from the suite's tenant body under a
// name of its own, and offboarded when its repeat ends.
type ControlPlane struct {
	ControlURL string // the admin listener's control plane, e.g. http://localhost:9090
	APIURL     string // the public API, e.g. http://localhost:8080
	Mint       Mint
	HTTP       *http.Client // nil: http.DefaultClient
}

// New creates a tenant from body, named eval-<uuid>.
func (cp *ControlPlane) New(ctx context.Context, body any) (Tenant, error) {
	b, ok := body.(map[string]any)
	if !ok {
		return nil, errors.New("eval: the suite's tenant is not an object")
	}
	admin, _ := b["administrator"].(map[string]any)
	subject, _ := admin["subject"].(string)
	if subject == "" {
		return nil, errors.New("eval: the suite's tenant names no administrator subject")
	}
	b = copyMap(b)
	name := "eval-" + uuid.NewString()
	b["name"] = name
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	if err := cp.control(ctx, http.MethodPost, "/control/v1/tenants", b, "", &created); err != nil {
		// A creation whose answer was lost leaves a tenant only its name
		// finds: offboard it by hand.
		return nil, fmt.Errorf("eval: create the tenant %s: %w", name, err)
	}
	t := &liveTenant{cp: cp, id: created.ID}
	token, err := cp.Mint(ctx, created.ID.String(), subject)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("eval: the administrator's token: %w", err), t.Close(context.WithoutCancel(ctx)))
	}
	t.admin = &apitools.Client{BaseURL: cp.APIURL, Token: token, HTTP: cp.HTTP}
	return t, nil
}

// control calls the control plane as an operator.
func (cp *ControlPlane) control(ctx context.Context, method, path string, body any, ifMatch string, into any) error {
	token, err := cp.Mint(ctx, "", "eval-runner")
	if err != nil {
		return fmt.Errorf("eval: the operator's token: %w", err)
	}
	raw, _ := json.Marshal(body) // a decoded YAML or a map of strings always encodes
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cp.ControlURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("eval: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", uuid.NewString())
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := client(cp.HTTP).Do(req)
	if err != nil {
		return fmt.Errorf("eval: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("eval: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, answer)
	}
	if into == nil {
		return nil
	}
	return errs.Wrapf(json.Unmarshal(answer, into), "eval: %s %s: decode the answer", method, path)
}

type liveTenant struct {
	cp    *ControlPlane
	id    uuid.UUID
	admin *apitools.Client
}

func (t *liveTenant) Admin() *apitools.Client { return t.admin }

func (t *liveTenant) As(ctx context.Context, subject string) (*apitools.Client, error) {
	token, err := t.cp.Mint(ctx, t.id.String(), subject)
	if err != nil {
		return nil, fmt.Errorf("eval: the token of %s: %w", subject, err)
	}
	return &apitools.Client{BaseURL: t.cp.APIURL, Token: token, HTTP: t.cp.HTTP}, nil
}

// Close offboards the tenant: its export and erasure follow as for any
// tenant (ADR-0029).
func (t *liveTenant) Close(ctx context.Context) error {
	return t.cp.control(ctx, http.MethodPost, "/control/v1/tenants/"+t.id.String()+":offboard", nil, "*", nil)
}

// DevIssuer mints tokens from a server running with ERP_DEV_ISSUER (its
// admin listener's /dev/token): the local cell a live run uses, and erp
// token.
func DevIssuer(adminURL string, h *http.Client) Mint {
	return func(ctx context.Context, tenant, subject string) (string, error) {
		raw, _ := json.Marshal(map[string]string{"tenant_id": tenant, "subject": subject}) // strings always encode
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(adminURL, "/")+"/dev/token", bytes.NewReader(raw))
		if err != nil {
			return "", fmt.Errorf("admin listener %q: %w", adminURL, err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client(h).Do(req)
		if err != nil {
			return "", fmt.Errorf("ask the admin listener: %w", err)
		}
		defer func() { _ = resp.Body.Close() }() // only read from: closing frees the connection and cannot change the answer
		var answer struct{ Token string }
		if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
			return "", fmt.Errorf("the admin listener answered %d with no readable token (is the server running with ERP_DEV_ISSUER=true?): %w", resp.StatusCode, err)
		}
		if resp.StatusCode != http.StatusOK || answer.Token == "" {
			return "", fmt.Errorf("the admin listener answered %d: is the server running with ERP_DEV_ISSUER=true?", resp.StatusCode)
		}
		return answer.Token, nil
	}
}

// Gateway gives each repeat's agent the live model through the cell's
// gateway, with its own token, on the suite's alias; what it cost is read
// from the tenant's ledger.
type Gateway struct {
	Alias string // the suite's
}

// For is the agent's runner on the gateway.
func (g Gateway) For(_ context.Context, agent *apitools.Client, _ Case, _ map[string]string) (*llm.Runner, error) {
	r := llm.Gateway(agent.BaseURL, agent.Token, agent.HTTP)
	r.Model = g.Alias
	return r, nil
}

func client(h *http.Client) *http.Client {
	if h == nil {
		return http.DefaultClient
	}
	return h
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
