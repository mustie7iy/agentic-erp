package api

import (
	"slices"
	"strings"
	"testing"
)

func TestWebhookEvents(t *testing.T) {
	types, err := WebhookEvents()
	if err != nil {
		t.Fatalf("WebhookEvents: %v", err)
	}
	if !slices.IsSorted(types) {
		t.Errorf("got %v, want sorted", types)
	}
	if !slices.Contains(types, "com.example.erp.tenancy.legal_entity_created.v1") {
		t.Errorf("got %v, want the legal entity created event among them", types)
	}
	for _, x := range types {
		if strings.HasPrefix(x, "com.example.erp.identity.") {
			t.Errorf("identity event %s offered to webhooks", x)
		}
	}
	if got := MustWebhookEvents(); !slices.Equal(got, types) {
		t.Errorf("MustWebhookEvents: got %v, want %v", got, types)
	}
}

func TestWebhookEventsRejectsBadContracts(t *testing.T) {
	for name, src := range map[string]string{
		"not yaml":  "components: [",
		"no type":   "components:\n  messages:\n    A:\n      x-webhook: true\n",
		"no marked": "components:\n  messages:\n    A:\n      headers:\n        allOf: [{properties: {type: {const: a}}}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWebhookEvents([]byte(src)); err == nil {
				t.Error("got no error, want the contract refused")
			}
		})
	}
}

func TestMustWebhookEventsPanics(t *testing.T) {
	saved := asyncapiYAML
	t.Cleanup(func() { asyncapiYAML = saved })
	asyncapiYAML = []byte("components: [")
	defer func() {
		if recover() == nil {
			t.Fatal("invalid contract did not panic")
		}
	}()
	MustWebhookEvents()
}
