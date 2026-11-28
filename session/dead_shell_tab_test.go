package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7944: a shell tab whose $SHELL exited used to read as live on every surface
// — its tmux session vanished with it, so nothing observed the death and
// `exit` stayed null. The fix stamps the exit on positive evidence only: a
// liveness probe whose answer is "server answered AND the session is gone"
// (#4135's proven absence — a shell is never remain-on-exit, so there is no
// pane to read a status from). Every non-answer — timeout, unlinked socket
// under a possibly-live server, EACCES — is unknown, never dead, because the
// guard built on the stamp must not steer a user into closing a shell that
// might be live. The probe pass is async off the snapshot path
// (KickShellExitProbe) so projection latency never depends on tmux; tests run
// its synchronous body.

// shellTabFixture builds a started local instance whose agent tab and one
// shell tab share a mock-backed executor and a real-file PTY factory: sessions
// named in alive report existing, and every executor call lands in commands.
func shellTabFixture(t *testing.T, alive map[string]bool) (*Instance, *[]string) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	var commands []string
	cmdExec := nameKeyedExecWithFinishedPanes(alive, nil)
	baseRun := cmdExec.RunFunc
	cmdExec.RunFunc = func(c *exec.Cmd) error {
		commands = append(commands, c.String())
		return baseRun(c)
	}
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	repoPath := "/tmp/dead-shell-repo"
	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, existingDir(t, filepath.Join(t.TempDir(), "wt")), "dead-shell",
		"dead-shell-branch", "", false, true)
	require.NoError(t, err)

	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	inst := &Instance{
		Title:       "dead-shell",
		Path:        repoPath,
		Program:     "bash",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs: []*Tab{
			newAgentTab(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "bash", pty, cmdExec)),
			{ID: "shell-id", Name: "terminal", Kind: TabKindShell,
				tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(shellName, defaultShell(), pty, cmdExec)},
		},
	}
	return inst, &commands
}

// livenessAnswers stubs the proven-absence probe (probeTabLiveness is the
// #4135 liveness probe) so the stamp matrix — exists/known pairs — is
// exercised without tmux.
func livenessAnswers(t *testing.T, answers map[string][2]bool) {
	t.Helper()
	prev := probeTabLiveness
	t.Cleanup(func() { probeTabLiveness = prev })
	probeTabLiveness = func(ts *tmux.TmuxSession) (bool, bool) {
		a, ok := answers[ts.SanitizedName()]
		if !ok {
			return true, true // unstubbed sessions read live-and-known
		}
		return a[0], a[1]
	}
}

// The roster refresh stamps a shell whose session is provably gone — for the
// local probe pass and the remote roster read alike, through the same
// evidence rule.
func TestShellExitStampedOnProvenAbsence(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	for _, run := range []struct {
		name string
		call func(*Instance)
	}{
		{"async pass", func(i *Instance) { i.probeShellExits() }},
		{"roster refresh", func(i *Instance) { i.RecordTabExits() }},
	} {
		t.Run(run.name, func(t *testing.T) {
			livenessAnswers(t, map[string][2]bool{shellName: {false, true}})
			inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true})

			run.call(inst)

			exit := inst.TabsData()[1].Exit
			require.NotNil(t, exit, "a provably-gone shell carries a stamped exit on the roster")
			assert.False(t, exit.StatusKnown, "a vanished session has no status to report")
			assert.False(t, exit.At.IsZero(), "the stamp records when the death was observed")
		})
	}
}

// Every non-answer is unknown, never dead — the R2 matrix that keeps a
// maybe-live shell from reading dead and the guard from steering a `w` at it.
func TestShellExitSkipsUnprovenAnswers(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	for name, answer := range map[string][2]bool{
		"live and known":         {true, true},
		"wedged / timed out":     {false, false},
		"socket unverified":      {false, false},
		"answered but ambiguous": {true, false},
	} {
		t.Run(name, func(t *testing.T) {
			livenessAnswers(t, map[string][2]bool{shellName: answer})
			inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true})

			inst.probeShellExits()
			inst.RecordTabExits()

			assert.Nil(t, inst.TabsData()[1].Exit,
				"no stamp without a server-answered absence — %s", name)
		})
	}
}

