package app

import (
	"errors"
	"os/exec"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/configagent"
	"github.com/sachiniyer/agent-factory/keys"
)

// TestConfigAgentKeyDispatches is the wiring lock for the `C` hotkey.
//
// It drives the REAL dispatch path — GlobalKeyStringsMap resolves the key, then
// handleDefaultKeyPress routes it — rather than calling handleConfigAgent
// directly. That distinction is the whole point: calling the handler by hand
// would pass even with no `case keys.KeyConfigAgent` in the switch, which is
// exactly the bug this guards against. The key must actually reach the action.
func TestConfigAgentKeyDispatches(t *testing.T) {
	h := newTestHome(t)

	// The binding itself: "C" must resolve to the config-agent action.
	name, ok := keys.GlobalKeyStringsMap["C"]
	require.True(t, ok, "\"C\" is not bound to any action")
	require.Equal(t, keys.KeyConfigAgent, name, "\"C\" must be bound to the config-agent action")

	var gotMode configagent.Mode
	var gotRepo string
	spawned := 0
	t.Cleanup(SetConfigAgentSpawnerForTest(func(mode configagent.Mode, repoPath string) (string, error) {
		spawned++
		gotMode, gotRepo = mode, repoPath
		return "af-config-1", nil
	}))

	model, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, name)
	require.NotNil(t, model)
	require.NotNil(t, cmd, "pressing C must return a command that spawns the config agent; "+
		"a nil cmd means the key fell through the dispatch switch to default")

	// The spawn happens in the returned cmd, off the event loop — so nothing
	// has run yet.
	assert.Equal(t, 0, spawned, "the spawn must not run inline on the UI thread")

	msg := cmd()
	assert.Equal(t, 1, spawned, "running the command must spawn the config agent exactly once")
	assert.Equal(t, configagent.ModeChange, gotMode,
		"the hotkey is the change entry point — a keypress in a running TUI is not a first-run tour")
	// The repo is a HINT (which agent does this project prefer?), not a
	// requirement: the config agent edits the global config and runs at AF home,
	// so an empty repo is a legitimate fall back to the global default_program.
	_ = gotRepo

	spawnedMsg, isSpawnMsg := msg.(configAgentSpawnedMsg)
	require.True(t, isSpawnMsg, "the command must report back a configAgentSpawnedMsg, got %T", msg)
	assert.NoError(t, spawnedMsg.err)
}

// TestConfigAgentMissingBinaryIsNonFatal pins the never-hang, never-crash
// requirement. A missing agent binary must leave the user in the TUI with a
// transient notice — not a modal, not a panic, and not a frozen UI.
func TestConfigAgentMissingBinaryIsNonFatal(t *testing.T) {
	h := newTestHome(t)

	// What configagent.Spawn returns when preflight finds no binary: the typed
	// error wrapping preflight's actionable message.
	missing := &configagent.ProgramUnavailableError{
		Agent:   "claude",
		Command: "/nonexistent/claude",
		Err:     errors.New("Claude Code is not installed or not on PATH (resolved command: \"/nonexistent/claude\")"),
	}
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, error) { return "", missing }))

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	require.NotNil(t, cmd)

	msg := cmd()
	spawnedMsg, ok := msg.(configAgentSpawnedMsg)
	require.True(t, ok, "expected configAgentSpawnedMsg, got %T", msg)
	require.Error(t, spawnedMsg.err, "a missing binary must be reported, not swallowed")

	var pe *configagent.ProgramUnavailableError
	require.True(t, errors.As(spawnedMsg.err, &pe), "the typed error must survive to the TUI so it can be rendered")

	// Feeding the failure back through the model surfaces it and keeps running.
	model, followUp := h.handleConfigAgentSpawned(spawnedMsg)
	require.NotNil(t, model, "the TUI must survive a failed config-agent spawn")
	require.NotNil(t, followUp, "the error must be surfaced to the user, not dropped")
	assert.Equal(t, stateDefault, model.(*home).state,
		"a failed spawn must leave the user in the normal TUI state — no modal, no takeover")
}

// TestConfigAgentSpawnEntersTheTakeover pins what a successful spawn does now:
// it hands the terminal to the agent's tmux session. It no longer "does nothing
// quietly" — that was the old Instance-based seam, where the daemon's events
// plane brought a row in and there was nothing for the TUI to do.
func TestConfigAgentSpawnEntersTheTakeover(t *testing.T) {
	h := newTestHome(t)

	var attached string
	prevExec := execConfigAgentAttach
	execConfigAgentAttach = func(name string) *exec.Cmd {
		attached = name
		return exec.Command("true") // never actually attach in a test
	}
	t.Cleanup(func() { execConfigAgentAttach = prevExec })

	model, cmd := h.handleConfigAgentSpawned(configAgentSpawnedMsg{sessionName: "af-config-7"})
	require.NotNil(t, model)
	require.NotNil(t, cmd, "a successful spawn must hand the terminal to the config agent")
	assert.Equal(t, "af-config-7", attached, "the takeover must attach to the session the daemon named")
}

// TestConfigAgentSpawnWithNoSessionNameIsAnError pins the defensive case: the
// daemon reporting success but naming no session must surface, not hand the
// terminal to `tmux attach-session -t ""`.
func TestConfigAgentSpawnWithNoSessionNameIsAnError(t *testing.T) {
	h := newTestHome(t)
	model, cmd := h.handleConfigAgentSpawned(configAgentSpawnedMsg{})
	require.NotNil(t, model)
	require.NotNil(t, cmd, "a nameless session must be reported, not silently ignored")
	assert.Equal(t, stateDefault, model.(*home).state)
}

