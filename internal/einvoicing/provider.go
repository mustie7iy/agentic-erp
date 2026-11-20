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
// 60 s per call; the connection's 5 s are the egress client's. An answer
// may be up to MaxAnswer: a document of MaxDocument in Base64 (Evatra's
// form) with room for the JSON around it; each adapter holds the document
// itself to MaxDocument.
const (
	MaxDocument = 10 << 20
	MaxAnswer   = (MaxDocument+2)/3*4 + 4<<10
	CallTimeout = 60 * time.Second
)

// Message is a document the provider holds and has not had acknowledged:
// its id at the provider and the participant identifier it is addressed
// to, as the provider lists it.
type Message struct {
	ID       string `json:"id"`
	Receiver string `json:"receiver,omitempty"`
}

// Outgoing is a rendered e-invoice as a provider is given it (ADR-0061
// §9, As built E3): its number, which the provider files it under, the
// seller's participant identifier, the business type (B2B or B2G), the
// XML as kept, whether a call before this one may have reached the
// provider (LookUp: the provider has no idempotency key, so the adapter
// looks the document up first and takes a submission it finds), and the
// tracking ids of earlier submissions the provider failed to send, which
// are not taken as found.
type Outgoing struct {
	Filename     string
	Participant  string
	BusinessType string
	Content      []byte
	LookUp       bool
	Failed       []string
}

// Submission is what the provider answered a send: its tracking id and
// processing status, and whether it was found already there (LookUp)
// rather than sent.
type Submission struct {
	TrackingID, Status string
	Found              bool
}

// Report is the provider's processing status of a submission.
type Report struct {
	Status string
}

// The providers' processing statuses the send workflow acts on (Evatra's,
// https://evatra.com/api as the plan read it on 2026-11-19); any other is
// read as still in progress.
const (
	ProviderPending         = "PENDING"
	ProviderCompleted       = "COMPLETED"
	ProviderFailed          = "FAILED"
	ProviderSendingFailed   = "SENDING_FAILED"
	ProviderPermanentFailed = "PERMANENT_FAILED"
)

// Provider is an accredited e-invoicing provider as one tenant's access
// point (ADR-0061 §3). Implementations: Evatra's adapter (Evatra, the first
// provider, po:2026-11-19) and the stand-in (tests, walkthrough); a second
// provider is one more adapter.
type Provider interface {
	// Pending lists the documents received and not yet acknowledged.
	Pending(ctx context.Context) ([]Message, error)
	// Fetch reads one document's bytes, at most MaxDocument; a larger one
	// is refused with einvoicing.document_too_large.
	Fetch(ctx context.Context, id string) ([]byte, error)
	// Acknowledge tells the provider the document is kept; acknowledging
	// twice is not an error.
	Acknowledge(ctx context.Context, id string) error
	// Send gives the provider one rendered document, as it is kept, and
	// answers its tracking id; a refusal is a problem below 500, which
	// another call would meet again (As built E3).
	Send(ctx context.Context, d Outgoing) (Submission, error)
	// Status reads the provider's processing status of a submission.
	Status(ctx context.Context, trackingID string) (Report, error)
}

// NoSending is a provider whose adapter sends nothing (the stand-in's own
// protocol): a send through its connector is refused before anything is
// queued.
type NoSending interface {
	SendsNothing()
}

// NewClient is the egress client providers are called through: public
// addresses only, no redirects, the system roots (ADR-0031), with the
// provider's bounds; opts add the walkthrough's stand-in.
func NewClient(opts ...egress.Option) (*egress.Client, error) {
	return egress.New(append([]egress.Option{egress.ResponseLimit(MaxAnswer), egress.CallTimeout(CallTimeout)}, opts...)...)
}

// LocalAcknowledgement is a provider that keeps no acknowledgement
// (Evatra): its Acknowledge does nothing and its Pending lists documents
// already kept, so a fetch leaves out those recorded and acknowledged
// (ADR-0061 As built E1).
type LocalAcknowledgement interface {
	AcknowledgesLocally()
}

// Adapters gives the provider of a connector's adapter, called through
// client: Evatra's, at the host of the connector's environment among
// evatra (nil: EvatraHosts()), its tokens kept for every connector of the
// server, or the stand-in's.
func Adapters(client *egress.Client, evatra map[string]string, now func() time.Time) func(Connector) (Provider, error) {
	if evatra == nil {
		evatra = EvatraHosts()
	}
	if now == nil {
		now = time.Now
	}
	sessions := &EvatraSessions{}
	return func(c Connector) (Provider, error) {
		if client == nil {
			return nil, problem.Unavailable("This server has no client for e-invoicing providers.", 0)
		}
		switch c.Adapter {
		case "evatra":
			return newEvatra(client, evatra, sessions, now, c)
		case "standin":
			return &StandIn{Client: client, BaseURL: strings.TrimSuffix(c.BaseURL, "/"), Token: c.Secret}, nil
		}
		return nil, problem.Unprocessable("integration.invalid_settings", "Connector "+c.Name+" names the adapter "+c.Adapter+", which this server does not have.")
	}
}

// StandIn is the provider's stand-in (ADR-0061 §3): an HTTP server for
// tests and the walkthrough (einvoicetest), called through the egress
// client as a real provider would be, with its credentials as a bearer
// token. Its protocol is its own, no provider's (einvoicetest emulates
// Evatra's API besides, for the Evatra adapter):
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
	case resp.Truncated || len(resp.Body) > MaxDocument:
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

// SendsNothing implements NoSending: the stand-in's protocol has no
// sending; the Evatra adapter, against Evatra's emulation, sends.
func (*StandIn) SendsNothing() {}

// cannotSend refuses a send through a provider that sends nothing.
func cannotSend() error {
	return problem.Unprocessable("einvoicing.adapter_cannot_send", "The stand-in's own protocol sends no e-invoices; send through an evatra connector.")
}

// Send implements Provider: refused, the stand-in's protocol sends nothing.
func (*StandIn) Send(context.Context, Outgoing) (Submission, error) {
	return Submission{}, cannotSend()
}

// Status implements Provider: refused, as Send is.
func (*StandIn) Status(context.Context, string) (Report, error) { return Report{}, cannotSend() }
