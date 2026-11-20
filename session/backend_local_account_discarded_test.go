package session

// Local-half tests for the #7668 discarded-env notice. Two properties
// matter here: the notice is TRUE (a name is named only when the pane's
// own Filter→ApplyAccount pipeline, fed the daemon's environment, would
// not deliver the operator's value — so a present keep-if-set proxy is
// never called "discarded"), and the notice is WIRED (it runs from
// Provision once at firstTimeSetup — removing that call must fail).

import (
	"os/exec"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// The keep-if-set half of R1 on the local backend: HTTPS_PROXY is in the
// discard set, but the local pane keeps a present ambient value — presence
// IS the operator's claim — so the notice must say nothing about it while
// still naming a pinned identity the pipeline does blank.
func TestWarnDiscardedLocalAccountEnvNames_KeepsPresentProxySilent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HTTPS_PROXY", "http://s3cr3t.corp.internal:8888")
	t.Setenv("GEMINI_API_KEY", "s3cr3t-key-do-not-log")

	i := &Instance{Title: "t", Program: "gemini", Account: "work", backend: &LocalBackend{}}
	buf := captureWarningLog(t)
	warnDiscardedLocalAccountEnvNames(i)
	out := buf.String()

	require.NotContains(t, out, "HTTPS_PROXY",
		"the pane's own pipeline delivers the operator's proxy — reporting it would be a lie")
	require.NotContains(t, out, "s3cr3t.corp.internal", "names only, never a value")
	require.Contains(t, out, "account sessions always ignore",
		"a pin the remedy cannot restore is named without the remedy")
	require.Contains(t, out, "GEMINI_API_KEY")
	require.NotContains(t, out, "s3cr3t-key-do-not-log")
}

// The ambient twin of a forwarded spelling reseats to the forwarded value
// rather than being discarded — the local pane keeps both anyway, so
// neither may be named. Forwarded names themselves are the claim channel,
// never discards.
func TestWarnDiscardedLocalAccountEnvNames_TwinAndForwardedStaySilent(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HTTPS_PROXY", "http://s3cr3t.corp.internal:8888")
	t.Setenv("https_proxy", "http://s3cr3t-low.internal:8888")
	t.Setenv("AF_SESSION_ENV_PASSTHROUGH", "HTTPS_PROXY") // explicit-forward var if read; harmless otherwise

	i := &Instance{Title: "t", Program: "gemini", Account: "work", backend: &LocalBackend{}}
	buf := captureWarningLog(t)
	warnDiscardedLocalAccountEnvNames(i)
	out := buf.String()
	require.NotContains(t, out, "HTTPS_PROXY")
	require.NotContains(t, out, "https_proxy")
	for _, v := range []string{"s3cr3t.corp.internal", "s3cr3t-low.internal"} {
		require.NotContains(t, out, v)
	}
}

// Removing the Provision call site must fail a test: drive Provision for a
// gemini account session over a real (in-place) worktree with a fake tmux
// handle, and require the notice in the warning log.
func TestProvision_WarnsDiscardedAccountEnvNamesOnFirstSetup(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "s3cr3t-key-do-not-log")
	repo := initInPlaceRepo(t, "main")

	cmdExec := cmd_test.MockCmdExec{
		RunFunc:    func(*exec.Cmd) error { return nil },
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte(""), nil },
	}
	inst := &Instance{
		Title: "t", Path: repo, Program: "gemini", Account: "work",
		inPlace: true, backend: &LocalBackend{},
	}
	inst.setTmuxLocked(tmux.NewTmuxSessionWithDeps("warn7668", "gemini", nil, cmdExec))

	buf := captureWarningLog(t)
	require.NoError(t, (&LocalBackend{}).Provision(inst, true))
	out := buf.String()
	require.Contains(t, out, "GEMINI_API_KEY",
		"a pinned identity the environment set must be named at session create")
	require.Contains(t, out, "account sessions always ignore")
	require.NotContains(t, out, "s3cr3t-key-do-not-log", "names only, never a value")

	// A non-first setup (a daemon-restart load) must not re-warn: the notice
	// is once per session.
	buf2 := captureWarningLog(t)
	require.NoError(t, (&LocalBackend{}).Provision(inst, false))
	require.NotContains(t, buf2.String(), "account sessions always ignore")
}

// An ambient (no-account) session and a non-gemini account session emit
// nothing — the notice is the gemini account boundary's (#7668).
func TestWarnDiscardedLocalAccountEnvNames_ScopeGates(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("GEMINI_API_KEY", "k")

	for _, inst := range []*Instance{
		{Title: "a", Program: "gemini", backend: &LocalBackend{}},               // ambient
		{Title: "c", Program: "claude", Account: "w", backend: &LocalBackend{}}, // claude account
		{Title: "x", Program: "codex", Account: "w", backend: &LocalBackend{}},  // codex account
	} {
		buf := captureWarningLog(t)
		warnDiscardedLocalAccountEnvNames(inst)
		require.Empty(t, buf.String(), "the notice runs for gemini account sessions only")
	}
}
