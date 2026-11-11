package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7079: a project switch whose config could not be READ in time set the
// TUI's program to the global default. The new-session form then showed the
// global agent as this project's, and a chosen account pinned it onto the
// create. Unknown is not "the project expresses no preference".

func unansweredConfigRead(root string) error {
	return fmt.Errorf("read checkout marker for %s: %w", root, context.DeadlineExceeded)
}

// switchToUnreadableProject switches h to a fresh project whose config read
// goes unanswered, at switch time and on every live re-read after it.
func switchToUnreadableProject(t *testing.T, h *home) string {
	t.Helper()
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) { return daemon.SnapshotResponse{}, nil }
	prevSwitch, prevLive := resolveSwitchProjectConfig, resolveConfiguredProgramContext
	resolveSwitchProjectConfig = func(repo *config.RepoContext) (*config.ResolvedConfig, error) {
		return nil, unansweredConfigRead(repo.Root)
	}
	resolveConfiguredProgramContext = func(_ context.Context, root string) (string, error) {
		return "", unansweredConfigRead(root)
	}
	t.Cleanup(func() { resolveSwitchProjectConfig, resolveConfiguredProgramContext = prevSwitch, prevLive })
	root := initTestGitRepo(t)
	h.switchProject(&config.RepoContext{Root: root, ID: config.RepoIDFromRoot(root)})
	return root
}

func TestSwitchProjectHoldsNoDefaultWhenTheProjectConfigCannotBeRead(t *testing.T) {
	clearDaemonTarget(t)
	h := newTestHome(t)
	h.program = tmux.ProgramCodex
	require.NotEmpty(t, h.appConfig.DefaultProgram, "precondition: a global default exists to be wrongly substituted")

	root := switchToUnreadableProject(t, h)

	require.Equal(t, root, h.repoRoot)
	require.Empty(t, h.program, "an unreadable project config is not the global default")
	require.True(t, h.programUnknown)
}

// The form asks the daemon for the project's default — the answer it gives
// once it can read the project config — instead of showing the global one.
func TestNewSessionFormAsksTheDaemonForAnUnknownProjectDefault(t *testing.T) {
	clearDaemonTarget(t)
	h := newTestHome(t)
	switchToUnreadableProject(t, h)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{Default: tmux.ProgramCodex}, nil
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))

	_, cmd := h.startNewInstance()
	require.Empty(t, h.pendingProgram, "the form never shows the global agent as this project's")
	for _, msg := range drainCmd(t, cmd, time.Second) {
		_, next := h.Update(msg)
		for _, m2 := range drainCmd(t, next, time.Second) {
			_, _ = h.Update(m2)
		}
	}

	require.Equal(t, tmux.ProgramCodex, h.pendingProgram, "the daemon's answer for the project fills the field")
}

// While nobody can read the project config, a chosen account must not pin
// the global agent onto the create: the create stays implicit, and the daemon
// resolves — or refuses — the project's own default.
func TestUnknownProjectDefaultIsNeverPinnedByAChosenAccount(t *testing.T) {
	clearDaemonTarget(t)
	h := newTestHome(t)
	switchToUnreadableProject(t, h)
	t.Cleanup(SetProgramListerForTest(func(string) (daemon.ListProgramsResponse, error) {
		return daemon.ListProgramsResponse{}, nil // the daemon cannot read it either
	}))
	t.Cleanup(SetAccountListerForTest(func(string, string) (daemon.ListAccountsResponse, error) {
		return daemon.ListAccountsResponse{}, nil
	}))

	_, cmd := h.startNewInstance()
	for _, msg := range drainCmd(t, cmd, time.Second) {
		_, _ = h.Update(msg)
	}
	h.pendingAccount, h.pendingAccountChosen = "work", true

	require.Empty(t, h.sendProgramForCreate(), "no program is pinned that nobody resolved for this project")
}
