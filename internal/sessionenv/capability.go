//go:build !windows

package sessionenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A launch capability is the only thing that lets the exec shim inject an
// account's scope (#4731). The launcher that decided what a pane runs — the
// process that calls Start — mints it IN MEMORY; the shim claims it over a unix
// socket the same process serves, and the claim is checked against the pane the
// kernel and tmux say the caller is, not against anything the caller asserts.
//
// Why a daemon-held record rather than a token file: a same-user child can write
// a forged token, move the real one away before the shim reads it, and put it
// back after the shim deleted it. None of that is possible for a record that
// exists only in the minting process and can be created only by that process:
// the socket exposes claim and nothing else.
//
// The capability CARRIES the admitted values (agent, account, exact command,
// passthrough names, launch proof). The shim uses those and ignores its own
// argv and environment, so a config edit after admission, an LD_PRELOAD added to
// the passthrough list, or a root the launcher resolved differently cannot make
// the shim re-derive a different launch.

// Capability is what a launcher admitted for one pane command.
type Capability struct {
	// Session is the tmux session the claiming pane must belong to. It is known
	// before the pane exists, unlike the pane id, so mint and claim cannot race.
	Session         string
	Agent           string
	Account         string
	Command         string
	Extras          []string
	Proof           AccountLaunchProof
	EnvironmentOnly bool
	// PerPane marks the capability tmux's default-command re-runs for every new
	// window: each pane may claim it once. Everything else is claimed once, total.
	PerPane bool
}

// PaneInfo is what tmux reports about a pane.
type PaneInfo struct {
	Session string
	RootPID int
}

// PaneLookup asks tmux about a pane id. A var-free seam: the tmux package
// supplies it, because this package cannot import tmux.
type PaneLookup func(ctx context.Context, paneID string) (PaneInfo, error)

const (
	capabilityTTL        = 15 * time.Minute
	capabilityMaxRecords = 1024
	claimIOTimeout       = 5 * time.Second
	claimMaxMessage      = 64 << 10
)

type capRecord struct {
	cap     Capability
	expires time.Time
	panes   map[string]bool // PerPane: "<pane id>:<root pid>" already claimed
}

// CapabilityBroker holds the minted records and answers claims.
type CapabilityBroker struct {
	mu     sync.Mutex
	recs   map[string]*capRecord
	lookup PaneLookup
	now    func() time.Time
}

// NewCapabilityBroker returns a broker that verifies claims through lookup.
func NewCapabilityBroker(lookup PaneLookup) *CapabilityBroker {
	return &CapabilityBroker{recs: make(map[string]*capRecord), lookup: lookup, now: time.Now}
}

// Mint records c and returns the bearer token the launcher puts in the pane's
// start command. The token alone proves nothing: a claim must also come from the
// pane root of a pane in c.Session.
func (b *CapabilityBroker) Mint(c Capability) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint launch capability: %w", err)
	}
	token := hex.EncodeToString(raw)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if len(b.recs) >= capabilityMaxRecords {
		return "", errors.New("too many unclaimed launch capabilities")
	}
	rec := &capRecord{cap: c, expires: b.now().Add(capabilityTTL)}
	if c.PerPane {
		// Backs tmux's session-long default-command: new windows open at any time,
		// so it lives until the session ends (Revoke), never by the clock.
		rec.expires = time.Time{}
	}
	b.recs[token] = rec
	return token, nil
}

func (b *CapabilityBroker) expireLocked() {
	now := b.now()
	for token, rec := range b.recs {
		if !rec.expires.IsZero() && now.After(rec.expires) {
			delete(b.recs, token)
		}
	}
}

// Revoke drops every capability bound to a tmux session. The owner of a PerPane
// capability calls it when the session ends, which is what bounds a record that
// has no expiry.
func (b *CapabilityBroker) Revoke(session string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for token, rec := range b.recs {
		if rec.cap.Session == session {
			delete(b.recs, token)
		}
	}
}

