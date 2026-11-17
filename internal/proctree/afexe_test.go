package proctree

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// The verdict afDaemonClass may place on rests on identity the carrier
// cannot write: this test binary is itself a real af-module build, so it
// attests at whatever path it was compiled to (#7491).
func TestExeAttestsAFVerifiesThisBinaryAtItsPath(t *testing.T) {
	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	require.True(t, ExeAttestsAF(self),
		"a real af-module build attests at any path — the property a foreign home's daemon needs")
}

// The forgery the verdict exists to refuse: argv[0]="af" plus --daemon on an
// executable the kernel attests is NOT an af build. `exec -a` can write the
// argv; it can never write the module path baked into the linked binary. The
// carrier is the test sleeper under the name "af", not a host binary: an
// Apple platform binary like /usr/bin/yes can have its kern.procargs2
// withheld even from a same-uid reader, so on darwin the claim it mounts can
// never be observed (#7492). The unsigned sleeper's argv reads on every host
// and its build info names no af module — foreign at any path.
func TestExeAttestsAFRejectsAForeignBinaryUnderAForgedArgv(t *testing.T) {
	carrier := sleeper.Named(t, filepath.Join(t.TempDir(), "af"))
	forged := exec.Command(carrier, "--daemon")
	forged.Args[0] = "af"
	require.NoError(t, forged.Start())
	t.Cleanup(func() { _ = forged.Process.Kill(); _, _ = forged.Process.Wait() })
	require.Eventually(t, func() bool {
		return ArgvIsAFDaemon(Argv(forged.Process.Pid))
	}, 5*time.Second, 20*time.Millisecond, "the forged argv must land before attestation is read")
	forgedProc, err := Lookup(forged.Process.Pid)
	require.NoError(t, err)
	require.False(t, ExeAttestsAF(forgedProc),
		"a foreign binary under an af name fails attestation — argv is writable, build info is not")
}

// exeSleeperEnv gates the reexec helper: a copy of this test binary exec'd
// under it sleeps instead of running the suite.
const exeSleeperEnv = "PROCTREE_TEST_EXE_SLEEPER"

// The reexec gate for exe-identity fixtures: the child runs ONLY this test,
// which sleeps rather than re-running the suite.
func TestExeSleeperProcess(t *testing.T) {
	if os.Getenv(exeSleeperEnv) != "1" {
		t.Skip("reexec helper — not a test")
	}
	time.Sleep(5 * time.Minute)
}

// copyFile copies path src to a fresh executable file dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	require.NoError(t, err)
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, out.Close())
	require.NoError(t, err)
}

// A REPLACED executable: the file at the exec'd path is swapped for a real
// af build after exec. The path now names an af binary, but the RUNNING
// process is still the foreign sleeper — a verdict read back off the path
// would launder the foreign process into "verified" (#7303's
// reopen-by-path class, Codex 4601463744). The answer must come from the
// running inode: unverified on every platform.
func TestExeAttestsAFRefusesAForeignProcessUnderAnAFPath(t *testing.T) {
	dir := t.TempDir()
	carrier := sleeper.Named(t, filepath.Join(dir, "af"))
	forged := exec.Command(carrier, "300")
	require.NoError(t, forged.Start())
	t.Cleanup(func() { _ = forged.Process.Kill(); _, _ = forged.Process.Wait() })
	forgedProc, err := Lookup(forged.Process.Pid)
	require.NoError(t, err)

	// Replace the carrier's name with a copy of THIS test binary — a real
	// af build — so the path attests where the process does not.
	afCopy := filepath.Join(dir, "af-copy")
	copyFile(t, os.Args[0], afCopy)
	require.NoError(t, os.Rename(afCopy, carrier))

	require.False(t, ExeAttestsAF(forgedProc),
		"the path holds an af build but the running inode is foreign — the verdict reads the inode, not the name")
}

