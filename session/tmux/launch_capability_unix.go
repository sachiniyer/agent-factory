//go:build !windows

package tmux

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// mintLaunchCapability admits spec for this session and returns where to claim it
// and the bearer token. The record lives in THIS process, which also serves the
// claim socket, so only the launcher can create one.
func (t *TmuxSession) mintLaunchCapability(spec launchSpec) (socket, token string, err error) {
	broker, socket, err := sessionenv.LaunchCapabilities(paneLookup, sessionAlive)
	if err != nil {
		return "", "", err
	}
	if launchSpecObserver != nil {
		launchSpecObserver(spec)
	}
	token, err = broker.Mint(sessionenv.Capability{
		Session:         t.sanitizedName,
		Agent:           spec.agent,
		Account:         spec.account,
		Command:         spec.command,
		Extras:          spec.extras,
		Proof:           spec.proof,
		EnvironmentOnly: spec.environmentOnly,
		PerPane:         spec.perPane,
	})
	if err == nil {
		t.capMu.Lock()
		t.pendingCapabilities = append(t.pendingCapabilities, pendingCapability{token: token, perPane: spec.perPane})
		t.capMu.Unlock()
	}
	return socket, token, err
}

// beginCapabilityPrepare starts a prepare's accounting. What an earlier launch
// installed stays valid until THIS one is installed: retiring it first would leave
// a live session's default-command naming a revoked capability if the install
// then fails.
func (t *TmuxSession) beginCapabilityPrepare() {
	t.capMu.Lock()
	t.pendingCapabilities = nil
	t.capMu.Unlock()
}

// revokePendingCapabilities ends exactly what this prepare minted, after a
// failure. An earlier launch's capability is left alone.
func (t *TmuxSession) revokePendingCapabilities() {
	t.capMu.Lock()
	pending := t.pendingCapabilities
	t.pendingCapabilities = nil
	t.capMu.Unlock()
	if b := sessionenv.LaunchBrokerIfStarted(); b != nil && len(pending) > 0 {
		tokens := make([]string, len(pending))
		for i, p := range pending {
			tokens[i] = p.token
		}
		b.RevokeTokens(tokens...)
	}
}

// commitCapabilities is the success half: the session now exists with this
// prepare's per-pane capability installed, so it is activated (it no longer
// expires and is subject to the lost-session sweep) and everything the session
// held before is retired. keepSingleUse is false for a refresh, which launches no
// pane, so the single-use capability it minted has no taker. Nothing happens when
// this prepare minted nothing.
func (t *TmuxSession) commitCapabilities(keepSingleUse bool) {
	t.capMu.Lock()
	pending := t.pendingCapabilities
	t.pendingCapabilities = nil
	t.capMu.Unlock()
	b := sessionenv.LaunchBrokerIfStarted()
	if b == nil || len(pending) == 0 {
		return
	}
	var keep []string
	for _, p := range pending {
		if p.perPane {
			b.Activate(p.token)
		}
		if p.perPane || keepSingleUse {
			keep = append(keep, p.token)
		}
	}
	b.RevokeSessionExcept(t.sanitizedName, keep...)
}

// revokeLaunchCapabilities ends every capability bound to a tmux session. Safe to
// call when this process never minted one.
func revokeLaunchCapabilities(session string) {
	if b := sessionenv.LaunchBrokerIfStarted(); b != nil {
		b.Revoke(session)
	}
}

// launchCapabilities reports how many capabilities session holds, and how many of
// them are per-pane.
func launchCapabilities(session string) (total, perPane int) {
	if b := sessionenv.LaunchBrokerIfStarted(); b != nil {
		return b.Capabilities(session)
	}
	return 0, 0
}

var paneIDPattern = regexp.MustCompile(`^%[0-9]+$`)

// paneLookup asks tmux which session a pane belongs to and what its root process
// is. The pane id is the claimant's own $TMUX_PANE, so it is validated before it
// becomes a tmux target: anything but %N would let a caller name a different
// target syntax.
func paneLookup(ctx context.Context, paneID string) (sessionenv.PaneInfo, error) {
	if !paneIDPattern.MatchString(paneID) {
		return sessionenv.PaneInfo{}, fmt.Errorf("malformed pane id")
	}
	out, err := outputTmuxBoundedWith(ctx, cmd.MakeExecutor(),
		"display-message", "-p", "-t", paneID, "#{session_name}\t#{pane_pid}")
	if err != nil {
		return sessionenv.PaneInfo{}, err
	}
	name, pid, ok := strings.Cut(strings.TrimRight(string(out), "\r\n"), "\t")
	root, perr := strconv.Atoi(strings.TrimSpace(pid))
	if !ok || perr != nil || name == "" {
		return sessionenv.PaneInfo{}, fmt.Errorf("tmux did not describe pane %s", paneID)
	}
	return sessionenv.PaneInfo{Session: name, RootPID: root}, nil
}

// sessionAlive is the sweep's liveness read. It asks as an EXISTING session
// (probeLiveness), not as a pre-spawn probe: when a tmp cleaner has unlinked the
// socket a live server still serves, the answer is unknown, never "gone", so the
// sweep cannot revoke the capability of a session that is still running.
func sessionAlive(ctx context.Context, session string) (alive, known bool) {
	a := probeSessionAs(ctx, cmd.MakeExecutor(), session, probeLiveness)
	return a.exists, a.known
}
