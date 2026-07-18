package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/log"
)

// The `C` hotkey's app-side wiring (phase 3).
//
// This file is the ONLY place in app/ that names the configagent package, and
// spawnConfigAgent below is the only call into it. That is deliberate: the spawn
// seam is being reworked (the config agent must become a pane rather than an
// Instance), so when it changes, this one function body moves and nothing else
// in app/ notices. handle_actions.go holds a single `case` that calls
// handleConfigAgent — no configagent types, options, or modes leak into the
// dispatch switch.
//
// It also lives in its own file rather than in handle_actions.go because that
// file is at 990 lines against the 1000-line production limit
// (scripts/lint-file-length.sh); a handler added there would trip the lint.

// spawnConfigAgent starts the config agent and reports only whether it failed.
// A package var so tests can drive the hotkey without spawning anything, and so
// the seam rework has exactly one call site to move.
//
// The signature is deliberately narrow — a mode and a repo path in, the tmux
// session name plus its socket path and an error out — so it stays valid across
// the rework: the caller does not care whether the agent is an Instance, a pane,
// or a bare tmux session. The socket path is what lets the attach pin `-S` so it
// resolves the session independently of the TUI's TMUX_TMPDIR (#2019).
var spawnConfigAgent = func(mode configagent.Mode, repoPath string) (string, string, error) {
	return configagent.Spawn(configagent.Options{Mode: mode, RepoPath: repoPath})
}

// reapConfigAgent tears the config-agent tmux session down once the user leaves
// the takeover. Best-effort: the daemon reaps every config agent on its own
// shutdown regardless, so a missed call cannot outlive the daemon.
var reapConfigAgent = configagent.Reap

// SetConfigAgentSpawnerForTest swaps the spawn seam and returns a restore func,
// matching SetLocalSessionPreflightForTest and the other app test seams.
func SetConfigAgentSpawnerForTest(f func(configagent.Mode, string) (string, string, error)) func() {
	prev := spawnConfigAgent
	spawnConfigAgent = f
	return func() { spawnConfigAgent = prev }
}

// SetConfigAgentReaperForTest swaps the reap seam and returns a restore func, so
// a test can assert the takeover tears its session down without a daemon.
func SetConfigAgentReaperForTest(f func(string) error) func() {
	prev := reapConfigAgent
	reapConfigAgent = f
	return func() { reapConfigAgent = prev }
}

// configAgentSpawnedMsg reports the outcome of an async config-agent spawn.
// err nil means the session is up; the daemon's own events plane brings the row
// in, so there is nothing to do on success.
type configAgentSpawnedMsg struct {
	err error
	// sessionName is the bare tmux session the daemon started, empty on failure.
	// The takeover attaches to it and the reap tears it down.
	sessionName string
	// socketPath is the absolute tmux server socket the session lives on, so the
	// attach can pin it with `tmux -S <path>` and resolve the session
	// independently of the TUI's TMUX_TMPDIR (#2019). Empty when the daemon could
	// not resolve it; the attach then falls back to the default socket.
	socketPath string
	// noticeID identifies the "Starting…" notice this spawn raised, so the
	// handler can retract ITS OWN notice and not whatever is on screen by then.
	// The spawn runs async for up to a minute — ample time for another action to
	// post its own notice — and the codebase already scopes this with a
	// generation token (see hideErrMsg in home_update.go).
	noticeID uint64
}

