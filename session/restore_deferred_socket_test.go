package session

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// socketGoneExec answers every has-session the way tmux does when the socket
// path is gone (exit 1, "error connecting to <socket> (No such file or
// directory)") and counts any new-session, which a respawn would issue.
func socketGoneExec(t *testing.T, newSessions *int) cmd_test.MockCmdExec {
	t.Helper()
	const diagnostic = "error connecting to /tmp/tmux-1000/default (No such file or directory)"
	failure := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", diagnostic)
	_, failErr := failure.Output()
	require.Error(t, failErr)
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			switch {
			case strings.Contains(s, "new-session"):
				*newSessions++
				return nil
			case strings.Contains(s, "has-session"):
				if c.Stderr != nil {
					_, _ = c.Stderr.Write([]byte(diagnostic + "\n"))
				}
				return failErr
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return nil, failErr },
	}
}

// #4135: a persisted row whose tmux socket is missing under a possibly-live
// server must be RETAINED as it was when its restore is refused — listed,
// started, its tmux binding and workspace untouched — never dropped, marked
// dead, or respawned beside the still-running agent. The restore is deferred.
func TestFromInstanceData_UnverifiedSocketRetainsTheRowAndDefersRestore(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

	const agentName = "af_socket_unverified_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := socketGoneExec(t, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	worktree := data.Worktree.WorktreePath

	restored, err := FromInstanceData(data)
	require.NoError(t, err, "a deferred restore must keep the row, not drop it")
	require.NotNil(t, restored)
	assert.Equal(t, Lost, restored.GetStatus(), "surfaced as Lost so the restore loop retries it later")
	assert.True(t, restored.Started(), "started, so storage keeps the record and the loop admits it")
	require.NotNil(t, restored.tmuxLocked(), "the persisted tmux binding must survive for the later restore")
	assert.Equal(t, 0, newSessions, "no second agent may be spawned beside a possibly-live one")
	_, statErr := os.Stat(worktree)
	assert.NoError(t, statErr, "the workspace must be untouched")
}

// socketGoneExceptExec is a name-keyed analogue of socketGoneExec: has-session
// answers "exists" for every name in alive (the reattach path), and answers
// tmux's socket-gone diagnostic — exit 1 with "error connecting to <socket>
// (No such file or directory)" on stderr — for every other name, which under a
// pinned live server (PinServerProbeForTest(os.Getpid())) restore reads as
// ErrSessionSocketUnverified. new-session creates the named session (so a
// respawn's readiness poll answers "exists" the way it does with nameKeyedExec)
// and is counted, so a test can assert exactly whether a refused sibling was
// respawned anyway.
func socketGoneExceptExec(t *testing.T, alive map[string]bool, newSessions *int) cmd_test.MockCmdExec {
	t.Helper()
	const diagnostic = "error connecting to /tmp/tmux-1000/default (No such file or directory)"
	failure := exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", diagnostic)
	_, failErr := failure.Output()
	require.Error(t, failErr)
	existing := make(map[string]bool, len(alive))
	for k, v := range alive {
		existing[k] = v
	}
	var mu sync.Mutex
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			s := c.String()
			n := nameOf(c)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				if c.Stderr != nil {
					_, _ = c.Stderr.Write([]byte(diagnostic + "\n"))
				}
				return failErr
			case strings.Contains(s, "new-session"):
				*newSessions++
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(cmd.String(), "list-panes") {
				return nil, nil
			}
			return []byte("content"), nil
		},
	}
}

