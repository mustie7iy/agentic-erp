package integration_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mustie7iy/agentic-erp/api/gen"
	"github.com/mustie7iy/agentic-erp/internal/app/apitest"
	"github.com/mustie7iy/agentic-erp/internal/integration"
	"github.com/mustie7iy/agentic-erp/internal/platform/database"
	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	"github.com/mustie7iy/agentic-erp/internal/platform/webhook"
)

const hooks = "/v1/webhook-subscriptions"

// hookURL is a public address written as an IP literal, so the check at
// write time needs no DNS.
const hookURL = "https://93.184.216.34/erp?token=tok-123"

// webhookClient is a tenant whose human may read and manage webhooks.
func webhookClient(t *testing.T) apitest.Client {
	t.Helper()
	ten := pgtest.NewTenant(t, "integration.webhook.read", "integration.webhook.manage")
	return apitest.Client{T: t, API: apitest.New(t), Tenant: ten, As: ten.Human}
}

func createHook(c apitest.Client, types string) (map[string]any, string) {
	c.T.Helper()
	return c.Must(201, "POST", hooks, `{"url":"`+hookURL+`","description":"portal","event_types":[`+types+`]}`, "")
}

// key is the key of a whsec_ secret.
func key(t *testing.T, secret any) []byte {
	t.Helper()
	s, ok := secret.(string)
	if _, err := webhook.ParseSecret(s); !ok || err != nil {
		t.Fatalf("secret %v: %v, want a whsec_ secret", secret, err)
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sealedKey opens a sealed secret column of a subscription.
func sealedKey(t *testing.T, c apitest.Client, id any, column string) []byte {
	t.Helper()
	var sealed []byte
	pgtest.Scan(t, `SELECT `+column+` FROM integration.webhook_subscription WHERE id = $1`, []any{id}, &sealed)
	if sealed == nil {
		return nil
	}
	// Inside a request the ring finds the tenant's own key, if it has one.
	var plain string
	if err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, func(ctx context.Context) (err error) {
		plain, err = apitest.FieldKeys().Open(ctx, c.Tenant.ID, sealed)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k, err := hex.DecodeString(plain)
	if err != nil {
		t.Fatalf("sealed secret %q is not hex: %v", plain, err)
	}
	return k
}

// TestWebhookSubscriptionLifecycle: create shows the secret once, sealed;
// reads never show it; update, rotation and delete are guarded by If-Match
// and audited; a deleted subscription stays, out of the default list
// (ADR-0041).
func TestWebhookSubscriptionLifecycle(t *testing.T) {
	c := webhookClient(t)
	created, tag := createHook(c, `"ledger.*","com.example.erp.tenancy.legal_entity_created.v1","ledger.*"`)
	id := created["id"].(string)
	first := key(t, created["secret"])
	if got := sealedKey(t, c, id, "secret_cipher"); string(got) != string(first) {
		t.Errorf("sealed secret: got %x, want %x", got, first)
	}
	if got := fmt.Sprint(created["event_types"]); got != "[ledger.* com.example.erp.tenancy.legal_entity_created.v1]" {
		t.Errorf("event types: got %s, want the duplicate dropped", got)
	}
	got, _ := c.Must(200, "GET", hooks+"/"+id, "", "")
	if _, ok := got["secret"]; ok {
		t.Error("get shows the secret")
	}
	if got["url"] != hookURL || got["status"] != "active" {
		t.Errorf("get: got %v", got)
	}
	updated, tag := c.Must(200, "PATCH", hooks+"/"+id, `{"description":"portal 2","event_types":["payables.*"]}`, tag)
	if updated["description"] != "portal 2" || fmt.Sprint(updated["event_types"]) != "[payables.*]" || updated["url"] != hookURL {
		t.Errorf("update: got %v", updated)
	}
	c.Problem(412, "request.version_mismatch", "POST", hooks+"/"+id+":rotateSecret", "", `"1"`)
	rotated, tag := c.Must(200, "POST", hooks+"/"+id+":rotateSecret", "", tag)
	second := key(t, rotated["secret"])
	if string(second) == string(first) {
		t.Error("rotation kept the secret")
	}
	if got := sealedKey(t, c, id, "previous_secret_cipher"); string(got) != string(first) {
		t.Errorf("previous secret: got %x, want %x", got, first)
	}
	expires, err := time.Parse(time.RFC3339Nano, fmt.Sprint(rotated["previous_secret_expires_at"]))
	if err != nil || time.Until(expires) < integration.SecretOverlap-time.Hour || time.Until(expires) > integration.SecretOverlap {
		t.Errorf("previous secret expires at %v (%v), want in about 24 hours", rotated["previous_secret_expires_at"], err)
	}
	deleted, tag := c.Must(200, "POST", hooks+"/"+id+":delete", "", tag)
	if deleted["status"] != "deleted" || deleted["previous_secret_expires_at"] != nil {
		t.Errorf("delete: got %v, want deleted without a previous secret", deleted)
	}
	for _, call := range [][2]string{{"PATCH", ""}, {"POST", ":delete"}, {"POST", ":rotateSecret"}} {
		c.Problem(409, "integration.webhook_deleted", call[0], hooks+"/"+id+call[1], `{"description":"x"}`, tag)
	}
	if page, _ := c.Must(200, "GET", hooks, "", ""); len(page["items"].([]any)) != 0 {
		t.Errorf("default list: got %v, want the deleted subscription left out", page["items"])
	}
	if page, _ := c.Must(200, "GET", hooks+"?status=deleted", "", ""); len(page["items"].([]any)) != 1 {
		t.Errorf("deleted list: got %v, want the subscription", page["items"])
	}
	c.Problem(404, "request.not_found", "GET", hooks+"/"+uuid.NewString(), "", "")
	var actions string
	pgtest.Scan(t, `SELECT string_agg(action, ',' ORDER BY occurred_at, action) FROM audit.audit_event WHERE object_id = $1`, []any{id}, &actions)
	if actions != "integration.webhook_subscription.create,integration.webhook_subscription.update,integration.webhook_subscription.rotate_secret,integration.webhook_subscription.delete" {
		t.Errorf("audit: got %s", actions)
	}
	var leaked int
	pgtest.Scan(t, `SELECT count(*) FROM audit.audit_event WHERE object_id = $1 AND (detail::text LIKE '%tok-123%' OR detail::text LIKE '%whsec_%')`, []any{id}, &leaked)
	if leaked != 0 {
		t.Errorf("audit entries with the URL's token or the secret: got %d, want 0", leaked)
	}
}

// TestWebhookSecretNotReplayed: a replayed create answers without the
// secret, which is not kept with the idempotency key.
func TestWebhookSecretNotReplayed(t *testing.T) {
	c := webhookClient(t)
	req := apitest.Req{Method: "POST", Path: hooks, Body: `{"url":"` + hookURL + `","event_types":["ledger.*"]}`,
		Tenant: c.Tenant.ID, As: c.As, Key: apitest.NewKey()}
	if first := apitest.JSON(t, c.API.Do(req)); first["secret"] == nil {
		t.Fatalf("first answer: got %v, want the secret", first)
	}
	replay := c.API.Do(req)
	if body := apitest.JSON(t, replay); replay.Code != 201 || body["secret"] != nil || body["id"] == nil {
		t.Errorf("replay: got %d %v, want 201 without the secret", replay.Code, body)
	}
	var stored int
	pgtest.Scan(t, `SELECT count(*) FROM platform.idempotency_key WHERE tenant_id = $1 AND response_body::text LIKE '%whsec_%'`, []any{c.Tenant.ID}, &stored)
	if stored != 0 {
		t.Errorf("stored answers with the secret: got %d, want 0", stored)
	}
}

// TestWebhookSubscriptionRefusals: URLs the egress client would refuse,
// event types not offered to webhooks, and more than 20 subscriptions.
func TestWebhookSubscriptionRefusals(t *testing.T) {
	c := webhookClient(t)
	for name, url := range map[string]string{
		"http": "http://93.184.216.34/erp", "credentials": "https://u:p@93.184.216.34/erp", "fragment": "https://93.184.216.34/erp#x",
		"private": "https://10.0.0.1/erp", "loopback name": "https://localhost/erp", "metadata": "https://169.254.169.254/latest",
	} {
		t.Run(name, func(t *testing.T) {
			c.In(t).Problem(422, "integration.invalid_webhook_url", "POST", hooks, `{"url":"`+url+`","event_types":["ledger.*"]}`, "")
		})
	}
	for name, types := range map[string]string{
		"identity context": `"identity.*"`, "unknown context": `"nothing.*"`, "event with a wildcard": `"ledger.journal_entry_posted.*"`,
		"unmarked event": `"com.example.erp.identity.role_assigned.v1"`, "unknown version": `"com.example.erp.tenancy.legal_entity_created.v9"`,
	} {
		t.Run(name, func(t *testing.T) {
			c.In(t).Problem(422, "integration.unknown_event_type", "POST", hooks, `{"url":"`+hookURL+`","event_types":[`+types+`]}`, "")
		})
	}
	created, tag := createHook(c, `"ledger.*"`)
	path := hooks + "/" + created["id"].(string)
	c.Problem(422, "integration.invalid_webhook_url", "PATCH", path, `{"url":"https://192.168.1.1/x"}`, tag)
	c.Problem(422, "integration.unknown_event_type", "PATCH", path, `{"event_types":["identity.*"]}`, tag)
	updated, _ := c.Must(200, "PATCH", path, `{"url":"https://93.184.216.35/new"}`, tag)
	if updated["url"] != "https://93.184.216.35/new" {
		t.Errorf("url: got %v", updated["url"])
	}

	// The limit counts the subscriptions not deleted.
	for range integration.MaxSubscriptions - 2 {
		createHook(c, `"ledger.*"`)
	}
	last, lastTag := createHook(c, `"ledger.*"`)
	c.Problem(422, "integration.webhook_limit", "POST", hooks, `{"url":"`+hookURL+`","event_types":["ledger.*"]}`, "")
	c.Must(200, "POST", hooks+"/"+last["id"].(string)+":delete", "", lastTag)
	createHook(c, `"ledger.*"`)
	if page, _ := c.Must(200, "GET", hooks+"?page_size=100", "", ""); len(page["items"].([]any)) != integration.MaxSubscriptions {
		t.Errorf("list: got %d, want %d", len(page["items"].([]any)), integration.MaxSubscriptions)
	}
	c.Problem(400, "request.invalid_page_token", "GET", hooks+"?page_token=x", "", "")
	page, _ := c.Must(200, "GET", hooks+"?page_size=15", "", "")
	rest, _ := c.Must(200, "GET", hooks+"?page_size=15&page_token="+page["next_page_token"].(string), "", "")
	if n := len(page["items"].([]any)) + len(rest["items"].([]any)); n != integration.MaxSubscriptions || rest["next_page_token"] != nil {
		t.Errorf("two pages: got %d subscriptions, next %v; want %d and no more", n, rest["next_page_token"], integration.MaxSubscriptions)
	}

	// What the dispatcher records is shown.
	pgtest.Exec(t, `UPDATE integration.webhook_subscription SET status = 'disabled', disabled_reason = 'failing', failing_since = now(), version = version + 1 WHERE id = $1`, created["id"])
	disabled, _ := c.Must(200, "GET", path, "", "")
	if disabled["disabled_reason"] != "failing" || disabled["failing_since"] == nil {
		t.Errorf("disabled: got %v, want its reason and when it began failing", disabled)
	}
}

// TestWebhookSecretsResealed: rotating the tenant's field key re-seals both
// secrets of a subscription, and passes over a missing previous one.
func TestWebhookSecretsResealed(t *testing.T) {
	c := webhookClient(t)
	rotating, tag := createHook(c, `"ledger.*"`)
	rotated, _ := c.Must(200, "POST", hooks+"/"+rotating["id"].(string)+":rotateSecret", "", tag)
	plain, _ := createHook(c, `"ledger.*"`)
	r := apitest.FieldKeys()
	r.Pool = pgtest.Pool(t)
	if err := r.Pool.InTx(context.Background(), database.Scope{Role: "erp_app", TenantID: c.Tenant.ID}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := r.Rotate(ctx, tx, c.Tenant.ID)
		return err
	}); err != nil {
		t.Fatalf("rotate the tenant key: %v", err)
	}
	if got := sealedKey(t, c, rotating["id"], "secret_cipher"); string(got) != string(key(t, rotated["secret"])) {
		t.Error("current secret not re-sealed")
	}
	if got := sealedKey(t, c, rotating["id"], "previous_secret_cipher"); string(got) != string(key(t, rotating["secret"])) {
		t.Error("previous secret not re-sealed")
	}
	if got := sealedKey(t, c, plain["id"], "previous_secret_cipher"); got != nil {
		t.Errorf("previous secret of a subscription never rotated: got %x, want none", got)
	}
}

// failingSealer fails every seal.
type failingSealer struct{}

func (failingSealer) Seal(context.Context, uuid.UUID, string) ([]byte, error) {
	return nil, errors.New("no key")
}

// TestWebhookDatabaseFailures calls every webhook handler outside a unit of
// work, with each of its statements failing in turn, and with sealing
// failing.
func TestWebhookDatabaseFailures(t *testing.T) {
	c := webhookClient(t)
	created, _ := createHook(c, `"ledger.*"`)
	id := uuid.MustParse(created["id"].(string))
	h := endpoints("")
	anyVersion := gen.UpdateWebhookSubscriptionParams{ID: id, IfMatch: "*"}
	calls := map[string]func(ctx context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := h.CreateWebhookSubscription(ctx, &gen.WebhookSubscriptionCreate{URL: hookURL, EventTypes: []string{"ledger.*"}}, gen.CreateWebhookSubscriptionParams{})
			return err
		},
		"get": func(ctx context.Context) error {
			_, err := h.GetWebhookSubscription(ctx, gen.GetWebhookSubscriptionParams{ID: id})
			return err
		},
		"list": func(ctx context.Context) error {
			_, err := h.ListWebhookSubscriptions(ctx, gen.ListWebhookSubscriptionsParams{})
			return err
		},
		"update": func(ctx context.Context) error {
			_, err := h.UpdateWebhookSubscription(ctx, &gen.WebhookSubscriptionUpdate{URL: gen.NewOptString(hookURL)}, anyVersion)
			return err
		},
		"rotate": func(ctx context.Context) error {
			_, err := h.RotateWebhookSubscriptionSecret(ctx, gen.RotateWebhookSubscriptionSecretParams{ID: id, IfMatch: "*"})
			return err
		},
		"delete": func(ctx context.Context) error {
			_, err := h.DeleteWebhookSubscription(ctx, gen.DeleteWebhookSubscriptionParams{ID: id, IfMatch: "*"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(context.Background()); err == nil {
				t.Error("outside a unit of work: got nil error, want an error")
			}
			apitest.EveryFailure(t, c.Tenant, c.Tenant.Human, name, call)
		})
	}
	h.Keys = failingSealer{}
	for _, name := range []string{"create", "rotate"} {
		t.Run("sealing fails on "+name, func(t *testing.T) {
			err := apitest.InRequest(t, c.Tenant, c.Tenant.Human, calls[name])
			if err == nil || !strings.Contains(err.Error(), "seal webhook secret") {
				t.Errorf("got %v, want the sealing error", err)
			}
		})
	}
}
