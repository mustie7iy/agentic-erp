// Package governance holds the controls the white paper puts "at the core":
// named agent identities, segregation of duties, autonomy limits, and a
// tamper-evident audit trail of every decision.
package governance

import (
	"bufio"
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
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
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
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{
		Seq: int64(len(l.events)) + 1, Time: l.now().UTC(), Actor: actor, Action: action,
		Subject: subject, Outcome: outcome, Detail: detail,
	}
	if n := len(l.events); n > 0 {
		e.PrevHash = l.events[n-1].Hash
	}
	h, err := hashEvent(e)
	if err != nil {
		return Event{}, err
	}
	e.Hash = h
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
		h, err := hashEvent(e)
		if err != nil {
			return err
		}
		if h != e.Hash {
			return fmt.Errorf("audit chain broken at seq %d: event was modified", e.Seq)
		}
		if e.Seq != int64(i)+1 {
			return fmt.Errorf("audit chain broken at seq %d: sequence gap", e.Seq)
		}
		prev = e.Hash
	}
	return nil
}

func hashEvent(e Event) (string, error) {
	e.Hash = ""
	b, err := json.Marshal(e) // struct field order and sorted map keys make this canonical
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
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
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