// TestFromInstanceData_UnverifiedSocketOnSiblingShellDefersRestore is the
// non-account counterpart to TestFromInstanceData_UnverifiedSocketRetainsTheRowAndDefersRestore.
// A persisted non-account Running row whose AGENT tmux session is LIVE
// (reattached on load) while its SIBLING SHELL tab's tmux socket is unlinked
// under a possibly-live server. The shell's RestoreWithResult refuses with
// tmux.ErrSessionSocketUnverified — "restore deferred: the session is kept as
// it was". setupTabs must honor that refusal for a non-account sibling the
// same way it already does for an account-scoped one: return the error so the
// row loads Lost and the lost-restore loop retries later, instead of logging
// and continuing into the dead-shell check. The dead-shell check re-probes
// with pre-spawn semantics (ProbeSession: socket-ENOENT is plain absence),
// would classify the possibly-live shell as dead, and respawn it under the
// persisted tmux name via Start (new-session), recreating the socket path
// beside the still-running server and orphaning the live shell — and, worse,
// making the live agent's later liveness probe read determinate absent so the
// daemon respawns a second agent beside the orphaned one: precisely what
// ErrSessionSocketUnverified exists to prevent (#4135/#5765, the f6f19be5
// refusal mechanism launch's agent-tab path already honors via
// preserveAgentHandle = ... || restoreDeferred(err)). Before the fix this
// issued exactly one new-session and replaced the shell binding with a fresh
// sibling, defeating the refusal for the common non-account row.
func TestFromInstanceData_UnverifiedSocketOnSiblingShellDefersRestore(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

	const agentName = "af_sibling_socket_unverified_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := socketGoneExceptExec(t, map[string]bool{agentName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	worktree := data.Worktree.WorktreePath

	restored, err := FromInstanceData(data)
	require.NoError(t, err, "a deferred sibling restore must keep the row, not drop it")
	require.NotNil(t, restored)

	// The refusal must propagate: the row loads Lost (not Running with a
	// respawned shell) so the daemon's lost-restore loop retries later instead
	// of presenting a working terminal that orphaned the live shell.
	assert.Equal(t, Lost, restored.GetStatus(),
		"a refused sibling restore must surface as Lost so the restore loop retries, not as Running with a respawned shell")
	assert.True(t, restored.Started(),
		"started, so storage keeps the record and the lost-restore loop admits it")

	// The reattached agent must survive untouched: its tmux binding is what
	// Recover reattaches, and finishLaunchTabFailure must spare the agent pane
	// (mirroring launch's preserveAgentHandle decision).
	require.NotNil(t, restored.tmuxLocked(),
		"the reattached agent's persisted tmux binding must survive the sibling's refusal")
	assert.Equal(t, agentName, restored.tmuxLocked().SanitizedName(),
		"the agent's binding must keep its exact persisted tmux name")
	assert.True(t, restored.TabAlive(0),
		"the live agent pane must survive the sibling's refused restore")

	// The headline guarantee: a refused sibling is NEVER respawned. Before
	// the fix, the dead-shell check re-probed the shell with pre-spawn
	// semantics, classified the possibly-live shell as dead, and Start issued
	// new-session — recreating the socket path beside the still-running server
	// and orphaning the live shell. After the fix the refusal holds for every
	// kind of sibling, so zero new-session commands run.
	assert.Equal(t, 0, newSessions,
		"a refused sibling restore must not respawn the shell via new-session beside the possibly-live server")

	// The refusing sibling's binding must survive intact too — the deferral's
	// contract is "the session is kept as it was", so a later restore retry
	// reattaches or re-spawns the exact persisted tmux name. A fresh sibling
	// replacing it is exactly the respawn the refusal forbids.
	tabs := restored.GetTabs()
	require.Len(t, tabs, 2)
	require.NotNil(t, tabs[1].tmux,
		"the refusing sibling's tmux binding must survive intact for a later restore retry")
	assert.Equal(t, shellName, tabs[1].tmux.SanitizedName(),
		"the refusing sibling must keep its exact persisted tmux name — no fresh sibling replaced it")

	// The workspace must be untouched: the refusal spawns nothing, and a
	// respawn would have bound the worktree to the orphaned replacement server.
	_, statErr := os.Stat(worktree)
	assert.NoError(t, statErr, "the workspace must be untouched by a deferred restore")
}

// TestFromInstanceData_UnverifiedSocketOnSiblingShell_NonAccountRefusalStillRespawnsAccountSibling
// guards the account wording: an ACCOUNT-scoped row whose sibling's restore is
// refused (here ErrSessionSocketUnverified) must keep the account-scoped error
// message and the live agent — the account branch already returned on any
// error, so the fix must not change its message or behavior.
func TestFromInstanceData_UnverifiedSocketOnSiblingShell_AccountScopedRefusalKeepsAgentAndMessage(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.PinServerProbeForTest(os.Getpid()))

	const agentName = "af_account_socket_unverified_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName

	var newSessions int
	exec := socketGoneExceptExec(t, map[string]bool{agentName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := deadInstanceData(t, Running, agentName, shellName)
	data.Account = "work"
	// AccountScope matches the session's Account, so the sibling's scope
	// provenance is known and the pre-scope probe is skipped: the refusal
	// comes from the sibling's own RestoreWithResult.
	data.Tabs[1].AccountScope = "work"

	restored, err := FromInstanceData(data)
	require.NoError(t, err, "an account-scoped deferred restore must keep the row, not drop it")
	require.NotNil(t, restored)

	// The account-scoped refusal has always returned on any error; the fix
	// must not regress it. The row loads Lost, the agent survives, and no
	// new-session spawns beside the possibly-live server.
	assert.Equal(t, Lost, restored.GetStatus(),
		"an account-scoped refused sibling restore surfaces as Lost for the restore loop")
	assert.True(t, restored.Started())
	require.NotNil(t, restored.tmuxLocked(),
		"the reattached agent's persisted tmux binding must survive the account-scoped sibling's refusal")
	assert.True(t, restored.TabAlive(0),
		"the live agent pane must survive the account-scoped sibling's refused restore")
	assert.Equal(t, 0, newSessions,
		"an account-scoped refused sibling restore must not spawn a new-session beside the possibly-live server")
}
