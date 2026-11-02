package pgtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPausedBody: a server reads the first part, the body pauses until
// Resume, then the rest comes; Send gives the answer; a client's failure
// comes as Sent's error; Close ends a pause.
func TestPausedBody(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body) // the test's own body, whole
		got <- string(b)
		_, _ = w.Write([]byte("done")) // to a live test client
	}))
	t.Cleanup(srv.Close)
	body := NewPausedBody([]byte("first "), []byte("rest"))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, body) // a test server's URL parses
	sent := Send(srv.Client(), req)
	<-body.Paused()
	body.Resume()
	body.Resume() // twice is fine
	if answer := <-sent; answer.Err != nil || answer.Status != http.StatusOK || answer.Body != "done" {
		t.Errorf("answer: got %d %q %v, want 200 done", answer.Status, answer.Body, answer.Err)
	}
	if b := <-got; b != "first rest" {
		t.Errorf("body: got %q, want %q", b, "first rest")
	}
	bad, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1", nil) // a URL that parses
	if answer := <-Send(srv.Client(), bad); answer.Err == nil {
		t.Errorf("unreachable: got %d, want an error", answer.Status)
	}
	closed := NewPausedBody(nil, []byte("x"))
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(closed); string(b) != "x" {
		t.Errorf("after Close: got %q, want the rest without a pause", b)
	}
}
