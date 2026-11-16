package integrationagent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/mustie7iy/agentic-erp/internal/apitools"
)

// subscription is what the run reads of a webhook subscription: never a
// secret, which the API lists for nobody.
type subscription struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	Description    string `json:"description"`
	Status         string `json:"status"`
	DisabledReason string `json:"disabled_reason"`
	FailingSince   string `json:"failing_since"`
}

// delivery is what the run reads of a webhook delivery.
type delivery struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	LastStatus int    `json:"last_status"`
	LastError  string `json:"last_error"`
}

// failed reports whether a delivery failed: it died, or it waits for
// another attempt after one failed.
func (d delivery) failed() bool {
	return d.Status == "dead" || (d.Status == "pending" && d.Attempts > 0 && (d.LastStatus >= http.StatusMultipleChoices || d.LastError != ""))
}

// outcome is a delivery's last attempt as the prompt gives it.
func (d delivery) outcome() string {
	if d.LastStatus != 0 {
		return fmt.Sprintf("HTTP %d", d.LastStatus)
	}
	return "error: " + d.LastError
}

// subscriptions notes the subscriptions that fail: disabled, failing
// since a failed delivery, or holding a failed delivery, with their
// failed deliveries, the newest first.
func (a *Agent) subscriptions(ctx context.Context, s survey) error {
	subs, err := list[subscription](ctx, a.API, "webhook subscriptions", "/v1/webhook-subscriptions", nil)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		failed, err := a.failedDeliveries(ctx, sub.ID)
		if err != nil {
			return err
		}
		if sub.Status != "disabled" && sub.FailingSince == "" && len(failed) == 0 {
			continue
		}
		s[sub.ID] = subscriptionFailing(sub, failed)
	}
	return nil
}

// failedDeliveries are a subscription's failed deliveries kept, at most
// MaxEvidence: those that died, then those waiting after a failed
// attempt, each the newest first. Delivered ones are not read.
func (a *Agent) failedDeliveries(ctx context.Context, sub string) ([]delivery, error) {
	var failed []delivery
	for _, status := range []string{"dead", "pending"} {
		err := apitools.Each(ctx, a.API, "/v1/webhook-subscriptions/"+url.PathEscape(sub)+"/deliveries", url.Values{"status": {status}}, func(d delivery) bool {
			if d.failed() {
				failed = append(failed, d)
			}
			return len(failed) < MaxEvidence
		})
		if err != nil {
			return nil, fmt.Errorf("integrationagent: the deliveries of webhook subscription %s: %w", sub, err)
		}
		if len(failed) == MaxEvidence {
			break
		}
	}
	return failed, nil
}

// subscriptionFailing is a failing subscription's entry: redeliver is
// given only with a delivery that died, pause only while it is active.
func subscriptionFailing(sub subscription, failed []delivery) *failing {
	f := &failing{objectType: SubscriptionObject, name: sub.Description, kinds: []string{CheckEndpoint, Investigate}}
	state := sub.Status
	if sub.DisabledReason != "" {
		state += " (" + sub.DisabledReason + ")"
	}
	if sub.FailingSince != "" {
		state += ", failing since " + sub.FailingSince
	}
	f.facts = append(f.facts, "endpoint "+sub.URL, "status "+state)
	ids, dead := make([]string, len(failed)), false
	for i, d := range failed {
		ids[i], dead = d.ID, dead || d.Status == "dead"
		f.facts = append(f.facts, fmt.Sprintf("delivery %s of %s: %s after %d attempt(s), the last %s", d.ID, d.Type, d.Status, d.Attempts, d.outcome()))
	}
	f.line = "Failing: subscription " + state
	if len(ids) > 0 {
		f.line += "; failed deliveries " + strings.Join(ids, ", ")
	}
	if dead {
		f.kinds = append(f.kinds, Redeliver)
	}
	if sub.Status == "active" {
		f.kinds = append(f.kinds, Pause)
	}
	return f
}

// bankFile is what the run reads of a bank file.
type bankFile struct {
	ID            string `json:"id"`
	PaymentRunID  string `json:"payment_run_id"`
	HouseBankCode string `json:"house_bank_code"`
	Format        string `json:"format"`
	Status        string `json:"status"`
}

// bankFiles notes the bank files the channel refused (failed), which may
// be transmitted again, and those the bank rejected.
func (a *Agent) bankFiles(ctx context.Context, s survey) error {
	for _, status := range []string{"failed", "rejected"} {
		files, err := list[bankFile](ctx, a.API, "bank files", "/v1/bank-files", url.Values{"status": {status}})
		if err != nil {
			return err
		}
		for _, b := range files {
			f := &failing{objectType: BankFileObject, name: strings.TrimSpace(b.Format + " from " + b.HouseBankCode), kinds: []string{Investigate},
				facts: []string{"payment run " + b.PaymentRunID, "status " + b.Status}, line: "Failing: bank file " + b.Status}
			if b.Status == "failed" {
				f.kinds = []string{Retransmit, Investigate}
			}
			s[b.ID] = f
		}
	}
	return nil
}

// connector is what the run reads of a connector: its secret_ref is a
// vault path, never a secret.
type connector struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// migrationRun is what the run reads of a migration run.
type migrationRun struct {
	ID          string `json:"id"`
	ConnectorID string `json:"connector_id"`
	Status      string `json:"status"`
	OperationID string `json:"operation_id"`
	CreatedAt   string `json:"created_at"`
}

// connectors notes the connectors whose latest migration run failed.
func (a *Agent) connectors(ctx context.Context, s survey) error {
	conns, err := list[connector](ctx, a.API, "connectors", "/v1/connectors", nil)
	if err != nil {
		return err
	}
	runs, err := list[migrationRun](ctx, a.API, "migration runs", "/v1/migration-runs", nil)
	if err != nil {
		return err
	}
	latest := map[string]migrationRun{}
	for _, r := range runs {
		// RFC 3339 instants in UTC compare as text; a tie keeps the larger id, a UUIDv7's later one.
		if l, ok := latest[r.ConnectorID]; !ok || r.CreatedAt > l.CreatedAt || (r.CreatedAt == l.CreatedAt && r.ID > l.ID) {
			latest[r.ConnectorID] = r
		}
	}
	for _, c := range conns {
		r, ok := latest[c.ID]
		if !ok || r.Status != "failed" {
			continue
		}
		s[c.ID] = &failing{objectType: ConnectorObject, name: c.Name, kinds: []string{CheckEndpoint, Investigate},
			facts: []string{"kind " + c.Kind + ", " + c.Status, fmt.Sprintf("latest migration run %s of %s: failed, operation %s", r.ID, r.CreatedAt, r.OperationID)},
			line:  "Failing: migration run " + r.ID + " failed"}
	}
	return nil
}
