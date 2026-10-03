package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestCreateRPC_WhitespaceProgramIsStoredRawThenOpaqueSelfHandoffIsRefused
// exercises the real daemon control-socket round-trip the bug report describes:
// the bare RPC create path gates Program through validateCreateProgram (the
// tokenizer trims leading/trailing whitespace, so " claude" is accepted) and
// manager_create.go:116 stores req.Program RAW with no strings.TrimSpace, so
// i.Program reaches the same-target guard untrimmed. With an opaque
// program_overrides.claude wrapper (effective ""), the guard's opaque branch
// must still refuse a self-handoff despite whitespace on the recorded enum.
//
// This closes the round-trip the session-level repro only simulates: there the
// whitespace stored value is injected directly via handoffTestInstance; here it
// flows through validateCreateProgram -> manager.CreateSession -> NewInstance
// -> i.Program and survives untrimmed, then the guard reads it.
func TestCreateRPC_WhitespaceProgramIsStoredRawThenOpaqueSelfHandoffIsRefused(t *testing.T) {
	const wrapper = "/home/dev/bin/agent-wrapper"

	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	// program_overrides.claude names a wrapper af cannot prove launches a known
	// agent, so HandoffEffectiveAgentForPath(path, "claude") returns "" and the
	// opaque branch of HandoffTargetIsCurrent is the one that decides sameness.
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: wrapper}
	require.NoError(t, config.SaveConfig(cfg))

	installOptionsRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)

	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	closeServer, err := startControlServer(manager, nil, nil, make(chan struct{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeServer() })

	// Create via the actual control-socket RPC (net/rpc over the daemon unix
	// socket), so validateCreateProgram runs at the RPC boundary. A leading
	// space on Program tokenizes to ["claude"] and passes the gate.
	const title = "ws-create"
	var resp CreateSessionResponse
	require.NoError(t, callDaemonNoEnsure("CreateSession", CreateSessionRequest{
		Title:    title,
		RepoPath: repoPath,
		Program:  " " + tmux.ProgramClaude,
	}, &resp), "validateCreateProgram must accept a leading-space program (tokenization trims)")

	inst, ok := manager.instances[daemonInstanceKey(repo.ID, title)]
	require.True(t, ok, "the created session must be registered in the manager")
	// The daemon stored req.Program RAW (manager_create.go:116): i.Program
	// carries the leading whitespace every launch path re-tokenizes away.
	require.Equal(t, " "+tmux.ProgramClaude, inst.AgentProgram(),
		"precondition: the daemon create path must persist the whitespace into i.Program untrimmed")

	// Agent conversations are cleared on a destructive self-handoff; seed one so
	// a refusal (no wipe) and a bug (wipe) are distinguishable.
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		title, wrapper, nil, nil))

	require.Empty(t, session.HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramClaude),
		"precondition: the opaque wrapper forces the opaque branch of the guard")
	require.Equal(t, tmux.ProgramClaude, inst.CurrentAgentName(),
		"precondition: the session is claude by its configured enum")

	// The guard must refuse the self-handoff despite the whitespace on the
	// recorded enum, naming the same agent it identified.
	require.ErrorContains(t, inst.ValidateHandoffTarget(tmux.ProgramClaude), "already running claude")
	_, swapErr := inst.SwapAgentProgram(tmux.ProgramClaude, session.HandoffReasonManual, "abc123def456", false)
	require.ErrorContains(t, swapErr, "already running claude",
		"the state mutation must refuse what the guard refuses")

	// No destructive side effect may fire on a refused self-handoff.
	require.Equal(t, " "+tmux.ProgramClaude, inst.AgentProgram(),
		"Program must not be rewritten to the trimmed enum on a refused self-handoff")
	require.Empty(t, inst.Tabs[0].Handoffs,
		"a refused self-handoff must not reach the ledger")

	// A different agent is still a real handoff the guard must admit — trimming
	// the recorded enum must not over-match and block a genuine cross-agent swap.
	require.NoError(t, inst.ValidateHandoffTarget(tmux.ProgramCodex),
		"a different agent stays reachable despite recorded whitespace")
}
