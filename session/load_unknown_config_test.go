package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7077 review, P1-1 and P1-2: what a LOAD does when the project config cannot
// be read. A load is retried by whoever owns the row — the daemon's refresh,
// the lost-restore loop — so a read that cannot complete must never (1) tear
// down an agent the load only reattached, (2) keep a row that launches nothing
// from loading, or (3) spend a launch's retry budget inside the caller's lock.

// checkoutProbe fronts the real git with one that counts the config layer's
// checkout-identity probe — `git -C <root> rev-parse --git-common-dir`, the
// read #6748 bounds with its 1s scan — and sleeps past that budget while slow.
// Every other git call answers normally.
type checkoutProbe struct {
	repo    string
	flag    string
	counter string
}

func newCheckoutProbe(t *testing.T) *checkoutProbe {
	t.Helper()
	globalAiderHome(t)
	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	initCmd := exec.Command("git", "init", "--quiet")
	initCmd.Dir = repo
	initCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := initCmd.CombinedOutput()
	require.NoError(t, err, "git init: %s", out)
	repo, err = filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	_, err = config.RegisterProject(repo)
	require.NoError(t, err)

	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	state := t.TempDir()
	p := &checkoutProbe{repo: repo, flag: filepath.Join(state, "slow"), counter: filepath.Join(state, "probes")}
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -eq 4 ] && [ "$3" = rev-parse ] && [ "$4" = --git-common-dir ]; then
	echo probe >> %q
	if [ -e %q ]; then sleep 3; fi