// Claim hands out the capability when, and only when, token names a live record
// and peerPID is the root process of pane paneID, a pane of the record's
// session. A refusal never consumes the record, so a stranger's failed claim
// cannot burn the launcher's own.
func (b *CapabilityBroker) Claim(ctx context.Context, token, paneID string, peerPID int) (Capability, error) {
	refuse := errors.New("launch capability refused")
	if token == "" || paneID == "" || peerPID <= 0 || b.lookup == nil {
		return Capability{}, refuse
	}
	b.mu.Lock()
	b.expireLocked()
	rec, ok := b.recs[token]
	var session string
	if ok {
		session = rec.cap.Session
	}
	b.mu.Unlock()
	if !ok {
		return Capability{}, fmt.Errorf("%w: unknown or already used", refuse)
	}
	info, err := b.lookup(ctx, paneID)
	if err != nil {
		return Capability{}, fmt.Errorf("%w: cannot identify pane %s: %v", refuse, paneID, err)
	}
	if info.Session != session {
		return Capability{}, fmt.Errorf("%w: pane %s is not in the launch's session", refuse, paneID)
	}
	if info.RootPID != peerPID {
		return Capability{}, fmt.Errorf("%w: caller pid %d is not the root of pane %s", refuse, peerPID, paneID)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok = b.recs[token]
	if !ok {
		return Capability{}, fmt.Errorf("%w: unknown or already used", refuse)
	}
	if rec.cap.PerPane {
		key := paneID + ":" + strconv.Itoa(peerPID)
		if rec.panes == nil {
			rec.panes = make(map[string]bool)
		}
		if rec.panes[key] {
			return Capability{}, fmt.Errorf("%w: pane %s already claimed it", refuse, paneID)
		}
		rec.panes[key] = true
	} else {
		delete(b.recs, token)
	}
	return rec.cap, nil
}

type claimRequest struct {
	// Hello asks the server to prove who it is before the shim sends the token.
	Hello bool
	Token string
	Pane  string
}

type claimResponse struct {
	OK    bool
	Error string
	Cap   *Capability
}

// Serve answers claims on ln until it is closed.
func (b *CapabilityBroker) Serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go b.serveConn(conn)
	}
}

// serveConn reads each request through readAttestedLine, so the caller's pid is
// the sender of THAT message where the platform can attest it (Linux), not
// whoever connected.
func (b *CapabilityBroker) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(claimIOTimeout))
	if enableAttestation(conn) != nil {
		return
	}
	enc := json.NewEncoder(conn)
	for round := 0; round < 2; round++ {
		line, pid, err := readAttestedLine(conn)
		if err != nil {
			return
		}
		var req claimRequest
		if json.Unmarshal(line, &req) != nil {
			return
		}
		if req.Hello {
			_ = enc.Encode(claimResponse{OK: true})
			continue
		}
		resp := claimResponse{Error: "launch capability refused"}
		ctx, cancel := context.WithTimeout(context.Background(), claimIOTimeout)
		c, cerr := b.Claim(ctx, req.Token, req.Pane, pid)
		cancel()
		if cerr == nil {
			resp = claimResponse{OK: true, Cap: &c}
		} else {
			resp.Error = cerr.Error()
		}
		_ = enc.Encode(resp)
		return
	}
}

