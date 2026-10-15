package workflow

import (
	"strings"
	"testing"

	"github.com/mustie7iy/agentic-erp/internal/platform/pgtest"
	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
)

// TestCodec: payloads are sealed and opened again; plain payloads pass
// through; a wrong key, a truncated or tampered payload, or sealed garbage
// are refused; the key must be 32 bytes.
func TestCodec(t *testing.T) {
	_, err := NewCodec([]byte("short"))
	pgtest.WantErr(t, err, "32 bytes")
	c, _ := NewCodec([]byte(strings.Repeat("k", 32)))
	other, _ := NewCodec([]byte(strings.Repeat("o", 32)))
	plain, _ := converter.GetDefaultDataConverter().ToPayload(map[string]any{"run_id": "0192a000-0000-7000-8000-000000000001"})
	sealed, err := c.Encode([]*commonpb.Payload{plain})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if got := string(sealed[0].Metadata[converter.MetadataEncoding]); got != encodingSealed {
		t.Errorf("sealed encoding: got %s, want %s", got, encodingSealed)
	}
	if got := string(sealed[0].Data); strings.Contains(got, "run_id") {
		t.Errorf("sealed data: got %q, want no plain text", got)
	}
	opened, err := c.Decode(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, want := string(opened[0].Data), string(plain.Data); got != want {
		t.Errorf("opened data: got %s, want %s", got, want)
	}
	if got, want := string(opened[0].Metadata[converter.MetadataEncoding]), string(plain.Metadata[converter.MetadataEncoding]); got != want {
		t.Errorf("opened encoding: got %s, want %s", got, want)
	}
	if passed, err := c.Decode([]*commonpb.Payload{plain}); err != nil || passed[0] != plain {
		t.Fatalf("plain payload: %v %v", passed, err)
	}
	if _, err := other.Decode(sealed); err == nil {
		t.Fatal("opened with another key")
	}
	short := &commonpb.Payload{Metadata: sealed[0].Metadata, Data: []byte{1, 2}}
	_, err = c.Decode([]*commonpb.Payload{short})
	pgtest.WantErr(t, err, "too short")
	nonce := make([]byte, c.aead.NonceSize())
	garbage := &commonpb.Payload{Metadata: sealed[0].Metadata, Data: c.aead.Seal(nonce, nonce, []byte{0xff, 0xff, 0xff}, nil)}
	if _, err := c.Decode([]*commonpb.Payload{garbage}); err == nil {
		t.Fatal("sealed garbage decoded")
	}
	if got := dataConverter(nil); got != converter.GetDefaultDataConverter() {
		t.Errorf("dataConverter(nil): got %v, want the default converter", got)
	}
	if got := dataConverter(c); got == converter.GetDefaultDataConverter() {
		t.Errorf("dataConverter(codec): got the default converter, want one that seals")
	}
}
