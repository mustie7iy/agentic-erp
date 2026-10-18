package iban_test

import (
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/iban"
)

func TestValid(t *testing.T) {
	for in, want := range map[string]bool{
		"DE89370400440532013000": true, "DE89 3704 0044 0532 0130 00": true, "de89370400440532013000": true,
		"DE89370400440532013001": false, "DE8937040044": false, "1E89370400440532013000": false, "DE89-3704-0044-0532-0130-00": false,
	} {
		t.Run(in, func(t *testing.T) {
			if got := iban.Valid(in); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