fi
exec %q "$@"
`, p.counter, p.flag, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return p
}

func (p *checkoutProbe) slow(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(p.flag, nil, 0o600))
}

func (p *checkoutProbe) count() int {
	data, err := os.ReadFile(p.counter)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "probe\n")
}

// mockRestoredTmux routes every restored tmux binding through a recording mock
// in which the named sessions are alive, and returns the recorded commands.
func mockRestoredTmux(t *testing.T, alive ...string) func() []string {
	t.Helper()
	live := map[string]bool{}
	for _, name := range alive {
		live[name] = true
	}
	inner := nameKeyedExec(live)
	var mu sync.Mutex
	var commands []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			mu.Lock()
			commands = append(commands, cmd.String())
			mu.Unlock()
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), commands...)
	}
}

func loadTestRow(p *checkoutProbe, title, agentName string, status Status, worktree string, tabs ...TabData) InstanceData {
	return InstanceData{
		ID:       title + "-id",
		Title:    title,
		Path:     p.repo,
		Program:  tmux.ProgramCodex,
		Status:   status,
		TmuxName: agentName,
		Tabs:     append([]TabData{{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName}}, tabs...),
		Worktree: GitWorktreeData{
			RepoPath: p.repo, WorktreePath: worktree, SessionName: title, BranchName: "af/" + title,
		},
	}
}

// P1-1. The agent is reattached, not spawned, by this load. The project
// config answers the agent's reads, then stops answering just before the
// account-scoped shell tab's environment read. That tab read failing must not
// reach the teardown that closes the live agent — and with one config answer
// per load, it never asks again at all.
func TestLoad_ConfigUnreadableAtATabNeverTearsDownTheReattachedAgent(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := newCheckoutProbe(t)
	t.Setenv("SHELL", "/bin/sh")
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))
	registerAccount(t, tmux.ProgramCodex, "work")

	const agentName = "af_unknown_cfg_keeps_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName
	commands := mockRestoredTmux(t, agentName, shellName)
	previousProbe := probeRestoredTabSession
	// The account-scoped shell is probed after the agent reattached and before
	// its environment is read: the moment the box stops answering.
	probeRestoredTabSession = func(session *tmux.TmuxSession) (bool, bool) {
		if session.SanitizedName() == shellName {
			p.slow(t)
		}
		return session.ProbeSession()
	}
	t.Cleanup(func() { probeRestoredTabSession = previousProbe })

	row := loadTestRow(p, "unknown-cfg-keeps-agent", agentName, Running, t.TempDir(),
		TabData{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName})
	row.Account = "work"
	instance, err := FromInstanceData(row)

	require.FileExists(t, p.flag, "precondition: the config stopped answering after the agent reattached")
	require.False(t, commandIncludesSession(commands(), "kill-session", agentName),
		"a config read that could not complete must never tear down the agent this load reattached")
	require.NoError(t, err, "the load reads the project config once, so a later read cannot fail it")
	require.NotNil(t, instance)
}

// P1-1, the teardown predicate itself: whatever reaches it, an unknown project
// config is not a reason to close the agent a load reattached.
func TestLaunchTabFailureOnAnUnknownConfigSparesTheAgent(t *testing.T) {
	commands := mockRestoredTmux(t, "af_spare_agent")
	agent := restoreTmuxSession("af_spare_agent", tmux.ProgramCodex)
	unknown := &ProjectConfigUnknownError{Path: "/repo", Attempts: 1, Err: unansweredProbe("/repo")}

	err := finishLaunchTabFailure(false, agent, fmt.Errorf("prepare account-scoped tab %q: %w", shellTabName, unknown))

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.False(t, commandIncludesSession(commands(), "kill-session", "af_spare_agent"),
		"an unreadable config says nothing about the reattached agent")
}

// P1-2. A Lost row launches nothing on load — launch returns before any
// reattach or spawn — so it must load whether or not the project config can be
// read, and read none of it: the lost-restore loop's Recover reads the config
// it needs, under its own retry policy.
func TestLoad_InertRowReadsNoProjectConfig(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := newCheckoutProbe(t)
	p.slow(t)
	commands := mockRestoredTmux(t)

	instance, err := FromInstanceData(loadTestRow(p, "inert-row", "af_inert_row", Lost, t.TempDir()))

	require.NoError(t, err, "a row that launches nothing must load with its project config unreadable")
	require.NotNil(t, instance)
	require.Zero(t, p.count(), "an inert load reads no project config")
	require.Empty(t, commands(), "an inert load touches no tmux session")
}

// P1-2. A live row's load is retried by its caller, which may hold a lock while
// it loads (the daemon's refresh holds the manager lock), so a load asks the
// config once and reports unknown — a launch's three-attempt budget there is
// paid by every other caller of that lock. It reports before anything was
// reattached.
func TestLoad_LiveRowAsksAnUnreadableConfigOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := newCheckoutProbe(t)
	p.slow(t)
	commands := mockRestoredTmux(t, "af_live_row")

	_, err := FromInstanceData(loadTestRow(p, "live-row", "af_live_row", Running, t.TempDir()))

	require.Error(t, err)
	require.True(t, isProjectConfigUnknown(err), "the load reports the config as unknown: %v", err)
	require.Contains(t, err.Error(), "(1 attempt)", "a load spends one attempt; its caller owns the retry")
	require.False(t, commandIncludesSession(commands(), "attach-session", "af_live_row"),
		"the unknown is reported before the load reattaches anything")
}

// P1-2, the daemon's hand-off: a context primed OFF the caller's lock answers
// the whole load from memory, so materializing the row reads no config even
// when the box has stopped answering since.
func TestLoad_PrimedContextReadsNoProjectConfig(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	p := newCheckoutProbe(t)
	mockRestoredTmux(t, "af_primed_row")

	rc, err := PrimeRestoreContext(p.repo)
	require.NoError(t, err)
	primedProbes := p.count()
	require.Positive(t, primedProbes, "precondition: priming read the config")
	p.slow(t)

	instance, err := FromInstanceDataWithContext(loadTestRow(p, "primed-row", "af_primed_row", Running, t.TempDir()), rc)

	require.NoError(t, err, "a primed load needs no config read to succeed")
	require.NotNil(t, instance)
	require.Equal(t, primedProbes, p.count(), "the load answered every config read from the primed snapshot")
}

// P1-3's read-only launch check keeps a launch's patience and refuses an
// unknown rather than answer it.
func TestCheckProjectConfigForLaunchRefusesAnUnreadableConfig(t *testing.T) {
	noRetryPause(t)
	p := newCheckoutProbe(t)
	p.slow(t)

	resolved, err := CheckProjectConfigForLaunch(p.repo)

	require.Nil(t, resolved)
	require.True(t, IsProjectConfigUnknown(err), "%v", err)
	require.Contains(t, err.Error(), fmt.Sprintf("(%d attempts)", launchConfigResolveAttempts))
}

// Nested plan freezes restore what they replaced: an inner plan ending never
// clears an outer one that is still executing.
func TestEnvPassthroughFreezesNest(t *testing.T) {
	inst := &Instance{}
	releaseOuter := inst.freezeEnvPassthrough([]string{"OUTER"})
	releaseInner := inst.freezeEnvPassthrough([]string{"INNER"})
	names, err := SessionEnvPassthroughForInstance(inst)
	require.NoError(t, err)
	require.Equal(t, []string{"INNER"}, names)

	releaseInner()
	names, err = SessionEnvPassthroughForInstance(inst)
	require.NoError(t, err)
	require.Equal(t, []string{"OUTER"}, names, "the outer plan's freeze survives the inner one's release")
	releaseOuter()
	require.Nil(t, inst.frozenEnvPassthrough.Load())
}
