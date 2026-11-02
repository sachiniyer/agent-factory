package api

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
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

// resolveSelfSession identifies the caller's own af session the same way
// `af sessions whoami` does: match the current tmux session name against the
// stored instances. Shared by the whoami command and `sessions archive --self`
// so the two cannot drift.
//
// A sandbox credential is bound to its session (#5571 W3.2), so when the daemon
// reports a bound caller that row is the answer: the narrowed projection a
// sandbox receives withholds TmuxName, and matching on it would never find the
// caller. Otherwise — operator credential, older daemon — the tmux path is
// unchanged.
func resolveSelfSession() (*session.InstanceData, error) {
	if apiclient.HasDaemonToken() {
		if row, ok, err := callerSessionFn(); err == nil && ok {
			return &row, nil
		}
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

var selfResolveSleep = time.Sleep

// insideAfTmuxSession reports whether this command runs in a tmux session af
// spawned — the af_ prefix is af's own marker, so it names only sessions af
// made. The guards that key on it re-read the name rather than trusting a
// caller, so a missed lookup answers false.
func insideAfTmuxSession() bool {
	name, err := currentTmuxName()
	return err == nil && strings.HasPrefix(name, tmux.TmuxPrefix)
}

// callerSessionFn resolves the credential-bound session; a var so tests can
// substitute the daemon.
var callerSessionFn = apiclient.CallerNoSpawn
