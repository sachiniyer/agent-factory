package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// currentTmuxName identifies the calling pane, never the server's most recent
// session. Held in a var so command tests can substitute an identity.
var currentTmuxName = func() (string, error) {
	context := os.Getenv("TMUX")
	if context == "" {
		return "", fmt.Errorf("not running inside a tmux session")
	}
	pane := os.Getenv("TMUX_PANE")
	// Only accept a pane ID; an empty or arbitrary tmux target could resolve to
	// a session unrelated to the caller.
	if len(pane) < 2 || pane[0] != '%' || strings.Trim(pane[1:], "0123456789") != "" {
		return "", fmt.Errorf("could not determine tmux session name: missing or invalid TMUX_PANE")
	}
	// TMUX is socket,pid,session-index. Split from the right because socket
	// paths may contain commas. Explicit -S also supports non-default servers.
	socket := context
	for range 2 {
		end := strings.LastIndexByte(socket, ',')
		if end < 0 {
			return "", fmt.Errorf("could not determine tmux session name: invalid TMUX socket context")
		}
		socket = socket[:end]
	}
	if !filepath.IsAbs(socket) {
		return "", fmt.Errorf("could not determine tmux session name: invalid TMUX socket path")
	}
	identity := os.Getenv(tmux.EnvMarkerSession)
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "-t", pane, "#{session_name}").Output()
	if err != nil {
		return "", fmt.Errorf("not running inside a tmux session: %w", err)
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", fmt.Errorf("could not determine tmux session name")
	}
	// The legacy launch path cannot stamp AF_SESSION or AF_SESSION_GEN, and
	// both can be inherited together (even into the session environment via
	// update-environment). A matching generation alone cannot prove stamping.
	// Ignore such markers, including those in panes created by older af builds.
	// Where stamping is supported, a conflicting identity remains an error.
	if identity != "" && tmux.SessionEnvSupported() {
		if identity != name {
			return "", fmt.Errorf("AF_SESSION %q does not match tmux pane session %q", identity, name)
		}
		return identity, nil
	}
	return name, nil
}

// errSelfNotFound marks "the session row is not there" answers from
// whoamiSession and diskWhoami. resolveSelfSession retries this miss inside an
// af_-named tmux session, where a just-spawned program can beat its own row's
// registration (#6586) — the marker is what tells that race apart from a real
// absence and from a corruption-caveated one, which retries too: the skipped
// repo may also be the write in flight.
var errSelfNotFound = errors.New("no Agent Factory session found")

// whoamiSession returns the session whose TmuxName matches tmuxName, preferring
// the daemon's live snapshot and falling back to the disk scan when no daemon is
// reachable (#1029 PR 2). When the daemon dropped a repo at startup due to a
// corrupted instances.json (#603), a no-match caveats naming the dropped repos
// rather than reporting a clean "not found", mirroring diskWhoami (#730).
func whoamiSession(tmuxName string) (*session.InstanceData, error) {
	// Lenient: the tmux name is unique and the disk record answers it on its own,
	// so a live-but-unanswering daemon may still be bypassed here.
	data, skipped, fallBack, err := snapshotReadScoped(daemon.SnapshotRequest{}, false)
	if err == nil {
		for i := range data {
			if data[i].TmuxName == tmuxName {
				return &data[i], nil
			}
		}
		// A match short-circuits above, so nothing here weakens a found session.
		// The absence claim caveats when the daemon dropped a repo at startup —
		// a corrupted file may be hiding the matching session — mirroring
		// diskWhoami's not-found-with-corruption behavior.
		if len(skipped) > 0 {
			return nil, fmt.Errorf("%w for tmux session %q; %s", errSelfNotFound, tmuxName, skippedReposSuffix(skipped))
		}
		return nil, fmt.Errorf("%w for tmux session %q", errSelfNotFound, tmuxName)
	}
	// Remote target: no local disk fallback; surface the daemon error (e.g. a 401
	// from a bad token) instead of masking it with a same-machine disk scan (#1681).
	if !fallBack {
		return nil, err
	}
	return diskWhoami(tmuxName)
}

