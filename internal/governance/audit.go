// Package governance holds the controls the white paper puts "at the core":
// named agent identities, segregation of duties, autonomy limits, and a
// tamper-evident audit trail of every decision.
package governance

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one auditable action by a human, a rule, or an agent.
type Event struct {
	Seq      int64          `json:"seq"`
	Time     time.Time      `json:"time"`
	Actor    string         `json:"actor"`
	Action   string         `json:"action"`
	Subject  string         `json:"subject,omitempty"`
	Outcome  string         `json:"outcome"`
	Detail   map[string]any `json:"detail,omitempty"`
	PrevHash string         `json:"prevHash"`
	Hash     string         `json:"hash"`
}

// AuditLog is an append-only, hash-chained log. Changing or deleting any
// past event breaks the chain, which Verify detects.
type AuditLog struct {
	mu     sync.Mutex
	events []Event
	path   string
	now    func() time.Time
}

// NewAuditLog keeps events in memory; with a path, events are also appended
// to a JSONL file and existing events are loaded and verified.
func NewAuditLog(path string) (*AuditLog, error) {
	l := &AuditLog{path: path, now: time.Now}
	if path == "" {
		return l, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e Event
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.UseNumber()
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("audit log %s: %w", path, err)
		}
		l.events = append(l.events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := l.Verify(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *AuditLog) Append(actor, action, subject, outcome string, detail map[string]any) (Event, error) {
	norm, err := normalize(detail)
	if err != nil {
		return Event{}, fmt.Errorf("audit detail: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{
		Seq: int64(len(l.events)) + 1, Time: l.now().UTC(), Actor: actor, Action: action,
		Subject: subject, Outcome: outcome, Detail: norm,
	}
	if n := len(l.events); n > 0 {
		e.PrevHash = l.events[n-1].Hash
	}
	e.Hash = hashEvent(e)
	if l.path != "" {
		if err := appendLine(l.path, e); err != nil {
			return Event{}, err
		}
	}
	l.events = append(l.events, e)
	return e, nil
}

func (l *AuditLog) Events() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.events...)
}

// Verify recomputes the chain and reports the first broken link.
func (l *AuditLog) Verify() error {
	prev := ""
	for i, e := range l.events {
		if e.PrevHash != prev {
			return fmt.Errorf("audit chain broken at seq %d: previous hash mismatch", e.Seq)
		}
		if hashEvent(e) != e.Hash {
			return fmt.Errorf("audit chain broken at seq %d: event was modified", e.Seq)
		}
		if e.Seq != int64(i)+1 {
			return fmt.Errorf("audit chain broken at seq %d: sequence gap", e.Seq)
		}
		prev = e.Hash
	}
	return nil
}

// normalize converts detail to the generic form it has after a JSON round
// trip (maps with sorted keys, exact numbers), so an event hashes the same
// when written and when reloaded from disk.
func normalize(detail map[string]any) (map[string]any, error) {
	if detail == nil {
		return nil, nil
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	_ = dec.Decode(&out) // decoding JSON we just encoded cannot fail
	return out, nil
}

// hashEvent hashes an event's canonical JSON (struct field order and
// sorted map keys). Details are normalized to JSON values before an event
// is created, so marshaling cannot fail.
func hashEvent(e Event) string {
	e.Hash = ""
	b, _ := json.Marshal(e)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func appendLine(path string, e Event) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(e) // normalized event: cannot fail
	_, err = f.Write(append(b, '\n'))
	return err
}