// handleConfigAgent is the `C` action: open the config agent.
//
// It returns immediately with a tea.Cmd and does the spawn off the event loop,
// mirroring resumeFromLimitCmd. This is not a style preference — Spawn does a
// daemon round trip that starts a program and waits for it to become ready, and
// running that inline would freeze the TUI for the whole readiness poll.
//
// ModeChange, not ModeOnboard: pressing a key in a running TUI is a deliberate
// "I want to change something" gesture, so the agent opens by asking what to
// change rather than starting a first-run tour. Onboarding is phase 4's trigger.
func (m *home) handleConfigAgent() (tea.Model, tea.Cmd) {
	// Re-entry guard. The spawn below is a daemon round trip that waits out the
	// agent's readiness budget (60s in task.WaitForReady), and the TUI renders
	// nothing while it runs — so a user who sees no response and presses C again
	// would get a SECOND config agent, and a third. The daemon auto-suffixes the
	// title, so nothing upstream stops it: each press is a real agent. The attach
	// path guards the identical hazard with attachTransitioning (#1530).
	if m.configAgentSpawning {
		return m, nil
	}
	// The ACTIVE project's repo root, not the process cwd: after an in-place
	// project switch (#1461) the active repo is m.repoRoot, which may no longer be
	// where af was launched.
	//
	// This is only a HINT about which agent to launch — an in-repo default_program
	// wins here, as it does for `af sessions create`. It is NOT the agent's working
	// directory: the config agent runs at AF home. So an empty repoRoot is fine and
	// falls back to the global config rather than needing a cwd.
	repoPath := m.repoRoot
	m.configAgentSpawning = true
	// Acknowledge the keypress. Without this the TUI is silent for as long as the
	// agent takes to become ready, which reads as a dead key — and a dead-feeling
	// key plus the guard above would mean C appears to do nothing at all.
	//
	// Set synchronously with NO auto-clear, rather than through
	// showTransientMessage: that clears after 3 seconds, which would expire most
	// of the way through a 60s readiness wait and put the user back in silence.
	// This notice stands until the spawn reports back and clears it.
	noticeID := m.setTransientNotice(errors.New("Starting the config agent…"))
	spawn := spawnConfigAgent
	return m, func() tea.Msg {
		name, socketPath, err := spawn(configagent.ModeChange, repoPath)
		return configAgentSpawnedMsg{err: err, sessionName: name, socketPath: socketPath, noticeID: noticeID}
	}
}

// handleConfigAgentSpawned finalizes the async spawn. A failure — most likely a
// missing agent binary, which Spawn reports as a typed ProgramUnavailableError
// carrying preflight's actionable message — is surfaced as the same transient,
// self-clearing notice every other non-fatal action uses. The user stays in the
// TUI with their config untouched; nothing is modal and nothing blocks.
func (m *home) handleConfigAgentSpawned(msg configAgentSpawnedMsg) (tea.Model, tea.Cmd) {
	// Clear the re-entry guard FIRST, and unconditionally: a failed spawn must
	// leave C pressable again, or one missing binary would disable the hotkey for
	// the rest of the session.
	m.configAgentSpawning = false
	if msg.err != nil {
		log.ErrorLog.Printf("could not start the config agent: %v", msg.err)
		// handleError replaces the "Starting…" notice with the failure, so the
		// user sees why rather than a notice that just vanishes.
		return m, m.handleError(msg.err)
	}
	// Retract the "Starting…" notice — but ONLY if it is still ours. The spawn ran
	// async for up to a minute, so another action may have posted its own notice
	// in the meantime, and clearing unconditionally would wipe a message the user
	// had not read. The generation token is the same mechanism hideErrMsg uses
	// (home_update.go).
	if msg.noticeID == m.transientNoticeID {
		m.errBox.Clear()
	}
	if msg.sessionName == "" {
		// The daemon reported success but named no session. Nothing to attach to,
		// and reaping an empty name is a no-op — surface it rather than hand the
		// terminal to a `tmux attach-session -t ""`.
		return m, m.handleError(errors.New("the config agent started but reported no session to attach to"))
	}
	return m, m.enterConfigAgent(msg.sessionName, msg.socketPath)
}

// configAgentDoneMsg reports that the user has left the config-agent takeover.
type configAgentDoneMsg struct {
	sessionName string
	err         error
}

// enterConfigAgent hands the terminal to the config agent's tmux session and
// takes it back when the user detaches or the agent exits.
//
// tea.ExecProcess is bubbletea's own primitive for exactly this — "spawning other
// interactive applications such as editors and shells from within a Program". It
// pauses the Program, releases the terminal, runs the child, and resumes. That is
// why this path needs neither the WS PTY stream nor remoteDetachTerminalReassert:
// the raw-proxy attach has to hand-restore bubbletea's modes because a clientless
// byte proxy scribbles over them (#845), whereas ExecProcess suspends and restores
// the Program around the child by construction.
//
// Attaching to a tmux session is also why this can work at all without an
// Instance: `tmux attach-session` needs only a session NAME, while the WS route
// needs an Instance to resolve a byte source — and an Instance is a row.
func (m *home) enterConfigAgent(sessionName, socketPath string) tea.Cmd {
	cmd := execConfigAgentAttach(sessionName, socketPath)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return configAgentDoneMsg{sessionName: sessionName, err: err}
	})
}

