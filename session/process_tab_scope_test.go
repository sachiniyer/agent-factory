package session

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #4506 review: process tabs meet teardown, the account-scope stop and the
// account swap after they finish, and each of those must handle a held dead
// pane. A tab af launched under the session's account must also survive a
// restart instead of being stopped as "pre-scope".

// tmuxModel answers tmux the way a real server does for the three states a
// process tab's session can be in: running, finished (a held dead pane whose pid
// is gone), and absent (tmux names the missing session and exits 1).
type tmuxModel struct {
	t        *testing.T
	mu       sync.Mutex
	alive    map[string]bool
	finished map[string]finishedPane
	// failOnStart makes new-session produce a finished pane at once, as a
	// command that exits immediately does under remain-on-exit.
	failOnStart map[string]finishedPane
	// launching names a pane whose root is the given process while it runs, and
	// which reads as its failOnStart pane once that process is gone.
	launching map[string]launchingPane
	output    map[string]string
	started   []string
	killed    []string
	missing   *exec.ExitError
}

func newTmuxModel(t *testing.T, alive ...string) *tmuxModel {
	t.Helper()
	var missing *exec.ExitError
	require.ErrorAs(t, exec.Command("sh", "-c", "exit 1").Run(), &missing)
	m := &tmuxModel{
		t: t, alive: map[string]bool{}, finished: map[string]finishedPane{},
		failOnStart: map[string]finishedPane{}, launching: map[string]launchingPane{}, output: map[string]string{},
		missing: missing,
	}
	for _, name := range alive {
		m.alive[name] = true
	}
	return m
}

func (m *tmuxModel) finish(name string, pane finishedPane, output string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive[name] = true
	m.finished[name] = pane
	m.output[name] = output
}

func (m *tmuxModel) killCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, k := range m.killed {
		if k == name {
			n++
		}
	}
	return n
}

func (m *tmuxModel) startCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.started)
}

func (m *tmuxModel) gone(name string) error {
	e := *m.missing
	e.Stderr = []byte("can't find session: " + name + "\n")
	return &e
}

// tmuxTarget names the session a tmux command addresses: its -t target, or
// new-session's -s name. list-panes also takes a bare -s flag, which is why -t
// is looked for first.
func tmuxTarget(c *exec.Cmd) string {
	for _, flag := range []string{"-t", "-s"} {
		for i, a := range c.Args {
			switch {
			case a == flag && i+1 < len(c.Args) && !strings.HasPrefix(c.Args[i+1], "-"):
				return strings.TrimSuffix(strings.TrimPrefix(c.Args[i+1], "="), ":")
			case strings.HasPrefix(a, flag+"="):
				return strings.TrimPrefix(a, flag+"=")
			}
		}
	}
	return ""
}

func (m *tmuxModel) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			_, err := m.answer(c)
			return err
		},
		OutputFunc: m.answer,
	}
}

func (m *tmuxModel) answer(c *exec.Cmd) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	joined := strings.Join(c.Args, " ")
	name := tmuxTarget(c)
	switch {
	case strings.Contains(joined, "new-session"):
		m.started = append(m.started, name)
		m.alive[name] = true
		if pane, ok := m.failOnStart[name]; ok && m.launching[name].done == nil {
			m.finished[name] = pane
		}
		return nil, nil
	case strings.Contains(joined, "list-sessions"):
		var names []string
		for n, ok := range m.alive {
			if ok {
				names = append(names, n)
			}
		}
		return []byte(strings.Join(names, "\n")), nil
	case name == "":
		return nil, nil
	case !m.alive[name]:
		if strings.Contains(joined, "display-message") {
			// tmux answers a missing target with the format's literal text and
			// exit 0 (measured: `||`).
			return []byte(paneFieldsBlank.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
		}
		return nil, m.gone(name)
	case strings.Contains(joined, "kill-session"):
		m.killed = append(m.killed, name)
		delete(m.alive, name)
		delete(m.finished, name)
		return nil, nil
	}
	if launcher := m.launching[name]; launcher.done != nil {
		select {
		case <-launcher.done:
			delete(m.launching, name)
			m.finished[name] = m.failOnStart[name]
		default:
			if strings.Contains(joined, "display-message") || strings.Contains(joined, "list-panes") {
				return []byte(paneFieldsRunning.Replace(strings.NewReplacer("#{pane_pid}", strconv.Itoa(launcher.pid)).Replace(
					c.Args[len(c.Args)-1])) + "\n"), nil
			}
			return nil, nil
		}
	}
	if pane, ok := m.finished[name]; ok {
		if answer, ok := pane.answer(c); ok {
			return []byte(answer), nil
		}
		if strings.Contains(joined, "capture-pane") {
			return []byte(m.output[name] + "\n\nPane is dead (status " + pane.status + ", Wed Sep 17 06:22:38 2026)\n"), nil
		}
		return nil, nil
	}
	if strings.Contains(joined, "display-message") {
		// A running pane: not dead, and no pid this test could be hurt by.
		return []byte(paneFieldsRunning.Replace(c.Args[len(c.Args)-1]) + "\n"), nil
	}
	return nil, nil
}