// A DELETED executable: the file at the exec'd path is unlinked after exec.
// On Linux /proc/<pid>/exe still opens the running image — the name is gone
// but the build isn't, so a real af build still attests. On darwin there is
// no inode re-opener — a deleted exe is honestly unverifiable, the same
// fail-closed answer.
func TestExeAttestsAFStillVerifiesADeletedExecutableOnLinux(t *testing.T) {
	copied := filepath.Join(t.TempDir(), "sleeper-copy")
	copyFile(t, os.Args[0], copied)
	child := exec.Command(copied, "-test.run=^TestExeSleeperProcess$")
	child.Env = append(os.Environ(), exeSleeperEnv+"=1")
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	var childProc Process
	require.Eventually(t, func() bool {
		var err error
		childProc, err = Lookup(child.Process.Pid)
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the copied binary must be running before its name is unlinked")

	require.NoError(t, os.Remove(copied))
	if runtime.GOOS == "linux" {
		require.True(t, ExeAttestsAF(childProc),
			"/proc/<pid>/exe opens the running inode — unlinked does not mean foreign")
	} else {
		require.False(t, ExeAttestsAF(childProc),
			"without an inode re-opener a deleted exe is honestly unverifiable — fail-closed")
	}
}

// The instance check: the kernel answered for the pid NUMBER, and a pid
// recycled between the snapshot and the read must not carry one process's
// attestation onto another's verdict.
func TestExeAttestsAFRevalidatesTheInstance(t *testing.T) {
	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	require.False(t, ExeAttestsAF(Process{PID: os.Getpid(), StartID: self.StartID + 1}),
		"a StartID that is not the live instance's cannot carry the verdict")
	require.False(t, ExeAttestsAF(Process{PID: 4194304, StartID: 1}),
		"a pid that does not exist attests nothing")
}

// The bound: an exe read that cannot answer — a stalled filesystem is a real
// shape — expires as UNVERIFIED, the fail-closed direction, rather than
// holding the sweep.
func TestExeAttestsAFHonoursTheBudget(t *testing.T) {
	oldOpen, oldBudget := exeOpenFn, exeAttestBudget
	exeOpenFn = func(pid int) (*os.File, bool) {
		select {} // a read that never answers
	}
	exeAttestBudget = 50 * time.Millisecond
	t.Cleanup(func() { exeOpenFn, exeAttestBudget = oldOpen, oldBudget })

	start := time.Now()
	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	require.False(t, ExeAttestsAF(self), "an attestation that cannot answer inside the budget is unverified")
	require.Less(t, time.Since(start), 5*time.Second,
		"the budget, not the stalled read, ends the wait")
}

// The stall that used to leak a goroutine, thread and fd PER ATTEMPT holds
// ONE worker now: concurrent attestations of the same (pid, StartID) share
// a single in-flight read, and a caller whose budget expires while it runs
// — or that arrives after others expired — joins that worker rather than
// spawning another (#7494 item 2).
func TestExeAttestsAFSingleFlightsAStalledRead(t *testing.T) {
	oldOpen, oldBudget := exeOpenFn, exeAttestBudget
	var workers atomic.Int32
	gate := make(chan struct{})
	exeOpenFn = func(pid int) (*os.File, bool) {
		workers.Add(1)
		<-gate
		return nil, false
	}
	exeAttestBudget = 30 * time.Millisecond
	t.Cleanup(func() {
		close(gate)
		exeOpenFn, exeAttestBudget = oldOpen, oldBudget
	})

	p := Process{PID: 424242, StartID: 7}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ExeAttestsAF(p)
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), workers.Load(),
		"eight expired attestations of one stalled instance must share ONE worker")

	// A later attempt while the read is still wedged joins the same worker —
	// the flight, not another spawn, is what it waits on.
	ExeAttestsAF(p)
	require.Equal(t, int32(1), workers.Load())
}

// One sweep scope pays the kernel read once per claimant instance no matter
// how many candidate paths meet it (#7494 item 2). The memo keys the
// INSTANCE — a recycled pid under a new StartID attests anew — and a fresh
// scope retries what the last sweep could not verify, so a wedged-mount
// verdict never pins forever.
func TestExeAttestationsMemoizesPerSweep(t *testing.T) {
	oldOpen := exeOpenFn
	var opens atomic.Int32
	exeOpenFn = func(pid int) (*os.File, bool) {
		opens.Add(1)
		return nil, false
	}
	t.Cleanup(func() { exeOpenFn = oldOpen })

	p := Process{PID: 424243, StartID: 7}
	att := NewExeAttestations()
	for range 5 {
		require.False(t, att.AttestsAF(p))
	}
	require.Equal(t, int32(1), opens.Load(), "one scope attests an instance once")

	require.False(t, att.AttestsAF(Process{PID: p.PID, StartID: 8}),
		"the same pid under a new StartID is a new instance")
	require.Equal(t, int32(2), opens.Load())

	require.False(t, NewExeAttestations().AttestsAF(p),
		"a new sweep retries what the last one could not verify")
	require.Equal(t, int32(3), opens.Load())

	var unscoped *ExeAttestations
	require.False(t, unscoped.AttestsAF(p), "nil scope attests directly")
	require.Equal(t, int32(4), opens.Load())
}
