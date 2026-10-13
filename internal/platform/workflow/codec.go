package workflow

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/sdk/converter"
	"google.golang.org/protobuf/proto"
)

// Payload encryption (ADR-0012: sensitive payloads are encrypted through a
// payload codec). Workflow arguments, results and activity payloads pass
// through the engine and stay in its history; with a codec key they are
// sealed with AES-256-GCM before they leave the process, so the cluster
// stores ciphertext only. The encoding marks sealed payloads, and payloads
// written without a codec are read as they are, so a key can be introduced
// on a running cell.

const encodingSealed = "binary/encrypted-aes256gcm"

// Codec seals payloads with one AES-256 key.
type Codec struct {
	aead cipher.AEAD
}

// NewCodec makes a codec from a 32-byte key.
func NewCodec(key []byte) (*Codec, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("workflow: the payload codec key must be 32 bytes, got %d", len(key))
	}
	block, _ := aes.NewCipher(key) // a 32-byte key never fails
	aead, _ := cipher.NewGCM(block)
	return &Codec{aead: aead}, nil
}

// Encode implements converter.PayloadCodec.
func (c *Codec) Encode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		plain, _ := proto.Marshal(p) // a payload is plain fields: marshalling cannot fail
		nonce := make([]byte, c.aead.NonceSize())
		_, _ = rand.Read(nonce) // crypto/rand never fails on supported platforms
		out[i] = &commonpb.Payload{Metadata: map[string][]byte{converter.MetadataEncoding: []byte(encodingSealed)}, Data: c.aead.Seal(nonce, nonce, plain, nil)}
	}
	return out, nil
}

// Decode implements converter.PayloadCodec; payloads not sealed by a codec
// pass through.
func (c *Codec) Decode(payloads []*commonpb.Payload) ([]*commonpb.Payload, error) {
	out := make([]*commonpb.Payload, len(payloads))
	for i, p := range payloads {
		if string(p.GetMetadata()[converter.MetadataEncoding]) != encodingSealed {
			out[i] = p
			continue
		}
		n := c.aead.NonceSize()
		if len(p.Data) < n {
			return nil, errors.New("workflow: sealed payload too short")
		}
		plain, err := c.aead.Open(nil, p.Data[:n], p.Data[n:], nil)
		if err != nil {
			return nil, fmt.Errorf("workflow: sealed payload: %w", err)
		}
		var decoded commonpb.Payload
		if err := proto.Unmarshal(plain, &decoded); err != nil {
			return nil, fmt.Errorf("workflow: sealed payload: decode: %w", err)
		}
		out[i] = &decoded
	}
	return out, nil
}

// dataConverter is the default converter, sealed with the codec when there
// is one.
func dataConverter(c *Codec) converter.DataConverter {
	if c == nil {
		return converter.GetDefaultDataConverter()
	}
	return converter.NewCodecDataConverter(converter.GetDefaultDataConverter(), c)
}