// launchingPane is a pane whose root, pid, is still af's launch shim; done
// closes once that process has exited and been collected.
type launchingPane struct {
	pid  int
	done <-chan struct{}
}

// paneFieldsBlank expands every pane field to nothing, as tmux does for a
// missing target; paneFieldsRunning is a running pane with no pid to hand out.
var (
	paneFieldsBlank = strings.NewReplacer("#{pane_pid}", "", "#{pane_dead}", "",
		"#{pane_dead_status}", "", "#{pane_dead_signal}", "", "#{pane_dead_time}", "")
	paneFieldsRunning = strings.NewReplacer("#{pane_pid}", "", "#{pane_dead}", "0",
		"#{pane_dead_status}", "", "#{pane_dead_signal}", "", "#{pane_dead_time}", "")
)

func (m *tmuxModel) session(name, program string) *tmux.TmuxSession {
	ex := m.exec()
	return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, persistPtyFactory{t: m.t, cmdExec: ex}, ex)
}

const scopeAgent = "af_4506_scope"

// scopedInstance is a started, account-scoped instance whose tabs are rebuilt
// from data exactly as a daemon load rebuilds them, then bound to the model.
func scopedInstance(t *testing.T, m *tmuxModel, account string, tabs ...TabData) *Instance {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Cleanup(tmux.SetNewSessionEnvSupportForTest(true))
	gw, err := git.NewGitWorktreeFromStorage("/tmp/4506-scope-repo", filepath.Join(t.TempDir(), "wt"), "scope",
		"scope-branch", "", false, true)
	require.NoError(t, err)
	inst := &Instance{
		ID: "scope-id", Title: "scope", Path: "/tmp/4506-scope-repo", Program: "codex", Account: account,
		backend: &LocalBackend{}, started: true, gitWorktree: gw, liveness: LiveRunning,
	}
	data := InstanceData{Title: "scope", Program: "codex", Account: account,
		Tabs: append([]TabData{{ID: "agent", Name: agentTabName, Kind: TabKindAgent, TmuxName: scopeAgent}}, tabs...)}
	restoreLocalTabs(inst, data)
	for _, tab := range inst.Tabs {
		name, program := tab.tmux.SanitizedName(), tab.tmux.Program()
		tab.tmux = m.session(name, program)
		if tab.Kind == TabKindProcess {
			tab.tmux.SetRemainOnExit()
		}
	}
	return inst
}

func processRow(id, scope string) TabData {
	return TabData{ID: id, Name: id, Kind: TabKindProcess, Command: "./" + id + ".sh",
		TmuxName: scopeAgent + tmuxTabSeparator + id, AccountScope: scope}
}

func tabByID(t *testing.T, inst *Instance, id string) *Tab {
	t.Helper()
	for _, tab := range inst.GetTabs() {
		if tab.ID == id {
			return tab
		}
	}
	t.Fatalf("no tab %q", id)
	return nil
}

