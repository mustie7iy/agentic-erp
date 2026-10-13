package workflow

import (
	"strings"
	"testing"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// TestCodec: payloads are sealed and opened again; plain payloads pass
// through; a wrong key, a truncated or tampered payload, or sealed garbage
// are refused; the key must be 32 bytes.
func TestCodec(t *testing.T) {
	if _, err := NewCodec([]byte("short")); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("short key: %v", err)
	}
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)))
	other, _ := NewCodec([]byte(strings.Repeat("o", 32)))
	plain, _ := converter.GetDefaultDataConverter().ToPayload(map[string]any{"run_id": "0192a000-0000-7000-8000-000000000001"})
	sealed, err := c.Encode([]*commonpb.Payload{plain})
	if err != nil || string(sealed[0].Metadata[converter.MetadataEncoding]) != encodingSealed || strings.Contains(string(sealed[0].Data), "run_id") {
		t.Fatalf("sealed: %v %v", sealed, err)
	}
	opened, err := c.Decode(sealed)
	if err != nil || string(opened[0].Data) != string(plain.Data) || string(opened[0].Metadata[converter.MetadataEncoding]) != string(plain.Metadata[converter.MetadataEncoding]) {
		t.Fatalf("opened: %v %v", opened, err)
	}
	if passed, err := c.Decode([]*commonpb.Payload{plain}); err != nil || passed[0] != plain {
		t.Fatalf("plain payload: %v %v", passed, err)
	}
	if _, err := other.Decode(sealed); err == nil {
		t.Fatal("opened with another key")
	}
	short := &commonpb.Payload{Metadata: sealed[0].Metadata, Data: []byte{1, 2}}
	if _, err := c.Decode([]*commonpb.Payload{short}); err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short payload: %v", err)
	}
	nonce := make([]byte, c.aead.NonceSize())
	garbage := &commonpb.Payload{Metadata: sealed[0].Metadata, Data: c.aead.Seal(nonce, nonce, []byte{0xff, 0xff, 0xff}, nil)}
	if _, err := c.Decode([]*commonpb.Payload{garbage}); err == nil {
		t.Fatal("sealed garbage decoded")
	}
	if dataConverter(nil) != converter.GetDefaultDataConverter() || dataConverter(c) == converter.GetDefaultDataConverter() {
		t.Fatal("data converters")
	}
}
