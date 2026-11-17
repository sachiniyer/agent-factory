package proctree

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
