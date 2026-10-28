package daemon

import (
	"net/http"
	"sync"
)

// httpRequestDrain closes admission to the finite RPC-shaped HTTP routes and
// joins the handlers that were already dispatched. Long-lived stream/proxy
// routes are deliberately outside this drain: they do not mutate checkpointed
// session state and their connections are cut by the HTTP servers themselves.
type httpRequestDrain struct {
	mu        sync.Mutex
	cond      *sync.Cond
	accepting bool
	active    int
}

func newHTTPRequestDrain() *httpRequestDrain {
	drain := &httpRequestDrain{accepting: true}
	drain.cond = sync.NewCond(&drain.mu)
	return drain
}

func (d *httpRequestDrain) enter() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.accepting {
		return false
	}
	d.active++
	return true
}

func (d *httpRequestDrain) leave() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.active--
	if d.active == 0 {
		d.cond.Broadcast()
	}
}

func (d *httpRequestDrain) closeAdmission() {
	d.mu.Lock()
	d.accepting = false
	d.mu.Unlock()
}

func (d *httpRequestDrain) wait() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.active != 0 {
		d.cond.Wait()
	}
}

func (s *controlServer) trackHTTPRPC(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.httpRequests == nil {
			next(w, r)
			return
		}
		if !s.httpRequests.enter() {
			writeHTTPError(w, r, http.StatusServiceUnavailable, errDaemonQuiescing())
			return
		}
		defer s.httpRequests.leave()
		next(w, r)
	}
}
