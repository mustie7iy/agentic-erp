package pgtest

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

// PausedBody is a request body sent in two parts with a pause between them
// that the test ends: a client slower than a listener's read timeout,
// without sleeping. Read gives the first part, then closes Paused and
// waits for Resume (or Close) before giving the rest. A transport sends a
// body of unknown length chunked and flushes each chunk, so the first part
// reaches the server before the pause.
type PausedBody struct {
	first, rest           *bytes.Reader
	paused, resume        chan struct{}
	pauseOnce, resumeOnce sync.Once
}

// NewPausedBody is a body of first, a pause, then rest.
func NewPausedBody(first, rest []byte) *PausedBody {
	return &PausedBody{first: bytes.NewReader(first), rest: bytes.NewReader(rest), paused: make(chan struct{}), resume: make(chan struct{})}
}

// Read implements io.Reader.
func (b *PausedBody) Read(p []byte) (int, error) {
	if b.first.Len() > 0 {
		return b.first.Read(p) //nolint:wrapcheck // a bytes.Reader's io.EOF, which the transport must see as it is
	}
	b.pauseOnce.Do(func() { close(b.paused) })
	<-b.resume
	return b.rest.Read(p) //nolint:wrapcheck // as above
}

// Paused is closed once the first part has been read and Read waits.
func (b *PausedBody) Paused() <-chan struct{} { return b.paused }

// Resume ends the pause; the rest follows.
func (b *PausedBody) Resume() { b.resumeOnce.Do(func() { close(b.resume) }) }

// Close ends the pause too, so a transport that gives up on the request
// (the server cut it) never waits on its body.
func (b *PausedBody) Close() error {
	b.Resume()
	return nil
}

// Sent is the answer to a request sent in the background (Send): its
// status and body, or the client's error.
type Sent struct {
	Status int
	Body   string
	Err    error
}

// Send sends req with c in the background and gives its answer on the
// channel once it comes, the response's body read and closed: a test waits
// on it for the server's answer to a body still paused, without polling.
func Send(c *http.Client, req *http.Request) <-chan Sent {
	out := make(chan Sent, 1)
	go func() {
		resp, err := c.Do(req) // #nosec G704 -- a test's own request to its own test server
		if err != nil {
			out <- Sent{Err: err}
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // read whole or failed; closing changes neither
		out <- Sent{Status: resp.StatusCode, Body: string(body), Err: err}
	}()
	return out
}