// TestConfigAgentDoneReapsTheSession pins that leaving the takeover tears the
// bare tmux session down. The daemon reaps on its own shutdown as a backstop, but
// a session per keypress that only dies with the daemon is a leak in every sense
// that matters to a long-running TUI.
func TestConfigAgentDoneReapsTheSession(t *testing.T) {
	h := newTestHome(t)
	var reaped string
	t.Cleanup(SetConfigAgentReaperForTest(func(name string) error { reaped = name; return nil }))

	h.configAgentSpawning = true
	model, _ := h.handleConfigAgentDone(configAgentDoneMsg{sessionName: "af-config-9"})
	require.NotNil(t, model)
	assert.Equal(t, "af-config-9", reaped, "leaving the takeover must reap the config agent's tmux session")
	assert.False(t, model.(*home).configAgentSpawning,
		"the re-entry guard must clear when the takeover ends, or C is dead for the rest of the session")
}

// TestConfigAgentDoneReapsEvenWhenTheAttachFailed pins that a failed attach still
// reaps. Otherwise a tmux session the user never even saw would survive until the
// daemon exits.
func TestConfigAgentDoneReapsEvenWhenTheAttachFailed(t *testing.T) {
	h := newTestHome(t)
	var reaped string
	t.Cleanup(SetConfigAgentReaperForTest(func(name string) error { reaped = name; return nil }))

	model, cmd := h.handleConfigAgentDone(configAgentDoneMsg{sessionName: "af-config-9", err: errors.New("attach blew up")})
	require.NotNil(t, model)
	assert.Equal(t, "af-config-9", reaped, "a failed attach must still reap the session")
	require.NotNil(t, cmd, "the failure must be surfaced to the user")
	assert.Equal(t, stateDefault, model.(*home).state, "a failed takeover leaves the user in a working TUI")
}

// TestConfigAgentDoesNotSpawnTwice pins the re-entry guard. The spawn is a
// daemon round trip that waits out the agent's 60s readiness budget with no UI
// feedback, so a user who thinks the key did nothing WILL press it again. Each
// press is a real agent: the daemon auto-suffixes the title, so nothing upstream
// deduplicates them. The attach path guards the identical hazard with
// attachTransitioning (#1530).
//
// In production the gate is handleDefaultKeyPress -> handleConfigAgent, so this
// drives the real dispatch rather than calling the handler directly.
func TestConfigAgentDoesNotSpawnTwice(t *testing.T) {
	h := newTestHome(t)

	spawns := 0
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, error) {
		spawns++
		return "af-config-1", nil
	}))

	pressC := func() tea.Cmd {
		_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
		return cmd
	}

	// The guard is armed synchronously inside handleConfigAgent, so the second
	// press is refused on the event loop — no goroutines or channels needed to
	// observe it. (An earlier version of this test held the first spawn open on a
	// channel to simulate a slow agent; that raced, because whichever call
	// reached the stub first blocked, and when that was the second press the test
	// deadlocked instead of failing. Deterministic beats realistic here.)
	cmd1 := pressC()
	require.NotNil(t, cmd1, "the first C must start a spawn")

	cmd2 := pressC()
	require.Nil(t, cmd2,
		"a second C while the first spawn is still in flight must be refused: the spawn waits out a "+
			"60s readiness budget with no UI feedback, so a user WILL press again, and each press would "+
			"otherwise create another agent (the daemon auto-suffixes the title, so nothing upstream dedupes)")

	// Only the accepted press does any work.
	cmd1()
	if cmd2 != nil {
		cmd2()
	}
	assert.Equal(t, 1, spawns, "exactly one config agent must be spawned for two presses")

	// After the spawn reports back, C works again — a failed or finished spawn
	// must not disable the hotkey for the rest of the session.
	h.handleConfigAgentSpawned(configAgentSpawnedMsg{})
	require.NotNil(t, pressC(), "C must be pressable again once the previous spawn settled")
}

// TestConfigAgentSpawnClearsOnlyItsOwnNotice pins the notice ownership. The spawn
// runs async for up to a minute — plenty of time for another action to post its
// own notice — so clearing unconditionally on report-back would wipe a message
// the user had not read yet. Only retract our own, and only if it is still up.
func TestConfigAgentSpawnClearsOnlyItsOwnNotice(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, error) { return "af-config-1", nil }))

	_, cmd := h.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	require.NotNil(t, cmd)
	msg := cmd().(configAgentSpawnedMsg)

	// Someone else posts a notice while the spawn was in flight.
	h.setTransientNotice(errors.New("something else the user needs to read"))

	h.handleConfigAgentSpawned(msg)
	assert.Contains(t, h.errBox.FullError(), "something else the user needs to read",
		"the spawn must not clear a notice raised by another action after it started — "+
			"it may be something the user has not read")

	// The normal case still retracts our own notice.
	h2 := newTestHome(t)
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) (string, error) { return "af-config-1", nil }))
	_, cmd2 := h2.handleDefaultKeyPress(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'C'}}, keys.KeyConfigAgent)
	own := cmd2().(configAgentSpawnedMsg)
	require.Contains(t, h2.errBox.FullError(), "Starting the config agent",
		"the keypress should have raised a starting notice")
	h2.handleConfigAgentSpawned(own)
	assert.Empty(t, h2.errBox.FullError(), "a successful spawn should retract its own starting notice")
}
