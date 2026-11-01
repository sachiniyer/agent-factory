package daemon

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// The gate admits a sandbox credential only through its owner's own forward
// (authorizeSandbox, #5571 W7.8). Tests that drive sandbox routes through the
// gate stand in for the supervised tunnels with this fixture: every session gets
// its own process group, which carries the callback identity that session's
// CURRENT credential derives from, as a live tunnel would.

// setRefForTest records ref as the ref sessionID's current credential derives
// from, for a fixture that names its runtimes' refs.
func (r *sandboxTokenRegistry) setRefForTest(sessionID, ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refBySession == nil {
		r.refBySession = map[string]string{}
	}
	r.refBySession[sessionID] = ref
}

// ownForwards gives each session its own stand-in tunnel process group.
type ownForwards struct {
	reg    *sandboxTokenRegistry
	mu     sync.Mutex
	pgids  map[string]int
	owners map[int]string
	next   int
}

// installOwnForwards installs the stand-in as the package's carrier lookup for
// the rest of t; a pgid it did not hand out falls through to whatever lookup was
// installed before it.
func installOwnForwards(t *testing.T, reg *sandboxTokenRegistry) *ownForwards {
	t.Helper()
	o := &ownForwards{reg: reg, pgids: map[string]int{}, owners: map[int]string{}, next: 70000}
	prev := callbackCarrierRef
	callbackCarrierRef = func(pgid int) (string, bool) {
		o.mu.Lock()
		owner, ok := o.owners[pgid]
		o.mu.Unlock()
		if !ok {
			return prev(pgid)
		}
		ref := o.reg.refFor(owner)
		return ref, ref != ""
	}
	t.Cleanup(func() { callbackCarrierRef = prev })
	return o
}

// pgidFor is sessionID's own forward.
func (o *ownForwards) pgidFor(sessionID string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if pgid, ok := o.pgids[sessionID]; ok {
		return pgid
	}
	o.next++
	o.pgids[sessionID], o.owners[o.next] = o.next, sessionID
	return o.next
}

// carry sends r through the forward of the session that owns its bearer token,
// as the endpoint's ConnContext would record it. A request already carrying a
// carrier, or with a token no session holds, is left alone.
func (o *ownForwards) carry(r *http.Request) *http.Request {
	if carrierPGID(r.Context()) != 0 {
		return r
	}
	owner, ok := o.reg.sessionFor(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !ok {
		return r
	}
	return withCarrier(r, o.pgidFor(owner))
}

// through sends r through the forward of sessionID, whoever owns its token.
func (o *ownForwards) through(r *http.Request, sessionID string) *http.Request {
	return withCarrier(r, o.pgidFor(sessionID))
}

func withCarrier(r *http.Request, pgid int) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), carrierPGIDKey{}, pgid))
}

// processCarrier stands in for one session's forward when a test connects to the
// real endpoint from this process: the endpoint records this process's own
// group as the carrier, and the stand-in reports that group as carrying the
// chosen session's current callback identity.
type processCarrier struct {
	mu      sync.Mutex
	session string
}

func thisProcessCarries(t *testing.T, reg *sandboxTokenRegistry, sessionID string) *processCarrier {
	t.Helper()
	pc := &processCarrier{session: sessionID}
	self := unix.Getpgrp()
	prev := callbackCarrierRef
	callbackCarrierRef = func(pgid int) (string, bool) {
		if pgid != self {
			return prev(pgid)
		}
		pc.mu.Lock()
		owner := pc.session
		pc.mu.Unlock()
		ref := reg.refFor(owner)
		return ref, ref != ""
	}
	t.Cleanup(func() { callbackCarrierRef = prev })
	return pc
}

// carry makes this process the forward of sessionID from now on.
func (pc *processCarrier) carry(sessionID string) {
	pc.mu.Lock()
	pc.session = sessionID
	pc.mu.Unlock()
}
