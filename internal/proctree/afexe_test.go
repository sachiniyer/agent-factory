package proctree

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
)

// The verdict afDaemonClass may place on rests on identity a bare claim
// cannot fake: this test binary is itself a real af-module build, so it
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
// holding the sweep. The stall is releasable AND its worker must finish in
// cleanup: the flight set is process-global, so a permanently stalled read
// would make every later attestation of this instance — the next -count
// iteration, any shuffled test — join the dead flight instead of using the
// restored opener (#7496).
func TestExeAttestsAFHonoursTheBudget(t *testing.T) {
	oldOpen, oldBudget := exeOpenFn, exeAttestBudget
	gate := make(chan struct{})
	exeOpenFn = func(pid int) (*os.File, bool) {
		<-gate // a read that answers only when released
		return nil, false
	}
	exeAttestBudget = 50 * time.Millisecond
	t.Cleanup(func() {
		close(gate)
		require.Eventually(t, func() bool {
			return attestFlights.inFlightCount() == 0
		}, 5*time.Second, 5*time.Millisecond,
			"the stalled flight's worker must finish in cleanup, not leak into later attestations")
		exeOpenFn, exeAttestBudget = oldOpen, oldBudget
	})

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
		require.Eventually(t, func() bool {
			return attestFlights.inFlightCount() == 0
		}, 5*time.Second, 5*time.Millisecond,
			"the stalled flight's worker must finish in cleanup, not leak into later attestations")
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

	// And the flight itself is all that remains: one shared record holding
	// one channel and one result, not one channel per abandoned waiter —
	// DoChan retained each timed-out caller until the worker finished, the
	// same leak in another shape (#7496). The bound is read off the flight
	// set's own structure — the map is the only place a flight can live —
	// not off a self-reported counter the code must remember to bump
	// (#7509): one entry, however many waiters expired.
	require.Equal(t, 1, attestFlights.inFlightCount(),
		"expired waiters must leave nothing behind — a stalled read holds one record per key")

	// And a waiter's own allocation stays flat under the stalled read: a
	// per-waiter channel is a heap allocation per call, so an allocation
	// bound that stays small across however many expired callers is the
	// structural pin the old self-counted f.chans could not give (#7509).
	allocs := testing.AllocsPerRun(32, func() { ExeAttestsAF(p) })
	require.Less(t, allocs, 8.0,
		"a wait on a stalled flight allocates a constant handful — no per-waiter record is retained")
	require.Equal(t, 1, attestFlights.inFlightCount())
	require.Equal(t, int32(1), workers.Load(),
		"the alloc probe joins the same flight — no new worker")
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

// An exec keeps pid and StartID but swaps the executable image: a claimant
// that attested as an af build and then exec'd a foreign binary must not
// carry the old verdict — StartID does not change on exec, so the memo
// re-checks the exe's (dev,ino) before trusting a cached positive (#7496).
// The identity seam answers for whatever file the open will present, so the
// memo sees the same image the attestation reads.
func TestExeAttestationsRevalidatesTheExecutableImage(t *testing.T) {
	oldOpen, oldID := exeOpenFn, exeIdentityFn
	var opens atomic.Int32
	var image atomic.Value
	image.Store(os.Args[0])
	exeOpenFn = func(pid int) (*os.File, bool) {
		opens.Add(1)
		f, err := os.Open(image.Load().(string))
		return f, err == nil
	}
	exeIdentityFn = func(pid int) (exeID, bool) {
		st, err := os.Stat(image.Load().(string))
		if err != nil {
			return exeID{}, false
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return exeID{}, false
		}
		return exeID{dev: uint64(uint32(sys.Dev)), ino: sys.Ino}, true
	}
	t.Cleanup(func() { exeOpenFn, exeIdentityFn = oldOpen, oldID })

	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	att := NewExeAttestations()
	require.True(t, att.AttestsAF(self), "this test build attests as af-module")
	require.True(t, att.AttestsAF(self), "the same image answers from the memo")
	require.Equal(t, int32(1), opens.Load())

	// The instance execs a foreign build: the exe identity changes, so the
	// cached positive must not ride the old image.
	image.Store(sleeper.Named(t, filepath.Join(t.TempDir(), "af")))
	require.False(t, att.AttestsAF(self),
		"a different image under the same (pid,StartID) re-attests — the old verdict cannot carry over")
	require.False(t, att.AttestsAF(self), "the new image's verdict memoizes too")
	require.Equal(t, int32(2), opens.Load(),
		"the image change forced exactly one re-attestation")
}

// The revalidation is part of the bounded operation: on darwin it reads
// exePath plus a stat on the executable's filesystem, so a stalled mount
// after a first positive attestation must expire as unverified — not hang
// the reap outside the timer (#7496). A synchronous revalidation would
// park on the gate forever; bounded, the call falls through to a
// re-attestation that is itself bounded.
func TestExeAttestationsRevalidationIsBounded(t *testing.T) {
	oldOpen, oldID, oldBudget := exeOpenFn, exeIdentityFn, exeAttestBudget
	gate := make(chan struct{})
	var opens atomic.Int32
	exeOpenFn = func(pid int) (*os.File, bool) {
		opens.Add(1)
		f, err := os.Open(os.Args[0])
		return f, err == nil
	}
	exeIdentityFn = func(pid int) (exeID, bool) {
		<-gate // a revalidation read that cannot answer
		return exeID{}, false
	}
	exeAttestBudget = 50 * time.Millisecond
	t.Cleanup(func() {
		close(gate)
		require.Eventually(t, func() bool {
			return attestFlights.inFlightCount() == 0
		}, 5*time.Second, 5*time.Millisecond,
			"the stalled flight's worker must finish in cleanup, not leak into later attestations")
		exeOpenFn, exeIdentityFn, exeAttestBudget = oldOpen, oldID, oldBudget
	})

	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	att := NewExeAttestations()
	require.True(t, att.AttestsAF(self), "the first attestation memoizes a positive")

	// The memo now holds a positive, and the identity seam is wedged: a
	// synchronous revalidation would never return. Bounded, it expires as
	// unverified — the fail-closed direction.
	done := make(chan bool, 1)
	go func() { done <- att.AttestsAF(self) }()
	select {
	case v := <-done:
		require.False(t, v, "a revalidation timeout reports unverified — fail-closed, not a hang")
	case <-time.After(10 * time.Second):
		t.Fatal("the identity revalidation hung — it must run inside the same bounded, single-flighted operation")
	}
}

// A revalidation timeout is memoized UNVERIFIED, not just reported
// (#7509): the wedged filesystem will not answer the next candidate
// either, and an unmemoized timeout would price the full budget once per
// candidate per pass. Cached false is the fail-closed direction on any
// image — later hits must not re-enter the read at all.
func TestExeAttestationsMemoizesARevalidationTimeout(t *testing.T) {
	oldOpen, oldID, oldBudget := exeOpenFn, exeIdentityFn, exeAttestBudget
	gate := make(chan struct{})
	var gateOnce sync.Once
	release := func() { gateOnce.Do(func() { close(gate) }) }
	var opens, identities atomic.Int32
	exeOpenFn = func(pid int) (*os.File, bool) {
		opens.Add(1)
		f, err := os.Open(os.Args[0])
		return f, err == nil
	}
	exeIdentityFn = func(pid int) (exeID, bool) {
		identities.Add(1)
		if identities.Load() == 1 {
			<-gate // only the FIRST revalidation read wedges
		}
		return exeID{}, false
	}
	exeAttestBudget = 50 * time.Millisecond
	t.Cleanup(func() {
		release()
		require.Eventually(t, func() bool {
			return attestFlights.inFlightCount() == 0
		}, 5*time.Second, 5*time.Millisecond,
			"the stalled flight's worker must finish in cleanup, not leak into later attestations")
		exeOpenFn, exeIdentityFn, exeAttestBudget = oldOpen, oldID, oldBudget
	})

	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	att := NewExeAttestations()
	require.True(t, att.AttestsAF(self), "the first attestation memoizes a positive")

	require.False(t, att.AttestsAF(self),
		"the wedged revalidation expires as unverified — and is then CACHED unverified")
	require.Equal(t, int32(1), identities.Load())

	// The pin needs the wedged flight to RETIRE: while it is still
	// stalled, later callers join the same flight and never re-invoke the
	// read, so a missing memoize cannot be told apart — the same vacuous
	// shape the f.chans counter was (#7509). Released, the next candidate
	// must answer from the memo alone: a fresh flight means the timeout
	// was never stored.
	release()
	require.Eventually(t, func() bool {
		return attestFlights.inFlightCount() == 0
	}, 5*time.Second, 5*time.Millisecond, "the wedged flight must retire before the next hit")
	require.False(t, att.AttestsAF(self))
	require.False(t, att.AttestsAF(self))
	require.Equal(t, int32(1), identities.Load(),
		"a memoized revalidation timeout never re-enters the read in this scope")
	require.Equal(t, int32(1), opens.Load(),
		"the memoized false is trusted — it never pays a re-attestation either")
}

// The revalidation read revalidates the INSTANCE too (#7509): the
// identity read happens outside the snapshot, so a pid recycled between
// the memo's key and the read must not carry the new process's exe
// identity onto the old instance's verdict — the same rule the full
// attestation already applies.
func TestExeAttestationsRevalidationRevalidatesTheInstance(t *testing.T) {
	oldID := exeIdentityFn
	exeIdentityFn = func(pid int) (exeID, bool) {
		return exeID{dev: 1, ino: 1}, true // the read "answers" for whatever pid it is given
	}
	t.Cleanup(func() { exeIdentityFn = oldID })

	_, ok := exeIdentityShared(Process{PID: 4194304, StartID: 1})
	require.False(t, ok,
		"a pid that no longer exists cannot revalidate — its answer belongs to a different process")

	self, err := Lookup(os.Getpid())
	require.NoError(t, err)
	_, ok = exeIdentityShared(Process{PID: os.Getpid(), StartID: self.StartID + 1})
	require.False(t, ok,
		"a stale StartID fails the instance check — the read answered for another instance")
	id, ok := exeIdentityShared(self)
	require.True(t, ok, "the live instance still revalidates")
	require.Equal(t, exeID{dev: 1, ino: 1}, id)
}