// resolveSelfSession identifies the caller's own af session the same way
// `af sessions whoami` does: match the current tmux session name against the
// stored instances. Shared by the whoami command and `sessions archive --self`
// so the two cannot drift.
//
// A sandbox credential is bound to its session (#5571 W3.2), so when the daemon
// reports a bound caller that row is the answer: the narrowed projection a
// sandbox receives withholds TmuxName, and matching on it would never find the
// caller. The credential-bound lookup is therefore the ONLY path that can
// resolve a sandbox credential, and it is retried through the registration race
// (#6586): a just-spawned program can exec as the first action of a
// just-spawned af session before its session row is visible in the daemon
// snapshot, and the daemon reports the caller even while the row is missing
// (resp.Caller is set unconditionally for a sandbox credential, independent of
// whether the row has registered). ErrCallerRowNotRegistered distinguishes that
// race from an operator credential (Caller empty), which has no row to wait for,
// so the credential-bound lookup polls only while the race window is open and
// falls through to the tmux path otherwise — the tmux path is unreachable for a
// sandbox credential (the projection strips TmuxName) but stays for operator
// tokens and older daemons.
//
// Target decides what a failed lookup costs. On a LOCAL target no credential
// can bind — the unix socket carries no token and the daemon trusts the peer —
// so the lookup can only ever answer ok=false or fail (the daemon stopped,
// warming or absent), and either way the tmux/disk resolution below must run
// unchanged: hard-failing here would turn an exported AF_DAEMON_TOKEN into a
// raw dial error for whoami, attention --self, archive --self and the proposal
// self-fallback. On a REMOTE target a non-race failure is real (transport, a
// bad token's 401) and surfaces. A poll that saw a bound caller fails closed
// everywhere: the credential is then proven to be a sandbox credential, so the
// tmux path could only resolve a DIFFERENT session.
func resolveSelfSession() (*session.InstanceData, error) {
	if apiclient.HasDaemonToken() {
		row, ok, err := pollCredentialBoundCaller()
		switch {
		case err == nil && ok:
			return &row, nil
		case errors.Is(err, errBoundCaller):
			return nil, err
		case err != nil && apiclient.IsRemoteTarget():
			return nil, err
		}
		// Otherwise: an operator credential (no bound caller to wait for) or a
		// local-target lookup that could never bind — the tmux/disk resolution
		// is unchanged.
	}
	tmuxName, err := currentTmuxName()
	if err != nil {
		return nil, err
	}
	// A tmux session only af names is an af session: the af_ prefix is af's own
	// marker, so inside one a bare "row not found" is the spawn race — the pane
	// execs the program while the session's registration is still landing
	// (#6586). Poll for the bounded window registration needs rather than
	// reporting a misleading absence — and rather than letting the item-session
	// guardrails, which read this same result, fail open in the window. Outside
	// an af_ name no row is coming, so the miss answers at once.
	retry := strings.HasPrefix(tmuxName, tmux.TmuxPrefix)
	for attempt := 0; ; attempt++ {
		row, err := whoamiSession(tmuxName)
		if err == nil || !errors.Is(err, errSelfNotFound) || !retry || attempt == selfResolveAttempts-1 {
			return row, err
		}
		selfResolveSleep(selfResolvePoll)
	}
}

// selfResolveAttempts × selfResolvePoll bound the registration-race window:
// ~1.5s, comfortably over the measured gap (a --self command at program exec
// misses; the row is scannable well before 1.5s) and short enough that a
// genuinely absent af_-named session is not a hang. The sleep is a var so tests
// run the loop without real waiting.
const (
	selfResolveAttempts = 10
	selfResolvePoll     = 150 * time.Millisecond
)

// selfResolveDeadline bounds the credential-bound poll in WALL TIME — attempts
// bound only the sleep budget, and a single call can otherwise block for the
// whole remote request timeout (5m), turning a ~1.5s window into a ~50-minute
// hang on a wedged daemon. It is a var (not a const) so a test can shrink it
// and prove the bound fires — the same pattern remote.go's timeout vars use.
var selfResolveDeadline = 30 * time.Second

var selfResolveSleep = time.Sleep