// ClaimCapability is the shim's side. A same-user process can unlink and
// re-bind the socket path and answer "granted" for any command, so it accepts a
// message only from a process that is this executable.
//
// Whose identity is checked matters. A connection's credentials name the process
// that created or connected the socket, and a listener can be bound by one
// process, inherited by a forked child that serves it, and the creator then
// exec'd into the real af: the creator passes any executable check while the
// child writes the answers (#6179 review, reproduced by
// TestCapability_ForkThenExecResponderIsRefused). So where the kernel attests
// the sender of each message (Linux, SCM_CREDENTIALS) the identity checked is
// that of the process that wrote THIS message, and it is checked on the hello
// before the token is sent and again on the answer.
//
// Where it cannot (macOS has LOCAL_PEERPID, which is per connection, and no
// per-message sender attestation) the check is per connection and the
// fork-then-exec bypass above remains. That is a stated residual, not a
// guarantee: see perMessageAttestation.
//
// Every failure refuses.
func ClaimCapability(socket, token string) (Capability, error) {
	conn, err := net.DialTimeout("unix", socket, claimIOTimeout)
	if err != nil {
		return Capability{}, fmt.Errorf("launch capability refused: claim endpoint unreachable: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(claimIOTimeout))
	if err := enableAttestation(conn); err != nil {
		return Capability{}, fmt.Errorf("launch capability refused: cannot attest the claim endpoint: %w", err)
	}
	enc := json.NewEncoder(conn)

	exchange := func(req claimRequest) (claimResponse, error) {
		if err := enc.Encode(req); err != nil {
			return claimResponse{}, err
		}
		line, sender, err := readAttestedLine(conn)
		if err != nil {
			return claimResponse{}, fmt.Errorf("launch capability refused: %w", err)
		}
		if err := sameExecutableAsSelf(sender); err != nil {
			return claimResponse{}, fmt.Errorf("launch capability refused: the claim endpoint is not af: %w", err)
		}
		var resp claimResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return claimResponse{}, fmt.Errorf("launch capability refused: %w", err)
		}
		return resp, nil
	}
	// The hello comes first so the token is never sent to a responder that
	// has not yet proved it is af.
	if hello, err := exchange(claimRequest{Hello: true}); err != nil {
		return Capability{}, err
	} else if !hello.OK {
		return Capability{}, errors.New("launch capability refused: the claim endpoint did not answer the hello")
	}
	resp, err := exchange(claimRequest{Token: token, Pane: os.Getenv("TMUX_PANE")})
	if err != nil {
		return Capability{}, err
	}
	if !resp.OK || resp.Cap == nil {
		return Capability{}, errors.New(resp.Error)
	}
	return *resp.Cap, nil
}

// exeID is a process's executable identity.
type exeID struct {
	path    string
	inode   uint64 // 0 when the platform cannot report it
	deleted bool   // the file was replaced or removed; path is where it lived
}

// exeIdentityOf is a seam so tests can stand in a foreign server.
var exeIdentityOf = platformExeIdentity

// sameExecutableAsSelf checks that pid runs the binary this process runs. By
// path, so that `af upgrade` or `mv` over the installed binary under a running
// daemon (the daemon's exe then reads "<path> (deleted)" with a different inode
// than the freshly installed shim) does not refuse every launch until a
// restart; and by inode as well when neither side was replaced, so a different
// file at the same path is not accepted.
func sameExecutableAsSelf(pid int) error {
	self, err := exeIdentityOf(os.Getpid())
	if err != nil {
		return fmt.Errorf("own executable unknown: %w", err)
	}
	other, err := exeIdentityOf(pid)
	if err != nil {
		return fmt.Errorf("pid %d executable unknown: %w", pid, err)
	}
	if self.path == "" || other.path != self.path {
		return fmt.Errorf("pid %d runs %q, not %q", pid, other.path, self.path)
	}
	if !self.deleted && !other.deleted && self.inode != 0 && other.inode != 0 && self.inode != other.inode {
		return fmt.Errorf("pid %d runs a different file at %q", pid, self.path)
	}
	return nil
}

const deletedSuffix = " (deleted)"

// normalizeExeLink reads a /proc/<pid>/exe target. The suffix is trusted only
// when the file really has no links: a live file literally named "af (deleted)"
// reads identically, and nlink==0 is what tells them apart.
func normalizeExeLink(target string, nlink, inode uint64) exeID {
	if strings.HasSuffix(target, deletedSuffix) && nlink == 0 {
		return exeID{path: strings.TrimSuffix(target, deletedSuffix), inode: inode, deleted: true}
	}
	return exeID{path: target, inode: inode}
}
