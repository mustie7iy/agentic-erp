package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// post sends a message to the stand-in and returns the status and answer.
func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestScript: while the script exists its turns answer in order, a tool
// call's input filled from the request; a changed script starts again; a
// call past the last turn is refused; without the script the answer file
// answers.
func TestScript(t *testing.T) {
	dir := t.TempDir()
	answer, script := filepath.Join(dir, "answer.json"), filepath.Join(dir, "turns.json")
	write(t, answer, `{"invoice_number": "RE-1"}`)
	srv := httptest.NewServer(handler(answer, script))
	t.Cleanup(srv.Close)
	msg := `{"model": "erp-close", "messages": [{"role": "user", "content": "Your session id is 0192a000-0000-7000-8000-000000000d01."}]}`
	steps := []struct {
		name, script, body string
		status             int
		want               string
	}{
		{"no script: the answer file", "", msg, 200, `RE-1`},
		{"the first turn, filled", `{"vars": {"session": "session id is ([0-9a-f-]{36})"}, "turns": [{"tool": "proposeAccrual", "input": {"body": {"session_id": "{{session}}"}}}, {"final": "Done."}]}`,
			msg, 200, `"session_id":"0192a000-0000-7000-8000-000000000d01"`},
		{"the second turn", "", msg, 200, `"text":"Done."`},
		{"past the last turn", "", msg, 500, "turns are played"},
		{"a changed script starts again", `{"turns": [{"final": "Again."}]}`, msg, 200, `"text":"Again."`},
		{"a var the request lacks", `{"vars": {"session": "session id is ([0-9a-f-]{36})"}, "turns": [{"tool": "x", "input": {"id": "{{session}}"}}]}`,
			`{"model": "erp-close"}`, 500, "nothing that"},
		{"a placeholder that is no var", `{"turns": [{"tool": "x", "input": {"id": "{{close}}"}}]}`, msg, 500, "no var"},
		{"a var that is no expression", `{"vars": {"close": "("}, "turns": [{"tool": "x", "input": {"id": "{{close}}"}}]}`, msg, 500, "no var"},
		{"a value that breaks no JSON", `{"vars": {"q": "(\")"}, "turns": [{"tool": "x", "input": {"id": "{{q}}"}}]}`, `{"model": "a\"b"}`, 200, `"id":"\""`},
		{"a script that is no script", `[`, msg, 500, "not a script"},
		{"no message", `{"turns": [{"final": "x"}]}`, `{`, 400, "not a message"},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if s.script != "" {
				write(t, script, s.script)
			}
			status, got := post(t, srv.URL, s.body)
			if status != s.status || !strings.Contains(got, s.want) {
				t.Errorf("got %d %s, want %d with %s", status, got, s.status, s.want)
			}
		})
	}
	t.Run("no answer file", func(t *testing.T) {
		if err := os.Remove(script); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(answer); err != nil {
			t.Fatal(err)
		}
		status, got := post(t, srv.URL, msg)
		if status != 500 || !strings.Contains(got, "read the answer") {
			t.Errorf("got %d %s, want 500 with read the answer", status, got)
		}
	})
	t.Run("a script that cannot be read", func(t *testing.T) {
		if err := os.Mkdir(script, 0o700); err != nil {
			t.Fatal(err)
		}
		status, got := post(t, srv.URL, msg)
		if status != 500 || !strings.Contains(got, "read the script") {
			t.Errorf("got %d %s, want 500 with read the script", status, got)
		}
	})
}
