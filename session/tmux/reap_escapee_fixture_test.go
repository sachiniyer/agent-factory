package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

const markedEscapeeArg = "--tmux-test-marked-escapee"

// Run before TestMain's sandbox rewrites the inherited environment. Using the
// test binary supplies setsid on both Linux and macOS without a setsid(1)
// dependency. The pane shell must not publish $!: that only proves a fork,
// not that the child has exec'd with its markers or installed HUP protection.
func handleMarkedEscapeeExec() {
	if len(os.Args) < 3 || os.Args[1] != markedEscapeeArg {
		return
	}
	if err := runMarkedEscapee(os.Args[2], os.Args[3:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runMarkedEscapee(pidFile string, child []string) error {
	signal.Ignore(syscall.SIGHUP)
	if _, err := syscall.Setsid(); err != nil {
		return err
	}
	if err := os.WriteFile(pidFile+".tmp", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	if err := os.Rename(pidFile+".tmp", pidFile); err != nil {
		return err
	}
	if len(child) > 0 {
		// The re-hosted member's own child, inheriting this helper's marked
		// environment and leading its OWN kernel session — the #3699 shape's
		// bottom half: `claude` under `bg-pty-host`, each a session leader.
		// Its identity publishes to a sibling pid file so the fixture can
		// assert on it directly.
		rehost := exec.Command(child[0], child[1:]...)
		rehost.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := rehost.Start(); err != nil {
			return err
		}
		if err := os.WriteFile(pidFile+".child", []byte(strconv.Itoa(rehost.Process.Pid)), 0o600); err != nil {
			return err
		}
	}
	// Stay in this image after publishing readiness. An exec here races the
	// parent's environment read: Darwin can return EIO while replacing the
	// argument pages even though this marked helper remains alive (#3963).
	time.Sleep(300 * time.Second)
	return nil
}

// The SIGHUP-ignoring counterpart of vanishSessionWithObservableEscapee
// (#7435, Codex 4597265500): the session is destroyed but its root survives
// — still parented by the tmux server, which the sweep's boundary
// classification then meets walking the root's chain. Establish absence AND
// a live root whose parent is still a positively-identified tmux process
// before letting the sweep look: without the parent assertion a root that
// reparented to init would exercise no boundary at all.
func vanishSessionWithSurvivingRoot(t *testing.T, name string, root proctree.Process) {
	t.Helper()
	out, err := exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).CombinedOutput()
	require.NoError(t, err, "vanish original tmux session: %s", out)
	require.Eventually(t, func() bool {
		exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
		if probeErr != nil || !known || exists {
			return false
		}
		current, lookupErr := proctree.Lookup(root.PID)
		return lookupErr == nil && current.StartID == root.StartID &&
			proctree.TmuxIdentityOf(current.PPID) == proctree.Tmux
	}, 5*time.Second, 20*time.Millisecond,
		"SIGHUP-ignoring root %d must be alive and still parented by the live tmux server after session %s vanished",
		root.PID, name)
}

// Killing tmux is asynchronous with respect to pane exit and reparenting.
// Establish absence AND a live, reparented helper before letting a sweep look.
// Do not require PPID 1: a runner may use a child subreaper.
func vanishSessionWithObservableEscapee(t *testing.T, name string, helper proctree.Process) {
	t.Helper()
	out, err := exec.Command("tmux", "kill-session", "-t", ExactTarget(name)).CombinedOutput()
	require.NoError(t, err, "vanish original tmux session: %s", out)
	require.Eventually(t, func() bool {
		exists, known, probeErr := probeSessionStrict(cmd.MakeExecutor(), name)
		if probeErr != nil || !known || exists {
			return false
		}
		current, lookupErr := proctree.Lookup(helper.PID)
		return lookupErr == nil && current.StartID == helper.StartID &&
			current.SID == helper.PID && current.PPID != helper.PPID
	}, 5*time.Second, 20*time.Millisecond,
		"marked helper %d must be alive and reparented after tmux session %s vanished", helper.PID, name)
}

// spawnMarkedParentWithMarkedChild starts a detached process that stays
// alive beside a direct, setsid'd marked child, and returns both
// identities. The parent reparents to init through a dying intermediate sh,
// so the child's ancestry is child→parent→init — free of the reaper's
// subtree and every protected placement. The child is the test binary
// reexec'd as a marked escapee: it setsid's ITSELF (syscall.Setsid, which
// is what the marked-escapee harness uses precisely because setsid(1) does
// not exist on darwin) and publishes its own pid only after the new session
// exists, so its SID proves nothing and only DESCENT through the parent can
// admit it — the pure descent-proof carrier the decision-table rows need.
// childGen selects the child's AF_SESSION_GEN: nil leaves the marker absent
// entirely.
func spawnMarkedParentWithMarkedChild(t *testing.T, name, home string, childGen *string) (parent, child proctree.Process) {
	t.Helper()
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	env := "env " + EnvMarkerSession + "=" + name + " " + EnvMarkerHome + "=" + home
	if childGen != nil {
		env += " " + EnvMarkerGeneration + "=" + *childGen
	}
	// wait, not exec: the parent must stay a live ancestor while the sweep
	// reads the child's chain — an exec'd parent would exit under it.
	script := "#!/bin/sh\n" + env + " '" + os.Args[0] + "' '" + markedEscapeeArg +
		"' '" + childPidFile + "' &\nwait\n"
	scriptFile := filepath.Join(dir, "parent.sh")
	require.NoError(t, os.WriteFile(scriptFile, []byte(script), 0o700))
	require.NoError(t, exec.Command("sh", "-c",
		"'"+scriptFile+"' </dev/null >/dev/null 2>&1 &").Run())
	waitForPIDFile(t, childPidFile)
	// The escapee publishes its own pid only after Setsid() — the file IS the
	// readiness signal. The PARENT is the script process — recompute it from
	// the child's ppid, which is honest and needs no second pid file.
	child = processFromPIDFile(t, childPidFile)
	var lookupErr error
	parent, lookupErr = proctree.Lookup(child.PPID)
	require.NoError(t, lookupErr)
	require.Equal(t, child.PID, child.SID,
		"the child must lead its own kernel session — descent is its only membership proof")
	require.Equal(t, parent.PID, child.PPID,
		"the child must stay a ppid-descendant of the proven parent")
	require.NotEqual(t, os.Getpid(), parent.PPID,
		"the parent must be detached — inside the reaper's subtree the subtree verdict, not descent, would answer")
	t.Cleanup(func() {
		_ = proctree.Signal(parent, syscall.SIGKILL)
		_ = proctree.Signal(child, syscall.SIGKILL)
	})
	return parent, child
}

// spawnTmuxNamedParentWithMarkedChild starts a `tmux`-comm'd process — a
// /bin/sh exec'd through a tmux-named link, so BOTH the kernel task name and
// argv[0]'s basename answer TmuxIdentityOf's tmux — beside a direct setsid'd
// marked child. The child's chain is child→tmux→init: the synthetic
// "carrier behind a server" shape the boundary rows need, without a real
// server. The child is the test binary reexec'd as a marked escapee —
// syscall.Setsid is platform-portable where setsid(1) is not — and the
// `wait` keeps the sh process alive under the tmux comm: an exec'd tail
// command would retitle it.
func spawnTmuxNamedParentWithMarkedChild(t *testing.T, name, home, gen string) (fakeTmux, child proctree.Process) {
	t.Helper()
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	link := namedBinary(t, "tmux", "/bin/sh")
	inner := "env " + EnvMarkerSession + "=" + name + " " +
		EnvMarkerHome + "=" + home + " " + EnvMarkerGeneration + "=" + gen +
		" '" + os.Args[0] + "' '" + markedEscapeeArg + "' '" + childPidFile + "' &\nwait"
	require.NoError(t, exec.Command("sh", "-c",
		"'"+link+"' -c '"+inner+"' </dev/null >/dev/null 2>&1 &").Run())
	waitForPIDFile(t, childPidFile)
	child = processFromPIDFile(t, childPidFile)
	var lookupErr error
	fakeTmux, lookupErr = proctree.Lookup(child.PPID)
	require.NoError(t, lookupErr)
	require.Equal(t, proctree.Tmux, proctree.TmuxIdentityOf(fakeTmux.PID),
		"the fixture's parent must read as a tmux process or the boundary rows mean nothing")
	t.Cleanup(func() {
		_ = proctree.Signal(fakeTmux, syscall.SIGKILL)
		_ = proctree.Signal(child, syscall.SIGKILL)
	})
	return fakeTmux, child
}

// spawnProvenLeaderBehindTmuxParent starts a detached marked LEADER that
// stays alive below a `tmux`-comm'd intermediate which itself parents a
// setsid'd marked child: leader→tmux→child, the "different server" shape —
// the child's chain reaches a proven member only THROUGH a tmux process, so
// descent must NOT transfer the leader's membership to the child. The child
// is the marked-escapee reexec (syscall.Setsid — setsid(1) is absent on
// darwin) publishing its own pid after the new session exists.
func spawnProvenLeaderBehindTmuxParent(t *testing.T, name, home, gen string) (leader, fakeTmux, child proctree.Process) {
	t.Helper()
	dir := t.TempDir()
	childPidFile := filepath.Join(dir, "child.pid")
	tmuxPidFile := filepath.Join(dir, "tmux.pid")
	link := namedBinary(t, "tmux", "/bin/sh")
	inner := "env " + EnvMarkerSession + "=" + name + " " +
		EnvMarkerHome + "=" + home + " " + EnvMarkerGeneration + "=" + gen +
		" '" + os.Args[0] + "' '" + markedEscapeeArg + "' '" + childPidFile + "' &\nwait"
	leaderScript := "#!/bin/sh\n'" + link + "' -c '" + inner + "' &\n" +
		recordPIDShell("$!", tmuxPidFile) + "\nexec sleep 300\n"
	scriptFile := filepath.Join(dir, "leader.sh")
	require.NoError(t, os.WriteFile(scriptFile, []byte(leaderScript), 0o700))
	require.NoError(t, exec.Command("sh", "-c",
		"'"+scriptFile+"' </dev/null >/dev/null 2>&1 &").Run())
	waitForPIDFile(t, tmuxPidFile)
	waitForPIDFile(t, childPidFile)
	fakeTmux = processFromPIDFile(t, tmuxPidFile)
	child = processFromPIDFile(t, childPidFile)
	var lookupErr error
	leader, lookupErr = proctree.Lookup(fakeTmux.PPID)
	require.NoError(t, lookupErr)
	require.Equal(t, proctree.Tmux, proctree.TmuxIdentityOf(fakeTmux.PID),
		"the intermediate must read as tmux — it is the server the child sits behind")
	t.Cleanup(func() {
		_ = proctree.Signal(leader, syscall.SIGKILL)
		_ = proctree.Signal(fakeTmux, syscall.SIGKILL)
		_ = proctree.Signal(child, syscall.SIGKILL)
	})
	return leader, fakeTmux, child
}
