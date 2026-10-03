package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestCreateRPC_WhitespaceProgramIsNormalizedAtCreateThenOpaqueSelfHandoffIsRefused
// exercises the real daemon control-socket round-trip the bug report describes:
// the bare RPC create path gates Program through validateCreateProgram (the
// tokenizer trims leading/trailing whitespace, so " claude" is accepted). The
// fix trims req.Program at the RPC create boundary (controlServer.createSession,
// not Manager.CreateSession — the root-agent ensure loop calls the latter
// directly with command strings that must not be trimmed) so the stored enum
// cannot carry surrounding whitespace: ResolveProgram's program_overrides lookup
// is an EXACT map key, so an untrimmed " claude" would miss the "claude"
// override and launch the bare command instead of the wrapper, and the opaque
// same-target guard would then refuse a genuine cross-command transition.
// Normalizing at the boundary is the single point that makes every reader — the
// override resolution, the recorded enum the guard compares, and the
// title-reservation helpers — agree on the trimmed identity.
//
// With an opaque program_overrides.claude wrapper (effective ""), the guard's
// opaque branch must refuse a self-handoff. This closes the round-trip the
// session-level repro only simulates: there the whitespace value is injected
// directly via handoffTestInstance; here it flows through validateCreateProgram
// -> controlServer.createSession (which trims) -> Manager.CreateSession -> NewInstance
// -> i.Program.
func TestCreateRPC_WhitespaceProgramIsNormalizedAtCreateThenOpaqueSelfHandoffIsRefused(t *testing.T) {
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
	// The create path normalizes the program enum: i.Program is the trimmed
	// "claude", not " claude". This is the fix — ResolveProgram's override
	// lookup is an exact map key, so an untrimmed value would miss the
	// "claude" override and launch the bare command instead of the wrapper.
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
		"the daemon create path must trim the program enum into i.Program")

	// Agent conversations are cleared on a destructive self-handoff; seed one so
	// a refusal (no wipe) and a bug (wipe) are distinguishable.
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(
		title, wrapper, nil, nil))

	require.Empty(t, session.HandoffEffectiveAgentForPath(inst.Path, tmux.ProgramClaude),
		"precondition: the opaque wrapper forces the opaque branch of the guard")
	require.Equal(t, tmux.ProgramClaude, inst.CurrentAgentName(),
		"precondition: the session is claude by its configured enum")

	// The guard must refuse the self-handoff: the recorded enum is the trimmed
	// claude and the target is claude, so the opaque branch matches.
	require.ErrorContains(t, inst.ValidateHandoffTarget(tmux.ProgramClaude), "already running claude")
	_, swapErr := inst.SwapAgentProgram(tmux.ProgramClaude, session.HandoffReasonManual, "abc123def456", false)
	require.ErrorContains(t, swapErr, "already running claude",
		"the state mutation must refuse what the guard refuses")

	// No destructive side effect may fire on a refused self-handoff.
	require.Equal(t, tmux.ProgramClaude, inst.AgentProgram(),
		"Program must not be rewritten on a refused self-handoff")
	require.Empty(t, inst.Tabs[0].Handoffs,
		"a refused self-handoff must not reach the ledger")

	// A different agent is still a real handoff the guard must admit — the
	// trimmed recorded enum must not over-match and block a genuine swap.
	require.NoError(t, inst.ValidateHandoffTarget(tmux.ProgramCodex),
		"a different agent stays reachable")
}
