package app

import (
	"errors"

	tea "github.com/charmbracelet/bubbletea"

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
// The signature is deliberately narrow — a mode and a repo path in, an error out
// — so it stays valid across the rework: the caller does not care whether the
// agent is an Instance, a pane, or a bare tmux session.
var spawnConfigAgent = func(mode configagent.Mode, repoPath string) error {
	_, err := configagent.Spawn(configagent.Options{Mode: mode, RepoPath: repoPath})
	return err
}

// SetConfigAgentSpawnerForTest swaps the spawn seam and returns a restore func,
// matching SetLocalSessionPreflightForTest and the other app test seams.
func SetConfigAgentSpawnerForTest(f func(configagent.Mode, string) error) func() {
	prev := spawnConfigAgent
	spawnConfigAgent = f
	return func() { spawnConfigAgent = prev }
}

// configAgentSpawnedMsg reports the outcome of an async config-agent spawn.
// err nil means the session is up; the daemon's own events plane brings the row
// in, so there is nothing to do on success.
type configAgentSpawnedMsg struct {
	err error
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
	// Target the ACTIVE project's repo root, not the process cwd: after an
	// in-place project switch (#1461) the active repo is m.repoRoot, which may
	// no longer be where af was launched. Mirrors the session-create path.
	repoPath := m.repoRoot
	if repoPath == "" {
		repoPath = "."
	}
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
		return configAgentSpawnedMsg{err: spawn(configagent.ModeChange, repoPath), noticeID: noticeID}
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
	// Success: retract the "Starting…" notice — but ONLY if it is still ours. The
	// spawn ran async for up to a minute, so another action may have posted its
	// own notice in the meantime, and clearing unconditionally would wipe a
	// message the user had not read. The generation token is the same mechanism
	// hideErrMsg uses (home_update.go).
	if msg.noticeID == m.transientNoticeID {
		m.errBox.Clear()
	}
	return m, nil
}
