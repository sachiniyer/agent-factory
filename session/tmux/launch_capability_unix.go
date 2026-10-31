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
	t.capabilityMints.Add(1)
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
	return socket, token, err
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

func sessionAlive(ctx context.Context, session string) (alive, known bool) {
	return probeSessionContext(ctx, cmd.MakeExecutor(), session)
}