// errBoundCaller marks every credential-bound poll failure that happens AFTER
// the daemon first reported the credential bound to a session. The binding is
// then proven — a sandbox credential's Caller is set unconditionally — so no
// outcome may fall back to a path that could resolve a DIFFERENT session (the
// tmux-name path) or report "no bound caller". errors.Is(err, errBoundCaller)
// is the "a bound caller was seen" test; the immediate cause stays reachable
// through the same wrap chain.
var errBoundCaller = errors.New("the daemon reported the credential bound to a session")

// pollCredentialBoundCaller runs the credential-bound lookup through the
// registration race (#6586) and returns its outcome:
//
//   - (row, true, nil): the bound session's row appeared in a snapshot.
//   - (zero, false, nil): the daemon reports no bound caller — an operator
//     credential or an older daemon, with no row to wait for.
//   - (zero, false, err): ErrCallerRowNotRegistered once the bounded window
//     closes with the row still absent; a "still being created" failure once
//     the window closes with the row still carrying OpCreating — a
//     still-creating row is never handed to acting verbs as if it were
//     committed (#6690); ErrCallerSessionGone once the daemon asserts the
//     bound session is no longer tracked (a row that will never land,
//     wrapped in errBoundCaller so it fails closed); any other failure
//     verbatim when no bound caller was ever seen; and — once a bound caller
//     HAS been seen — every failure wrapped in errBoundCaller so a bound
//     answer can never be silently un-resolved (bound, then "no caller", or
//     bound then a dead daemon, is an inconsistent answer to fail closed on,
//     not a tmux cue).
//
// The poll is bounded twice: selfResolveAttempts×selfResolvePoll caps the sleep
// budget, and selfResolveDeadline caps wall time including an in-flight call.
func pollCredentialBoundCaller() (session.InstanceData, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), selfResolveDeadline)
	defer cancel()
	sawBound := false
	for attempt := 0; ; attempt++ {
		row, ok, err := callerSessionFn(ctx)
		if err == nil {
			if ok {
				if row.InFlightOp != session.OpCreating {
					return row, true, nil
				}
				// The row is visible but the create has not committed — a
				// still-creating row is never handed to acting verbs as if
				// it were committed, so it gets the same bounded wait the
				// registration race gets (#6690).
				sawBound = true
			} else {
				if sawBound {
					// The credential did not change between polls, so the
					// daemon's answer did — an inconsistency that fails closed
					// rather than dropping to a lookup that can only resolve a
					// different session.
					return session.InstanceData{}, false, fmt.Errorf("%w, then reported none", errBoundCaller)
				}
				return row, ok, nil
			}
		} else if errors.Is(err, apiclient.ErrCallerSessionGone) {
			// CallerGone rides a bound answer: the credential IS bound (the
			// daemon set Caller), but its session is no longer tracked, so
			// the row will never land. Fail closed with the accurate answer —
			// never an ambiguous race, never a tmux cue (#6690).
			return session.InstanceData{}, false, fmt.Errorf("%w: %w", errBoundCaller, err)
		} else if !errors.Is(err, apiclient.ErrCallerRowNotRegistered) {
			if sawBound {
				return session.InstanceData{}, false, fmt.Errorf("%w, then the lookup failed: %w", errBoundCaller, err)
			}
			return session.InstanceData{}, false, err
		} else {
			sawBound = true
		}
		if attempt == selfResolveAttempts-1 || ctx.Err() != nil {
			if err == nil {
				// The last answer was a still-creating row: the session is
				// on its way but not committed, so say exactly that.
				return session.InstanceData{}, false, fmt.Errorf("%w: the calling session is still being created", errBoundCaller)
			}
			// A row that never registered is gone or never coming — on a
			// daemon that does not flag CallerGone the wire cannot tell a
			// still-registering row from a gone one (it reports the bound
			// caller and an empty projection either way), so say all of
			// them.
			return session.InstanceData{}, false, fmt.Errorf("%w: %w — it may still be registering, or it may have been archived, stopped, or removed", errBoundCaller, err)
		}
		selfResolveSleep(selfResolvePoll)
	}
}

// callerSessionFn resolves the credential-bound session; a var so tests can
// substitute the daemon. The context bounds the round-trip, so the retry loop
// above is bounded in wall time and not only in sleeps.
var callerSessionFn = apiclient.CallerNoSpawn
