package einvoicing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mustie7iy/agentic-erp/internal/platform/egress"
	"github.com/mustie7iy/agentic-erp/internal/platform/problem"
)

// The provider client's bounds (ADR-0061 §3): a document of up to 10 MiB,
// 60 s per call; the connection's 5 s are the egress client's.
const (
	MaxDocument = 10 << 20
	CallTimeout = 60 * time.Second
)

// Message is a document the provider holds and has not had acknowledged:
// its id at the provider and the participant identifier it is addressed
// to, as the provider lists it.
type Message struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver,omitempty"`
}

// Provider is an accredited e-invoicing provider as one tenant's access
// point (ADR-0061 §3). Implementations: the stand-in (tests, walkthrough)
// and, once chosen, one adapter per provider (Q1).
type Provider interface {
	// Pending lists the documents received and not yet acknowledged.
	Pending(ctx context.Context) ([]Message, error)
	// Fetch reads one document's bytes, at most MaxDocument; a larger one
	// is refused with einvoicing.document_too_large.
	Fetch(ctx context.Context, id string) ([]byte, error)
	// Acknowledge tells the provider the document is kept; acknowledging
	// twice is not an error.
	Acknowledge(ctx context.Context, id string) error
}

// NewClient is the egress client providers are called through: public
// addresses only, no redirects, the system roots (ADR-0031), with the
// provider's bounds; opts add the walkthrough's stand-in.
func NewClient(opts ...egress.Option) (*egress.Client, error) {
	return egress.New(append([]egress.Option{egress.ResponseLimit(MaxDocument), egress.CallTimeout(CallTimeout)}, opts...)...)
}

// Adapters gives the provider of a connector's adapter, called through
// client: the stand-in, the only one until a provider is chosen.
func Adapters(client *egress.Client) func(Connector) (Provider, error) {
	return func(c Connector) (Provider, error) {
		if client == nil {
			return nil, problem.Unavailable("This server has no client for e-invoicing providers.", 0)
		}
		if c.Adapter != "standin" {
			return nil, problem.Unprocessable("integration.invalid_settings", "Connector "+c.Name+" names the adapter "+c.Adapter+", which this server does not have.")
		}
		return &StandIn{Client: client, BaseURL: strings.TrimSuffix(c.BaseURL, "/"), Token: c.Secret}, nil
	}
}

// StandIn is the provider's stand-in (ADR-0061 §3): an HTTP server for
// tests and the walkthrough (einvoicetest), called through the egress
// client as a real provider would be, with its credentials as a bearer
// token. Its protocol is its own, no provider's:
//
//	GET  {base}/messages                   {"messages": [{"id": ..., "receiver": ...}]}
//	GET  {base}/messages/{id}              the document's bytes
//	POST {base}/messages/{id}/acknowledge  204; again, 204
//
// Production refuses it (integration.adapter_not_certified).
type StandIn struct {
	Client  *egress.Client
	BaseURL string // without a trailing slash
	Token   string
}

func (s *StandIn) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + s.Token}}
}

func (s *StandIn) message(id string) string { return s.BaseURL + "/messages/" + url.PathEscape(id) }

// Pending implements Provider.
func (s *StandIn) Pending(ctx context.Context) ([]Message, error) {
	resp, err := s.Client.Get(ctx, s.BaseURL+"/messages", s.header())
	if err != nil {
		return nil, fmt.Errorf("einvoicing: list the provider's messages: %w", err)
	}
	if resp.Status != http.StatusOK || resp.Truncated {
		return nil, fmt.Errorf("einvoicing: list the provider's messages: answered %d (truncated %v)", resp.Status, resp.Truncated)
	}
	var list struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return nil, fmt.Errorf("einvoicing: read the provider's messages: %w", err)
	}
	return list.Messages, nil
}

// Fetch implements Provider.
func (s *StandIn) Fetch(ctx context.Context, id string) ([]byte, error) {
	resp, err := s.Client.Get(ctx, s.message(id), s.header())
	switch {
	case err != nil:
		return nil, fmt.Errorf("einvoicing: fetch message %s: %w", id, err)
	case resp.Status != http.StatusOK:
		return nil, fmt.Errorf("einvoicing: fetch message %s: answered %d", id, resp.Status)
	case resp.Truncated:
		return nil, tooLarge(id)
	}
	return resp.Body, nil
}

// tooLarge refuses a document above MaxDocument: it is not recorded (T65c
// records it refused, ADR-0061 §5), and the fetch names it.
func tooLarge(id string) error {
	return problem.Unprocessable("einvoicing.document_too_large", fmt.Sprintf("Message %s is larger than %d bytes.", id, MaxDocument))
}

// Acknowledge implements Provider.
func (s *StandIn) Acknowledge(ctx context.Context, id string) error {
	resp, err := s.Client.Post(ctx, s.message(id)+"/acknowledge", s.header(), nil)
	if err != nil {
		return fmt.Errorf("einvoicing: acknowledge message %s: %w", id, err)
	}
	if resp.Status != http.StatusNoContent && resp.Status != http.StatusOK {
		return fmt.Errorf("einvoicing: acknowledge message %s: answered %d", id, resp.Status)
	}
	return nil
}
