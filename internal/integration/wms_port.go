package integration

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

// The WMS port's bounds: a message of up to 1 MiB each way, 30 s a call;
// the connection's 5 s are the egress client's.
const (
	MaxWMSMessage  = 1 << 20
	WMSCallTimeout = 30 * time.Second
)

// Warehouse is an external warehouse management system as one tenant's
// connector reaches it (ADR-0082 As built S2q). Implementations: the
// stand-in (tests, walkthrough); a vendor's WMS is one more adapter.
type Warehouse interface {
	// Send gives the WMS one message, its body as kept; the WMS takes a
	// message id it holds already as delivered, so a send whose answer was
	// lost is sent again safely. A refusal is a problem below 500, which
	// another call would meet again.
	Send(ctx context.Context, id string, body []byte) error
	// Confirmations lists the ids of the movements the WMS reports and has
	// not had acknowledged.
	Confirmations(ctx context.Context) ([]string, error)
	// Confirmation reads one, at most MaxWMSMessage bytes.
	Confirmation(ctx context.Context, id string) ([]byte, error)
	// Acknowledge tells the WMS the movement is recorded; twice is not an
	// error.
	Acknowledge(ctx context.Context, id string) error
}

// NewWMSClient is the egress client warehouse systems are called through:
// public addresses only, no redirects, the system roots (ADR-0031), with
// the port's bounds; opts add the walkthrough's stand-in.
func NewWMSClient(opts ...egress.Option) (*egress.Client, error) {
	return egress.New(append([]egress.Option{egress.ResponseLimit(MaxWMSMessage), egress.CallTimeout(WMSCallTimeout)}, opts...)...)
}

// WMSConnection is what an adapter needs of a wms connector: its name,
// settings and resolved credentials.
type WMSConnection struct {
	Name   string
	Secret string
	WMSSettings
}

// WMSAdapters gives the warehouse of a connector's adapter, called
// through client: the stand-in's, the only adapter.
func WMSAdapters(client *egress.Client) func(WMSConnection) (Warehouse, error) {
	return func(c WMSConnection) (Warehouse, error) {
		if client == nil {
			return nil, problem.Unavailable("This server has no client for warehouse systems.", 0)
		}
		if c.Adapter != StandInAdapter {
			return nil, problem.Unprocessable("integration.invalid_settings", "Connector "+c.Name+" names the adapter "+c.Adapter+", which this server does not have.")
		}
		return &WMSStandIn{Client: client, BaseURL: strings.TrimSuffix(c.BaseURL, "/"), Token: c.Secret}, nil
	}
}

// WMSStandIn is the warehouse system's stand-in (wmstest): an HTTPS
// server for tests and the walkthrough, called through the egress client
// as a vendor's WMS would be, with the connector's credentials as a
// bearer token. Its protocol is its own, no vendor's:
//
//	PUT  {base}/messages/{id}                    the message's JSON; 204, again 204
//	GET  {base}/confirmations                    {"confirmations": [{"id": ...}]}
//	GET  {base}/confirmations/{id}               the confirmation's JSON
//	POST {base}/confirmations/{id}/acknowledge   204; again, 204
//
// Production refuses it (integration.adapter_not_certified).
type WMSStandIn struct {
	Client  *egress.Client
	BaseURL string // without a trailing slash
	Token   string
}

func (s *WMSStandIn) header() http.Header {
	return http.Header{"Authorization": {"Bearer " + s.Token}, "Content-Type": {"application/json"}}
}

func (s *WMSStandIn) confirmation(id string) string {
	return s.BaseURL + "/confirmations/" + url.PathEscape(id)
}

// Send implements Warehouse. A 4xx answer but 408 and 429 is the WMS
// refusing the message; anything else fails the call, to be tried again.
func (s *WMSStandIn) Send(ctx context.Context, id string, body []byte) error {
	resp, err := s.Client.Do(ctx, http.MethodPut, s.BaseURL+"/messages/"+url.PathEscape(id), s.header(), body)
	switch {
	case err != nil:
		return fmt.Errorf("integration: send message %s to the warehouse system: %w", id, err)
	case resp.Status >= 200 && resp.Status < 300:
		return nil
	case resp.Status >= 400 && resp.Status < 500 && resp.Status != http.StatusRequestTimeout && resp.Status != http.StatusTooManyRequests:
		return problem.Unprocessable("integration.wms_refused", fmt.Sprintf("The warehouse system refused message %s (HTTP %d).", id, resp.Status))
	}
	return fmt.Errorf("integration: send message %s to the warehouse system: answered %d", id, resp.Status)
}

// Confirmations implements Warehouse.
func (s *WMSStandIn) Confirmations(ctx context.Context) ([]string, error) {
	resp, err := s.Client.Get(ctx, s.BaseURL+"/confirmations", s.header())
	if err != nil {
		return nil, fmt.Errorf("integration: list the warehouse system's confirmations: %w", err)
	}
	if resp.Status != http.StatusOK || resp.Truncated {
		return nil, fmt.Errorf("integration: list the warehouse system's confirmations: answered %d (truncated %v)", resp.Status, resp.Truncated)
	}
	var list struct {
		Confirmations []struct {
			ID string `json:"id"`
		} `json:"confirmations"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		return nil, fmt.Errorf("integration: read the warehouse system's confirmations: %w", err)
	}
	ids := make([]string, len(list.Confirmations))
	for i, c := range list.Confirmations {
		ids[i] = c.ID
	}
	return ids, nil
}

// Confirmation implements Warehouse.
func (s *WMSStandIn) Confirmation(ctx context.Context, id string) ([]byte, error) {
	resp, err := s.Client.Get(ctx, s.confirmation(id), s.header())
	switch {
	case err != nil:
		return nil, fmt.Errorf("integration: read confirmation %s: %w", id, err)
	case resp.Status != http.StatusOK:
		return nil, fmt.Errorf("integration: read confirmation %s: answered %d", id, resp.Status)
	case resp.Truncated:
		return nil, problem.Unprocessable("integration.wms_message_too_large", fmt.Sprintf("Confirmation %s is larger than %d bytes.", id, MaxWMSMessage))
	}
	return resp.Body, nil
}

// Acknowledge implements Warehouse.
func (s *WMSStandIn) Acknowledge(ctx context.Context, id string) error {
	resp, err := s.Client.Post(ctx, s.confirmation(id)+"/acknowledge", s.header(), nil)
	if err != nil {
		return fmt.Errorf("integration: acknowledge confirmation %s: %w", id, err)
	}
	if resp.Status != http.StatusNoContent && resp.Status != http.StatusOK {
		return fmt.Errorf("integration: acknowledge confirmation %s: answered %d", id, resp.Status)
	}
	return nil
}
