package app

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/apiclient"
)

// In-interface daemon restart (#2479). When a kill times out because the daemon
// took the request and went quiet (errDaemonUnresponsive), the TUI used to tell
// the user to leave the interface and run `af daemon restart`. Instead it now
// OFFERS to run the restart for them, behind a confirm — the interface performs
// the recovery rather than prescribing a shell command.
//
// The restart goes through `af daemon restart` (a subprocess), which operates on
// the daemon's PROCESS/unit, not its control socket (#2168): it shuts the old
// daemon down with a SIGTERM fallback when the socket is wedged and respawns a
// supervised one, so it recovers precisely the unresponsive daemon this path is
// reached for. The TUI reconnects on its next snapshot poll (withDaemonHTTP
// re-dials per call), so no in-process reconnection is needed.
//
// A restart is destructive-adjacent: recovering the daemon can drop the sessions
// it is running (#2176), so it is gated on a confirm. Only when the restart
// itself cannot run does a clear message stand in as the last resort.

// daemonRestartRequestedMsg is the confirm's fast, on-loop result: it raises no
// state, it just asks Update to dispatch the slow restart off the event loop —
// the same confirm→async handoff the kill/archive verbs use.
type daemonRestartRequestedMsg struct{}

// daemonRestartedMsg carries the async restart's outcome back to the event loop.
type daemonRestartedMsg struct{ err error }

// restartDaemonAction runs `af daemon restart` in a subprocess. A package var so
// the test suite can stub it — the real one restarts a real daemon, which no
// unit test may do.
var restartDaemonAction = func() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not locate the af binary: %w", err)
	}
	// --quiet suppresses the "no daemon to restart" line; a real failure still
	// exits non-zero with output, which is captured for the fallback message. The
	// subprocess inherits AGENT_FACTORY_HOME, so it targets the same daemon.
	cmd := exec.Command(exe, "daemon", "restart", "--quiet")
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("%w: %s", cerr, trimmed)
		}
		return cerr
	}
	return nil
}

// offerDaemonRestart presents the restart confirm for an unresponsive daemon. A
// remote target's daemon is on another machine, where a local restart would do
// nothing, so the caller only reaches this for a local target.
func (m *home) offerDaemonRestart(sessionTitle string) tea.Cmd {
	message := fmt.Sprintf("[!] The daemon stopped responding while killing '%s'. Restart it?", sessionTitle)
	detail := "Restarting recovers the daemon, but sessions it is running may be dropped. The kill may still finish on its own."
	return m.confirmActionWithDetail(message, detail, func() tea.Msg {
		return daemonRestartRequestedMsg{}
	})
}

// restartDaemonCmd runs the restart off the event loop (it shuts a daemon down
// and respawns one — seconds, not milliseconds).
func (m *home) restartDaemonCmd() tea.Cmd {
	restart := restartDaemonAction
	return func() tea.Msg {
		return daemonRestartedMsg{err: restart()}
	}
}

// handleDaemonRestarted reports the outcome. Success is a plain notice; failure
// is the last-resort fallback — the in-interface recovery was attempted and
// could not run, so naming the manual command here is honest rather than a
// reflexive "go type this" (#2479).
func (m *home) handleDaemonRestarted(msg daemonRestartedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.handleError(fmt.Errorf(
			"could not restart the daemon: %w — restart it from a terminal with `af daemon restart`", msg.err))
	}
	return m, m.showTransientMessage("Daemon restarted.")
}

// isRemoteTarget is a seam so a test can exercise the local restart-offer branch
// without a remote target configured. Production reads the real target.
var isRemoteTarget = apiclient.IsRemoteTarget
