package app

import (
	"errors"
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
	t.Cleanup(SetConfigAgentSpawnerForTest(func(mode configagent.Mode, repoPath string) error {
		spawned++
		gotMode, gotRepo = mode, repoPath
		return nil
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
	assert.NotEmpty(t, gotRepo, "the spawn must target a repo path")

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
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) error { return missing }))

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

// TestConfigAgentSpawnSuccessIsQuiet pins that a successful spawn raises no
// error notice: the daemon's events plane brings the session in on its own, so
// there is nothing for this handler to announce.
func TestConfigAgentSpawnSuccessIsQuiet(t *testing.T) {
	h := newTestHome(t)
	model, cmd := h.handleConfigAgentSpawned(configAgentSpawnedMsg{})
	require.NotNil(t, model)
	assert.Nil(t, cmd, "a successful spawn should not raise a notice")
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
	t.Cleanup(SetConfigAgentSpawnerForTest(func(configagent.Mode, string) error {
		spawns++
		return nil
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