// execConfigAgentAttach builds the attach command. A var so tests can drive the
// takeover without a tmux server.
//
// Two things make this attach survive af running INSIDE a tmux session (#2019):
//
//   - $TMUX is scrubbed from the child's env. When af itself was launched from a
//     tmux pane, af inherits $TMUX, and `tmux attach-session` then refuses to
//     nest — "sessions should be nested with care, unset $TMUX to force" — and
//     exits 1. This is the ONLY terminal handover in the TUI that shells out to
//     `tmux attach-session` (every other attach goes through the WS raw proxy,
//     which needs an Instance — a session-list row — that the config agent
//     deliberately is not), so it is the only one that hits the nesting refusal.
//     Dropping $TMUX is exactly what tmux's own error instructs.
//
//   - The server socket is pinned with `-S <path>`. After $TMUX is gone, tmux
//     resolves the session through the DEFAULT socket
//     (${TMUX_TMPDIR:-/tmp/tmux-<uid>}/default), and the daemon that spawned the
//     session can resolve a different TMUX_TMPDIR than this TUI — so "default"
//     could point at two different directories and the attach would not find the
//     session. Pinning the authoritative socket the daemon reported makes
//     resolution independent of either side's TMUX_TMPDIR. socketPath is empty
//     only when the daemon could not resolve it, in which case the attach falls
//     back to the default socket (Part 1 alone still fixes the reported bug).
var execConfigAgentAttach = func(sessionName, socketPath string) *exec.Cmd {
	args := make([]string, 0, 5)
	if socketPath != "" {
		args = append(args, "-S", socketPath)
	}
	args = append(args, "attach-session", "-t", sessionName)
	cmd := exec.Command("tmux", args...)
	cmd.Env = configAgentAttachEnv()
	return cmd
}

// configAgentAttachEnv is the current environment with the TMUX marker removed,
// so the nested `tmux attach-session` above does not refuse to run (#2019).
//
// Only TMUX is dropped, and deliberately: TMUX_TMPDIR is left in place because it
// participates in default-socket resolution (and the `-S` pin depends on nothing
// here). This mirrors daemon/vscode_server.go's vscodeChildEnv, which likewise
// builds a child env from os.Environ() dropping selected KEY= prefixes because a
// stale inherited handle would break the child — the same shape, one key.
//
// The `TMUX=` prefix match is exact: it drops `TMUX=…` without touching
// `TMUX_TMPDIR=…`, whose key does not start with `TMUX=`.
func configAgentAttachEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// handleConfigAgentDone runs when the user leaves the takeover: reap the session
// and reload the config so the TUI reflects whatever the agent set.
func (m *home) handleConfigAgentDone(msg configAgentDoneMsg) (tea.Model, tea.Cmd) {
	m.configAgentSpawning = false
	if msg.err != nil {
		// A failed attach is not fatal — the user is back in the TUI either way —
		// but the session must still be reaped, so this does not return early.
		log.ErrorLog.Printf("config agent attach ended with an error: %v", msg.err)
	}
	if rerr := reapConfigAgent(msg.sessionName); rerr != nil {
		// Best-effort: the daemon reaps every config agent on its own shutdown, so
		// a failure here cannot outlive the daemon. Not worth a user-facing error.
		log.WarningLog.Printf("config agent: reaping %s failed: %v", msg.sessionName, rerr)
	}

	// Re-read the config so what the agent set is what the TUI now believes.
	//
	// This is deliberately partial, and saying so is the honest thing: `af config
	// set` writes config.toml, which af and the daemon read at STARTUP. There is
	// no reload signal and config.toml is deliberately outside the daemon's
	// single-writer regime, so most keys genuinely do not take effect until a
	// restart — which is why `af config set` prints that note itself, and why the
	// briefing tells the agent not to repeat it. Reloading here keeps the TUI's
	// own view (m.appConfig) honest rather than pretending a restart happened.
	if cfg, err := config.LoadConfig(); err != nil {
		log.WarningLog.Printf("config agent: could not reload the config after the walkthrough: %v", err)
	} else {
		m.appConfig = cfg
	}
	if msg.err != nil {
		// Name what happened and what to do, not a bare exit code. A raw
		// "config agent: exit status 1" (what tmux returns when it refuses to
		// nest, #2019, and what any other attach failure surfaces as) tells the
		// user neither the cause nor a way forward. Lead with the consequential
		// half — the transient notice clips its tail at real widths, and the
		// wrapped cause stays available under `E details`.
		return m, m.handleError(fmt.Errorf(
			"could not attach to the config agent — it may have exited, or tmux is unavailable · press C to retry, or run af config set to edit config directly: %w",
			msg.err))
	}
	return m, nil
}