func TestLoadFlagsOnlySiblingsNotLaunchedUnderTheSessionScope(t *testing.T) {
	inst := &Instance{Title: "scope"}
	restoreLocalTabs(inst, InstanceData{Title: "scope", Account: "work", Tabs: []TabData{
		{ID: "agent", Name: agentTabName, Kind: TabKindAgent, TmuxName: scopeAgent},
		processRow("scoped", "work"),
		processRow("legacy", ""),
		processRow("other", "personal"),
		{ID: "shell", Name: "shell", Kind: TabKindShell, TmuxName: scopeAgent + "__shell", AccountScope: "work"},
	}})
	flagged := map[string]bool{}
	for _, tab := range inst.Tabs[1:] {
		flagged[tab.ID] = tab.accountScopeProvenanceUnknown
	}
	assert.Equal(t, map[string]bool{"scoped": false, "legacy": true, "other": true, "shell": false}, flagged,
		"only a pane af did not record launching under the session's account is stopped at load")
	assert.Equal(t, "work", inst.ToInstanceData().Tabs[1].AccountScope, "the provenance must round-trip")
}

// The regression the review named: a watcher af started under the scope was
// stopped by every restart and never came back.
func TestScopedProcessTabSurvivesARestart(t *testing.T) {
	name := scopeAgent + "__watch"
	m := newTmuxModel(t, scopeAgent, name)
	inst := scopedInstance(t, m, "work", processRow("watch", "work"))

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Zero(t, m.killCount(name), "a pane launched under the session's account is reattached, not stopped")
	assert.Nil(t, tabByID(t, inst, "watch").Exit)
	assert.Zero(t, m.startCount())
}

func TestPreScopeProcessTabIsStoppedOnceAndSaysWhy(t *testing.T) {
	name := scopeAgent + "__watch"
	m := newTmuxModel(t, scopeAgent, name)
	inst := scopedInstance(t, m, "work", processRow("watch", ""))

	before := time.Now()
	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Equal(t, 1, m.killCount(name))
	exit := tabByID(t, inst, "watch").Exit
	require.NotNil(t, exit, "the row must say why the tab went inert")
	assert.Equal(t, TabStoppedByAccountScope, exit.StoppedBy)
	assert.False(t, exit.StatusKnown)
	assert.False(t, exit.At.Before(before))

	// The next load: the recorded reason survives, and nothing runs again.
	reloaded := scopedInstance(t, m, "work", inst.ToInstanceData().Tabs[1:]...)
	require.NoError(t, (&LocalBackend{}).setupTabs(reloaded))
	assert.Equal(t, 1, m.killCount(name), "there is nothing left to stop")
	assert.Zero(t, m.startCount(), "a process command is never re-run")
	require.NotNil(t, tabByID(t, reloaded, "watch").Exit)
	assert.Equal(t, TabStoppedByAccountScope, tabByID(t, reloaded, "watch").Exit.StoppedBy)
}

func TestAddedTabsRecordTheirLaunchScope(t *testing.T) {
	m := newTmuxModel(t, scopeAgent)
	inst := scopedInstance(t, m, "work")
	inst.Tabs[0].tmux.SetAccountForAgent("codex", "work")

	proc, err := inst.AddProcessTab("./watch.sh", "watch")
	require.NoError(t, err)
	shell, err := inst.AddShellTab()
	require.NoError(t, err)

	scopes := map[string]string{}
	for _, td := range inst.ToInstanceData().Tabs {
		scopes[td.ID] = td.AccountScope
	}
	assert.Equal(t, "work", scopes[proc.ID])
	assert.Equal(t, "work", scopes[shell.ID])
}

// swapInstance is scopedInstance fenced for an account swap.
func swapInstance(t *testing.T, m *tmuxModel, tabs ...TabData) *Instance {
	t.Helper()
	inst := scopedInstance(t, m, "work", tabs...)
	inst.mu.Lock()
	inst.inFlightOp = OpRespawning
	inst.mu.Unlock()
	return inst
}

