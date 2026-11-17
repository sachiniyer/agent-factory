package proctree

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
// argv; it can never write the module path baked into the linked binary.
// `yes -- --daemon` keeps the carrier alive: GNU yes exits on an
// unrecognized OPTION, so the `--` ends parsing and the discrete "--daemon"
// argument still lands in argv for the claim.
func TestExeAttestsAFRejectsAForeignBinaryUnderAForgedArgv(t *testing.T) {
	yesPath, err := exec.LookPath("yes")
	require.NoError(t, err)
	forged := exec.Command(yesPath, "--", "--daemon")
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
	oldPath, oldBudget := exePathFn, exeAttestBudget
	exePathFn = func(pid int) (string, bool) {
		select {} // a read that never answers
	}
	exeAttestBudget = 50 * time.Millisecond
	t.Cleanup(func() { exePathFn, exeAttestBudget = oldPath, oldBudget })

	start := time.Now()
	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	require.False(t, ExeAttestsAF(self), "an attestation that cannot answer inside the budget is unverified")
	require.Less(t, time.Since(start), 5*time.Second,
		"the budget, not the stalled read, ends the wait")
}