// The async kick is once-in-flight: snapshot reads may call it every poll
// without ever waiting on tmux (R4) or stacking probes.
func TestKickShellExitProbeOnceInFlight(t *testing.T) {
	livenessAnswers(t, nil)
	inst, _ := shellTabFixture(t, map[string]bool{"af_dead_shell": true, "af_dead_shell__terminal": true})
	inst.shellExitProbeRunning.Store(true) // simulate an in-flight pass
	inst.KickShellExitProbe()              // must return without starting another
	assert.True(t, inst.shellExitProbeRunning.Load())
	inst.shellExitProbeRunning.Store(false)
	inst.KickShellExitProbe()
	assert.Eventually(t, func() bool { return !inst.shellExitProbeRunning.Load() },
		5*time.Second, 5*time.Millisecond, "the kicked pass completes and clears the flag")
}

// A shell whose exit was stamped before its session was lost still respawns
// on restore exactly as on master (#386/#991): the replacement launches, and
// the OLD incarnation's stamp is cleared — kept, it would read the live shell
// dead forever and the guard would refuse a healthy PTY.
func TestSetupTabsRespawnsAShellWhoseSessionIsGone(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"
	inst, commands := shellTabFixture(t, map[string]bool{agentName: true})
	inst.Tabs[1].Exit = &TabExit{At: time.Unix(1726000000, 0)}

	require.NoError(t, (&LocalBackend{}).setupTabs(inst))
	assert.Nil(t, inst.Tabs[1].Exit, "a respawned shell drops the exit that described its dead predecessor")

	spawns := 0
	for _, c := range *commands {
		if strings.Contains(c, "new-session") && strings.Contains(c, shellName) {
			spawns++
		}
	}
	assert.Equal(t, 1, spawns, "a shell whose session is definitively gone is replaced, as on master")
}

// The daemon's roster is authoritative end to end (R1): ReconcileTabsFromData
// mirrors the stamped exit onto the local row AND mirrors a cleared one back
// off — a running TUI cannot keep the old incarnation's death against a live
// replacement.
func TestReconcileMirrorsShellExitBothDirections(t *testing.T) {
	inst, _ := shellTabFixture(t, map[string]bool{"af_dead_shell": true, "af_dead_shell__terminal": true})

	_, err := inst.ReconcileTabsFromData([]TabData{
		{ID: "agent-id", Kind: TabKindAgent},
		{ID: "shell-id", Name: "terminal", Kind: TabKindShell,
			Exit: &TabExitData{At: time.Unix(1726000000, 0)}},
	})
	require.NoError(t, err)
	require.NotNil(t, inst.Tabs[1].Exit, "the daemon's stamped exit lands on the local row")

	_, err = inst.ReconcileTabsFromData([]TabData{
		{ID: "agent-id", Kind: TabKindAgent},
		{ID: "shell-id", Name: "terminal", Kind: TabKindShell},
	})
	require.NoError(t, err)
	assert.Nil(t, inst.Tabs[1].Exit, "the daemon's cleared exit clears the local row too")
}

// TabDead is the interactive-entry read (#7944): true on the stamped exit and
// nothing else — no probes, so it cannot block the Bubble Tea loop, and no
// unproven absence, so it cannot steer a `w` at a maybe-live shell.
func TestTabDead(t *testing.T) {
	const agentName = "af_dead_shell"
	shellName := agentName + tmuxTabSeparator + "terminal"

	t.Run("stamped exit", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true})
		inst.Tabs[1].Exit = &TabExit{At: time.Now()}
		assert.True(t, inst.TabDead(1))
	})
	t.Run("unstamped is never dead", func(t *testing.T) {
		// Even a definitively-absent session answers not-dead until the stamp
		// lands: absence is evidence only once a liveness probe PROVES it.
		livenessAnswers(t, map[string][2]bool{shellName: {false, true}})
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true})
		assert.False(t, inst.TabDead(1))
	})
	t.Run("agent and out-of-range", func(t *testing.T) {
		inst, _ := shellTabFixture(t, map[string]bool{agentName: true, shellName: true})
		assert.False(t, inst.TabDead(0))
		assert.False(t, inst.TabDead(-1))
		assert.False(t, inst.TabDead(9))
	})
}