// After a restart, a process tab whose session is gone stays inert, with its
// tmux handle still bound. The swap must not refuse over it (#4506 review).
func TestAccountSwapPassesAnInertProcessTabAfterARestart(t *testing.T) {
	m := newTmuxModel(t, scopeAgent)
	inst := swapInstance(t, m, processRow("deploy", "work"))
	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	require.NotNil(t, tabByID(t, inst, "deploy").tmux, "the inert tab keeps its binding")

	require.NoError(t, inst.StopForAccountSwap())
	assert.Equal(t, 1, m.killCount(scopeAgent), "the agent is still stopped")
	assert.Zero(t, m.startCount(), "nothing is re-run")
	assert.Nil(t, tabByID(t, inst, "deploy").Exit, "absence is not evidence of how the command ended")
}

// The narrow half of that rule: a process tab that was running until the swap
// and then vanished before its pane was observed is not inert. A detached child
// of it may still hold the old identity, so the swap still refuses.
func TestAccountSwapStillRefusesARunningProcessTabThatVanishesUnobserved(t *testing.T) {
	name := scopeAgent + "__watch"
	m := newTmuxModel(t, scopeAgent, name)
	inst := swapInstance(t, m, processRow("watch", "work"))
	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	m.mu.Lock()
	delete(m.alive, name)
	m.mu.Unlock()

	err := inst.StopForAccountSwap()
	require.ErrorIs(t, err, ErrAccountSwapAgentTeardownBlind)
	require.ErrorContains(t, err, `tab "watch"`)
}

func TestAccountSwapKeepsAFinishedProcessPane(t *testing.T) {
	name := scopeAgent + "__deploy"
	m := newTmuxModel(t, scopeAgent)
	m.finish(name, finishedPane{status: "7", at: "1726000000"}, "deployed")
	inst := swapInstance(t, m, processRow("deploy", "work"))

	require.NoError(t, inst.StopForAccountSwap())
	assert.Zero(t, m.killCount(name), "a finished pane with nothing running is kept, output and all")
	exit := tabByID(t, inst, "deploy").Exit
	require.NotNil(t, exit)
	assert.Equal(t, TabExit{Status: 7, StatusKnown: true, At: time.Unix(1726000000, 0)}, *exit)
}

func TestAccountSwapStopsARunningProcessTabAndSaysWhy(t *testing.T) {
	name := scopeAgent + "__watch"
	m := newTmuxModel(t, scopeAgent, name)
	inst := swapInstance(t, m, processRow("watch", "work"))

	require.NoError(t, inst.StopForAccountSwap())
	assert.Equal(t, 1, m.killCount(name))
	exit := tabByID(t, inst, "watch").Exit
	require.NotNil(t, exit)
	assert.Equal(t, TabStoppedByAccountSwap, exit.StoppedBy)
}

// A finished command that left a child running is not quiet: the child still
// carries the old account, so the swap stops it.
func TestAccountSwapStopsAFinishedPaneWhoseChildStillRuns(t *testing.T) {
	root, survivor := finishedRootWithSurvivor(t)
	name := scopeAgent + "__serve"
	m := newTmuxModel(t, scopeAgent)
	m.finish(name, finishedPane{pid: root, status: "0", at: "1726000000"}, "")
	inst := swapInstance(t, m, processRow("serve", "work"))

	require.NoError(t, inst.StopForAccountSwap())
	assert.Equal(t, 1, m.killCount(name))
	assert.False(t, proctree.AliveSame(survivor), "the finished command's child outlived the swap")
	exit := tabByID(t, inst, "serve").Exit
	require.NotNil(t, exit)
	assert.True(t, exit.StatusKnown, "the exit the pane reported is recorded before the stop")
	assert.Empty(t, exit.StoppedBy)
}

// finishedRootWithSurvivor runs a session leader that starts a child and exits,
// as a pane root does under tmux. It returns the reaped leader's pid and the
// child, which still carries the leader's pid as its session id.
func finishedRootWithSurvivor(t *testing.T) (int, proctree.Process) {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := c.Output()
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	var child proctree.Process
	require.Eventually(t, func() bool {
		var lookupErr error
		child, lookupErr = proctree.Lookup(pid)
		return lookupErr == nil
	}, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, c.Process.Pid, child.SID, "the child must keep the finished leader's session")
	return c.Process.Pid, child
}

