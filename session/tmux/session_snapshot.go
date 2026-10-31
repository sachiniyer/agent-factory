package tmux

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
)

// A SessionSnapshot is one point-in-time read of every pane on the tmux server,
// taken with a single `list-panes -a`. Restoring a project's sessions asked tmux
// three questions per session — has-session (does it exist), display-message
// (which session generation: id, server pid, creation stamp) and list-panes (the
// pane rows PaneRootProcessMatches verifies) — so a TUI launch cost 3N tmux
// subprocesses, and the one tmux server answers them serially (#6423). One listing
// carries all three answers for every session.
//
// It is an optimization, never an authority: every consumer uses it only for a
// POSITIVE answer (the session is in the snapshot) and otherwise runs the same
// per-session command it always did, so an absent session, a wedged server and
// every error keep their existing handling.
type SessionSnapshot struct {
	taken    time.Time
	sessions map[string]*snapshotSession
}

type snapshotSession struct {
	gen  tmuxGeneration
	rows []string // one paneRowFormat reply per pane, as list-panes prints it
}

// snapshotMaxAge bounds how long a primed snapshot may answer. A restore pass takes
// well under a second; this only limits the damage of a caller that forgot to
// release.
const snapshotMaxAge = 10 * time.Second

// snapshotRowFormat prefixes paneRowFormat with the session's identity. TAB, not
// "|", separates the session fields: paneRowFormat itself uses "|", and a tab
// cannot be part of a sanitized session name, so a row that does not split into
// five parts is dropped rather than misread.
const snapshotRowFormat = "#{session_name}\t#{session_id}\t#{pid}\t#{session_created}\t" + paneRowFormat

// CaptureSessionSnapshot lists every pane on the server in one tmux call. Any
// failure (no server, a timeout) returns an error and the caller simply does not
// prime: restore then behaves exactly as it did before the snapshot existed.
func CaptureSessionSnapshot() (*SessionSnapshot, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmd.MakeExecutor(), "list-panes", "-a", "-F", snapshotRowFormat)
	if err != nil {
		return nil, fmt.Errorf("list tmux panes: %w", err)
	}
	return parseSessionSnapshot(string(out), time.Now()), nil
}

func parseSessionSnapshot(out string, taken time.Time) *SessionSnapshot {
	s := &SessionSnapshot{taken: taken, sessions: map[string]*snapshotSession{}}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 5)
		if len(parts) != 5 || parts[0] == "" || !strings.HasPrefix(parts[1], "$") || parts[2] == "" || parts[3] == "" {
			continue
		}
		sess, ok := s.sessions[parts[0]]
		if !ok {
			sess = &snapshotSession{gen: tmuxGeneration{sessionID: parts[1], serverPID: parts[2], created: parts[3]}}
			s.sessions[parts[0]] = sess
		}
		// A pane row never contains whitespace (paneRowFormat is built so), which
		// is what the per-session path's strings.Fields relied on too.
		if row := strings.TrimSpace(parts[4]); row != "" && !strings.ContainsAny(row, " \t") {
			sess.rows = append(sess.rows, row)
		}
	}
	return s
}

// primedSnapshot is the snapshot restore-time probes may answer from. Process-wide
// because FromInstanceData has no way to carry one, and scoped by
// PrimeSessionSnapshot's release.
var primedSnapshot atomic.Pointer[SessionSnapshot]

// PrimeSessionSnapshot makes s the answer source for the restore-time probes until
// the returned release runs. Only the TUI's snapshot restore primes it; the daemon
// never does, so its probes always ask tmux.
func PrimeSessionSnapshot(s *SessionSnapshot) (release func()) {
	if s == nil {
		return func() {}
	}
	primedSnapshot.Store(s)
	return func() { primedSnapshot.CompareAndSwap(s, nil) }
}

// currentSnapshot is the primed snapshot if it is still fresh, else nil. All its
// methods are nil-safe, so a consumer reads: ask the snapshot, else ask tmux.
func currentSnapshot() *SessionSnapshot {
	s := primedSnapshot.Load()
	if s == nil || time.Since(s.taken) > snapshotMaxAge {
		return nil
	}
	return s
}

// has reports whether the session had at least one pane in the snapshot.
func (s *SessionSnapshot) has(name string) bool {
	if s == nil {
		return false
	}
	sess, ok := s.sessions[name]
	return ok && len(sess.rows) > 0
}

// generation returns a private copy of the session's identity (the copy is
// mutable per generation, see tmuxGeneration.teardownInitiated).
func (s *SessionSnapshot) generation(name string) (*tmuxGeneration, bool) {
	if !s.has(name) {
		return nil, false
	}
	gen := s.sessions[name].gen
	return &gen, true
}

// paneRows returns the session's pane rows, if it is in the snapshot.
func (s *SessionSnapshot) paneRows(name string) ([]string, bool) {
	if !s.has(name) {
		return nil, false
	}
	return s.sessions[name].rows, true
}
