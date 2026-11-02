package main

import (
	"context"
	"net"
	"net/http"
	"sync"
)

// requests are the requests the API and admin listeners serve, counted so
// the shutdown can wait for them, and given a context of their own the
// shutdown cancels (ADR-0038 As built: the replica's record outlives every
// request). http.Server.Shutdown returns at its deadline without waiting
// for the handlers still running and cancels none of them, so without
// this a request that read the configuration could commit after launch
// deleted the record, and an activation the replica lacks a key for could
// pass in that window.
type requests struct {
	ctx    context.Context // the servers' BaseContext: every request's context derives from it, and end cancels it
	cancel context.CancelFunc
	mu     sync.Mutex
	n      int        // handlers running
	idle   *sync.Cond // signalled, on mu, when n falls to 0
}

// newRequests is the requests of a server whose context is ctx: they keep
// its values but not its cancellation, which begins the drain rather than
// ending the requests.
func newRequests(ctx context.Context) *requests {
	q := &requests{}
	q.ctx, q.cancel = context.WithCancel(context.WithoutCancel(ctx))
	q.idle = sync.NewCond(&q.mu)
	return q
}

// base is the servers' BaseContext.
func (q *requests) base(net.Listener) context.Context { return q.ctx }

// track counts next's handlers while they run; a panicking handler is
// counted out too (net/http recovers it after the deferred call ran).
func (q *requests) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		q.n++
		q.mu.Unlock()
		defer func() {
			q.mu.Lock()
			if q.n--; q.n == 0 {
				q.idle.Broadcast()
			}
			q.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// end follows the servers' Shutdown, which drained what finished within
// ERP_SHUTDOWN_TIMEOUT: it cancels the requests still running, so the
// unit of work's transaction rolls back rather than commits, closes their
// connections, so a body still arriving (an upload's five minutes,
// ADR-0020) stops being read, and waits for every handler to return. A
// request that starts after the cancellation has a context already ended
// and cannot begin a transaction. A handler that ignored its context would
// hold the record until the pod is killed, which leaves the record to
// expire: the safe side.
func (q *requests) end(servers []*http.Server) {
	q.cancel()
	for _, srv := range servers {
		// Its listeners were closed by Shutdown; what Close reports of
		// them, or of a connection that was already going, changes nothing.
		_ = srv.Close()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.n > 0 {
		q.idle.Wait()
	}
}
