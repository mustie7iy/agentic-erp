package api

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

//go:embed asyncapi.yaml
var asyncapiYAML []byte

// WebhookEvents returns, sorted, the CloudEvents types a webhook may
// subscribe to: the messages marked x-webhook: true (ADR-0041). Each
// marked message names its type as the const of its headers' type in an
// allOf, as every message of the contract does.
func WebhookEvents() ([]string, error) { return parseWebhookEvents(asyncapiYAML) }

// MustWebhookEvents is WebhookEvents for program start: the embedded
// contract is checked by this package's tests, so failing to read it is a
// build defect.
func MustWebhookEvents() []string {
	types, err := parseWebhookEvents(asyncapiYAML)
	if err != nil {
		//nolint:forbidigo // init-time wiring of a static value: the embedded contract is fixed at build time and checked by this package's tests
		panic(err)
	}
	return types
}

type webhookMessage struct {
	Webhook bool `yaml:"x-webhook"`
	Headers struct {
		AllOf []struct {
			Properties struct {
				Type struct {
					Const string `yaml:"const"`
				} `yaml:"type"`
			} `yaml:"properties"`
		} `yaml:"allOf"`
	} `yaml:"headers"`
}

func parseWebhookEvents(src []byte) ([]string, error) {
	var doc struct {
		Components struct {
			Messages map[string]webhookMessage `yaml:"messages"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("api: parse asyncapi: %w", err)
	}
	var types []string
	for name, m := range doc.Components.Messages {
		if !m.Webhook {
			continue
		}
		t := ""
		for _, part := range m.Headers.AllOf {
			if c := part.Properties.Type.Const; c != "" {
				t = c
			}
		}
		if t == "" {
			return nil, fmt.Errorf("api: webhook message %s names no event type", name)
		}
		types = append(types, t)
	}
	if len(types) == 0 {
		return nil, errors.New("api: no message is marked x-webhook")
	}
	slices.Sort(types)
	return types, nil
}