func TestKillAndArchiveTearDownAFinishedProcessPane(t *testing.T) {
	for _, mode := range []struct {
		name  string
		close func(*tmux.TmuxSession) (teardownState, bool, error)
	}{
		{"kill", func(ts *tmux.TmuxSession) (teardownState, bool, error) {
			return teardownKill{}.closeTab(ts, "scope", "deploy")
		}},
		{"archive", func(ts *tmux.TmuxSession) (teardownState, bool, error) {
			return teardownArchive{}.closeTab(ts, "scope", "deploy")
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			name := scopeAgent + "__deploy"
			m := newTmuxModel(t)
			m.finish(name, finishedPane{status: "7", at: "1726000000"}, "deployed")

			state, blind, err := mode.close(m.session(name, "./deploy.sh"))
			require.NoError(t, err)
			assert.Equal(t, stateKnown, state, "a finished pane must not read as a pane that may still be live")
			assert.False(t, blind)
			assert.Equal(t, 1, m.killCount(name))
		})
	}
}

func TestAddProcessTabReportsACommandThatFailsAtOnce(t *testing.T) {
	m := newTmuxModel(t, scopeAgent)
	inst := scopedInstance(t, m, "")
	name := scopeAgent + "__deplyo"
	m.failOnStart[name] = finishedPane{status: "127", at: "1726000000"}
	m.output[name] = "sh: 1: ./deplyo.sh: not found"

	_, err := inst.AddProcessTab("./deplyo.sh", "deplyo")
	require.Error(t, err)
	assert.ErrorContains(t, err, "exited immediately with status 127")
	assert.ErrorContains(t, err, "./deplyo.sh: not found", "the command's own output says what went wrong")
	assert.NotContains(t, err.Error(), "Pane is dead")
	assert.Equal(t, 1, inst.TabCount(), "no tab is added for a command that never ran")
	assert.Equal(t, 1, m.killCount(name), "the held pane is removed")
}

// On a loaded host af's launch shim can take longer than the watch before the
// command even starts, and a refusal by the shim is an immediate failure too.
// The watch therefore starts once the pane leaves the shim.
func TestAddProcessTabWaitsForTheLaunchShimBeforeWatching(t *testing.T) {
	m := newTmuxModel(t, scopeAgent)
	inst := scopedInstance(t, m, "")
	name := scopeAgent + "__slow"
	// A stand-in for the shim's first stage: a shell whose command line starts
	// af's launch shim, alive for well over the 150ms watch.
	launcher := exec.Command("/bin/sh", "-c",
		"sleep 0.5; : "+sessionenv.AccountEnvironmentExecMarker+" claude 0 work '' 0 ./slow.sh")
	launcher.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, launcher.Start())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = launcher.Wait()
	}()
	t.Cleanup(func() {
		_ = launcher.Process.Kill()
		<-done
	})
	m.mu.Lock()
	m.launching[name] = launchingPane{pid: launcher.Process.Pid, done: done}
	m.failOnStart[name] = finishedPane{status: "127", at: "1726000000"}
	m.output[name] = "af: could not start the filtered session process"
	m.mu.Unlock()

	_, err := inst.AddProcessTab("./slow.sh", "slow")
	require.Error(t, err, "a shim that refuses after the watch window is still an immediate failure")
	assert.ErrorContains(t, err, "exited immediately with status 127")
	assert.Equal(t, 1, inst.TabCount())
}

func TestAddProcessTabRecordsACommandThatFinishedCleanly(t *testing.T) {
	m := newTmuxModel(t, scopeAgent)
	inst := scopedInstance(t, m, "")
	name := scopeAgent + "__migrate"
	m.failOnStart[name] = finishedPane{status: "0", at: "1726000000"}

	tab, err := inst.AddProcessTab("./migrate.sh", "migrate")
	require.NoError(t, err)
	require.NotNil(t, tab.Exit, "a command seen finishing is recorded as finished")
	assert.Equal(t, TabExit{Status: 0, StatusKnown: true, At: time.Unix(1726000000, 0)}, *tab.Exit)
	assert.Zero(t, m.killCount(name), "a successful one-shot keeps its output")
}
