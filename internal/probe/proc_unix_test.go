//go:build !windows

package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/sachiniyer/agent-factory/cmd"
)

// The process-group lifecycle, exercised on real processes — the fake
// executor can prove the WIRING (SysProcAttr, Cancel, WaitDelay) but only a
// real tree proves a kill reaches a descendant the launcher spawned.

// sweepProcGroup ends a whole tree: a launcher that spawned a child has no
// members left once the sweep returns, and the group answers ESRCH.
func TestSweepProcGroup_EndsWholeTree(t *testing.T) {
	c := exec.Command("sh", "-c", "sleep 60 & exec sleep 60")
	setupProcGroup(c)
	require.NoError(t, c.Start())
	pgid := c.Process.Pid

	// The leader's reaping is the caller's job (execProbe runs c.Run); run
	// the sweep while Wait drains so the poll sees members die, not zombies.
	swept := make(chan struct{})
	go func() { sweepProcGroup(c); close(swept) }()
	_ = c.Wait()
	select {
	case <-swept:
	case <-time.After(sweepProcGroupBound + 3*time.Second):
		t.Fatal("sweepProcGroup did not return after the group died")
	}
	require.Equal(t, unix.ESRCH, unix.Kill(-pgid, 0),
		"no member of the probe's process group may outlive the sweep")
}

// cancelViaGroup is the deadline's first word: SIGINT to the whole group —
// a graceful interrupt the CLI can use to finish a truncate-then-write
// credential save, before os/exec's WaitDelay escalates to SIGKILL.
func TestCancelViaGroup_SignalsTheGroup(t *testing.T) {
	c := exec.Command("sleep", "60")
	setupProcGroup(c)
	require.NoError(t, c.Start())
	pgid := c.Process.Pid

	require.NoError(t, cancelViaGroup(c)())
	require.Error(t, c.Wait(), "SIGINT must reach the group leader")
	require.Equal(t, unix.ESRCH, unix.Kill(-pgid, 0),
		"a default-disposition child exits on the group's SIGINT")
}

// The auth.lock contract: a held lock refuses the probe (a live copy is
// refreshing); released, the probe takes it for its own run window; and the
// hold is exclusive — a second opener cannot acquire while af holds it.
func TestHoldAuthLock_ExcludesConcurrentRefreshers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.lock")
	require.NoError(t, os.WriteFile(path, []byte{}, 0o600))

	release, err := holdAuthLock(path)
	require.NoError(t, err)
	require.NotNil(t, release)

	// A second open+flock — the shape a live refresher takes — cannot
	// acquire while af holds it. (flock contends across open file
	// descriptions even inside one process.)
	_, err = holdAuthLock(path)
	require.Error(t, err, "a held auth.lock must refuse a second refresher")

	release()
	release2, err := holdAuthLock(path)
	require.NoError(t, err, "a released lock is acquirable again")
	release2()

	// A symlinked lock file is refused — the guard never follows links.
	link := filepath.Join(t.TempDir(), "auth.lock")
	require.NoError(t, os.Symlink(path, link))
	_, err = holdAuthLock(link)
	require.ErrorIs(t, err, errSymlinkedAuthLock)
}

// The probe holds auth.lock for the whole run, not just the auth.json read:
// inside the exec window a second refresher still cannot acquire.
func TestProbe_HoldsAuthLockAcrossTheRun(t *testing.T) {
	fx := &fakeExec{}
	r, home := newTestRunner(t, fx, []string{"PATH=/usr/bin"}, nil)
	path := filepath.Join(accountDir(t, home, "work"), "auth.lock")
	require.NoError(t, os.WriteFile(path, []byte{}, 0o600))
	var heldErr error
	fx.runFn = func(c *exec.Cmd) error {
		// execProbe is mid-run here — the lock must still be af's.
		_, heldErr = holdAuthLock(path)
		writeSuccess(c, "ok")
		return nil
	}

	p, ok := r.AccountProbe("codex")
	require.True(t, ok)
	require.Equal(t, KindHealthy, collect(t, p.Request("work")).Kind)
	require.Error(t, heldErr, "the probe must hold auth.lock for the whole run")

	// After the verdict the lock is free again.
	release, err := holdAuthLock(path)
	require.NoError(t, err)
	release()
}

// The full lifecycle end to end: a stub codex launcher that spawns a
// SIGINT-immune descendant, a hard timeout, and the proof that when the
// verdict arrives NOTHING the run spawned is still alive — the cap slot and
// the verdict both wait for the tree to die.
func TestProbe_TimeoutSweepsTheWholeTree(t *testing.T) {
	bin := t.TempDir()
	// The stub behaves like the npm codex launcher: it spawns a native
	// "worker" (sleep) that ignores SIGINT — the disposition a credential
	// save might briefly hold — then parks itself. Its child pid lands in
	// the account home so the test can name it after the run dir is gone.
	stub := `#!/bin/sh
trap '' INT TERM HUP
(trap '' INT; exec sleep 60) &
echo $! > "$CODEX_HOME/child.pid"
exec sleep 60
`
	require.NoError(t, os.WriteFile(filepath.Join(bin, "codex"), []byte(stub), 0o700))
	// argv[0] resolves through PATH — the child env carries it, and the
	// ambient one backs the construction-time lookup.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, home := newTestRunner(t, cmd.Exec{}, []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}, func(o *Options) { o.Timeout = 100 * time.Millisecond })
	p, ok := r.AccountProbe("codex")
	require.True(t, ok)

	v := collect(t, p.Request("work"))
	require.Equal(t, KindUnknown, v.Kind)
	require.Equal(t, ClassTimeout, v.Class)

	// The descendant ignored the graceful SIGINT; only the sweep's SIGKILL
	// could end it. The verdict already arrived — the sweep waited — so the
	// child must be gone NOW, not eventually.
	pidData, err := os.ReadFile(filepath.Join(accountDir(t, home, "work"), "child.pid"))
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	require.NoError(t, err)
	require.Equal(t, unix.ESRCH, unix.Kill(pid, 0),
		"the run's descendant must be dead before the verdict returns")
}
