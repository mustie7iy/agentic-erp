// Command modelstub stands in for the model provider in the pilot
// walkthrough (scripts/walkthrough.sh), as the mock SAP extract stands in
// for SAP, so the walkthrough needs no live model or its key. It answers a
// Messages call (POST /v1/messages) in one of two ways:
//
//   - from a script of turns (-script), while that file exists and holds
//     turns: each call is answered by the next turn, a tool call or a final
//     text, as an evaluation suite's replay answers (ADR-0039, ADR-0058
//     §12), so an agent run through the stand-in calls the API as the
//     script says. A turn's input may name {{name}}, filled from the
//     script's vars: a regular expression over the request whose first
//     group is the value (the session id the agent opened, which the
//     walkthrough cannot know). A changed script starts again at its first
//     turn; a call past its last turn is refused.
//
//   - otherwise with the text of one file (-answer), which the walkthrough
//     writes as the document it uploads prints, so a capture's extraction
//     is known.
//
//     modelstub -addr 127.0.0.1:18081 -answer extraction.json -script turns.json
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18081", "where to listen")
	answer := flag.String("answer", "", "the file whose text every message answers")
	script := flag.String("script", "", "the file of turns that answer messages while it exists")
	flag.Parse()
	srv := &http.Server{Addr: *addr, Handler: handler(*answer, *script), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "modelstub:", err)
		os.Exit(1)
	}
}

// Script is a script of turns.
type Script struct {
	Vars  map[string]string `json:"vars"`
	Turns []Turn            `json:"turns"`
}

// Turn is one model turn: a tool call with its input, or a final text.
type Turn struct {
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Final string          `json:"final,omitempty"`
}

// player plays a script's turns in order, from the first again when the
// script changes.
type player struct {
	mu     sync.Mutex
	loaded []byte
	next   int
}

var placeholder = regexp.MustCompile(`\{\{([a-z][a-z0-9_]*)\}\}`)

// turn is the next turn of the script in raw for the request body, and
// its number; nil without a script.
func (p *player) turn(raw, body []byte) (*Turn, int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !bytes.Equal(raw, p.loaded) {
		p.loaded, p.next = raw, 0
	}
	var s Script
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, 0, fmt.Errorf("the script is not a script of turns: %w", err)
	}
	if p.next >= len(s.Turns) {
		return nil, 0, fmt.Errorf("the script's %d turns are played", len(s.Turns))
	}
	t := s.Turns[p.next]
	p.next++
	if t.Tool == "" {
		return &t, p.next, nil
	}
	input, err := fill(string(t.Input), s.Vars, string(body))
	if err != nil {
		return nil, 0, fmt.Errorf("turn %d: %w", p.next, err)
	}
	t.Input = json.RawMessage(input)
	return &t, p.next, nil
}

// fill replaces each {{name}} of input with the first group of the var's
// expression in the request body, as JSON string content.
func fill(input string, vars map[string]string, body string) (string, error) {
	var missing error
	out := placeholder.ReplaceAllStringFunc(input, func(m string) string {
		name := placeholder.FindStringSubmatch(m)[1]
		re, err := regexp.Compile(vars[name])
		if vars[name] == "" || err != nil {
			missing = fmt.Errorf("{{%s}} is no var of the script", name)
			return m
		}
		found := re.FindStringSubmatch(body)
		if len(found) < 2 {
			missing = fmt.Errorf("{{%s}}: the request has nothing that %s matches", name, vars[name])
			return m
		}
		quoted, _ := json.Marshal(found[1]) // a string always encodes
		return string(quoted[1 : len(quoted)-1])
	})
	if missing != nil {
		return "", missing
	}
	if !json.Valid([]byte(out)) {
		return "", errors.New("the filled input is not JSON")
	}
	return out, nil
}

func handler(answer, script string) http.Handler {
	p := &player{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		var req struct {
			Model string `json:"model"`
		}
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			http.Error(w, "modelstub: the request is not a message: "+err.Error(), http.StatusBadRequest)
			return
		}
		content, stop, err := reply(p, answer, script, body)
		if err != nil {
			http.Error(w, "modelstub: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{ // the gateway reports a broken answer as the provider failing
			"id": "msg_walkthrough", "type": "message", "role": "assistant", "model": req.Model,
			"content": content, "stop_reason": stop, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	})
	return mux
}

// reply is the answer's content and stop reason: the script's next turn
// while the script exists, else the answer file's text.
func reply(p *player, answer, script string, body []byte) ([]map[string]any, string, error) {
	var raw []byte
	if script != "" {
		var err error
		raw, err = os.ReadFile(script) // #nosec G304 -- the walkthrough names its own file
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("read the script: %w", err)
		}
	}
	t, n, err := p.turn(raw, body)
	switch {
	case err != nil:
		return nil, "", err
	case t != nil && t.Tool != "":
		return []map[string]any{{"type": "tool_use", "id": fmt.Sprintf("toolu_walkthrough_%d", n), "name": t.Tool, "input": t.Input}}, "tool_use", nil
	case t != nil:
		return []map[string]any{{"type": "text", "text": t.Final}}, "end_turn", nil
	}
	text, err := os.ReadFile(answer) // #nosec G304 -- the walkthrough names its own file
	if err != nil {
		return nil, "", fmt.Errorf("read the answer: %w", err)
	}
	return []map[string]any{{"type": "text", "text": string(text)}}, "end_turn", nil
}
