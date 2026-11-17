package proctree

import (
	"debug/buildinfo"
	"os"
	"sync"
	"syscall"
	"time"
)

// afModulePath is the Go module path a kernel-attested executable must carry
// in its build info to count as an agent-factory binary (#7491). Any build of
// this repo — the installed af, a source-built agent-factory, a test binary —
// carries it. This is a guard against RENAMING, not proof of identity: a
// foreign executable renamed to "af" fails because rename cannot rewrite the
// bytes, but the path is chosen at build time — a binary purpose-built with
// our module in its go.mod carries it too (#7494 item 3). That residual cost
// floor — building or borrowing a real af binary — is the one #7484's design
// already accepts; every cheaper forgery (rename, argv, comm) now blocks.
const afModulePath = "github.com/sachiniyer/agent-factory"

// exeAttestBudget bounds the whole attestation — the kernel exe read, the
// buildinfo parse, and the StartID revalidation. A verdict that authorizes
// cleanup must never wait on an unbounded filesystem (an exe hosted on a
// stalled NFS mount is a real shape); running out means UNVERIFIED, which is
// the fail-closed direction. Package var so tests can shrink the wait.
var exeAttestBudget = 2 * time.Second

// ExeAttestsAF reports whether p's kernel-attested executable is a build of
// this repo's agent-factory module — the af-daemon identity a process cannot
// write (#7491 item 1). argv alone still protects a claimant from signals;
// this is what a claim needs before it may also authorize a cleanup verdict:
//
//   - the executable is opened on its INODE, not re-resolved by path:
//     /proc/<pid>/exe opens the running image even after the name is
//     unlinked, and on darwin the PROC_PIDPATHINFO path is re-verified
//     against the text vnode's (dev,ino) in the process's own memory map —
//     a deleted or replaced name can never stand in for the running binary
//     (#7303's reopen-by-path class, #7492);
//   - the file opened that way is an af build iff its embedded Go build
//     info names this module — `cp /bin/bash ./af` passes an af-named path
//     and fails here, while a real af build verifies at ANY path,
//     including another home's daemon (the #7484 R18 shape). The module
//     path is chosen at build time, so this attests "built as the af
//     module", a bar a rename cannot reach — not "the af project built
//     it" (#7494 item 3);
//   - the read is revalidated against the snapshot's StartID: the exe read
//     happens outside the snapshot, so a pid recycled between them must not
//     carry one process's verdict onto the next;
//   - the whole check is budget-bounded: an expired wait reports false.
func ExeAttestsAF(p Process) bool {
	ok, _ := exeAttestsAFShared(p)
	return ok
}

// exeAttestsAFShared runs the attestation read under the in-flight set so
// concurrent attempts on the SAME process instance share one worker
// (#7494 item 2). It returns the verdict plus the (dev,ino) of the inode
// the verdict was read from, so a memoizing caller can re-check image
// identity before trusting a cached true (#7496).
func exeAttestsAFShared(p Process) (bool, exeID) {
	// Read the seam in the CALLER's goroutine before spawning the worker:
	// a worker that outlives its budget must never read a package var a
	// test later restores — the lazy read races the restore (the same
	// shape a racing caller would hit in production, #7491).
	fn := exeOpenFn
	// Attempts on the SAME process instance fly in formation (#7494 item
	// 2): a wedged exe — a hung NFS mount, a FIFO renamed over the exe
	// path — holds at most ONE worker, goroutine and fd no matter how many
	// candidates its pid reaches in one sweep or the next. Each caller
	// still bounds its own wait: an expired budget reports false and the
	// shared read keeps the worker it already paid.
	key := attestKey{pid: p.PID, startID: p.StartID}
	f, leader := attestFlights.begin(key)
	if leader {
		go func() {
			f.ok, f.id = exeAttestsAF(fn, p)
			attestFlights.finish(key, f)
		}()
	}
	timer := time.NewTimer(exeAttestBudget)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.ok, f.id
	case <-timer.C:
		return false, exeID{}
	}
}

// attestKey names a process instance for the in-flight set and the memo:
// the pid number plus the kernel's start-time identity, so a pid recycled
// into a new process can never share a flight or a verdict.
type attestKey struct {
	pid     int
	startID uint64
}

// exeID is the (dev, ino) of the executable image a verdict was read from.
// An exec keeps pid and StartID but swaps the image, so a memoized positive
// verdict is only honest while the kernel still reports this inode as the
// process's executable (#7496). dev is normalized to its low 32 bits — the
// width darwin's vnode_info reports — and both the attested file's stat and
// the revalidation read apply the same normalization on every platform, so
// the comparison is apples-to-apples.
type exeID struct {
	dev, ino uint64
}

// attestFlight is ONE shared attestation read. Every caller that meets the
// same (pid, StartID) while the read runs joins this exact record — one
// channel, one result — and a caller whose budget expires drops its select
// and leaves NOTHING behind: the flight holds constant memory no matter how
// many waiters attach or abandon (#7496 — x/sync's singleflight.DoChan
// allocated a channel PER WAITER and retained every abandoned one until the
// worker finished, which is the item-2 leak in another shape).
type attestFlight struct {
	done chan struct{}
	ok   bool
	id   exeID
	// chans counts the result channels this flight holds open: exactly
	// one — the shared done every waiter selects on. x/sync's DoChan
	// allocated one channel per waiter and retained each abandoned one
	// until the worker finished (#7496); the counter makes the constant-
	// memory invariant directly assertable by the test.
	chans int
}

// attestFlightSet tracks in-flight attestation reads by process instance.
// In-flight attempts share a result; a finished attempt is not cached here —
// cross-attempt dedup is the caller's ExeAttestations scope, so a
// wedged-mount verdict can recover on the next sweep.
type attestFlightSet struct {
	mu       sync.Mutex
	inFlight map[attestKey]*attestFlight
}

var attestFlights = attestFlightSet{inFlight: map[attestKey]*attestFlight{}}

// begin returns the flight for key, creating and leading it if none exists.
// A follower must never start a worker — the leader already paid for it.
func (s *attestFlightSet) begin(key attestKey) (f *attestFlight, leader bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.inFlight[key]; ok {
		return f, false
	}
	f = &attestFlight{done: make(chan struct{}), chans: 1}
	s.inFlight[key] = f
	return f, true
}

// finish publishes the leader's result to every waiter and drops the key so
// the next attempt starts a fresh flight.
func (s *attestFlightSet) finish(key attestKey, f *attestFlight) {
	s.mu.Lock()
	delete(s.inFlight, key)
	s.mu.Unlock()
	close(f.done)
}

// inFlightCount is the test seam for the memory bound: under a stalled read
// it must stay 1 however many waiters expire, and reach 0 once the read
// lands.
func (s *attestFlightSet) inFlightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inFlight)
}

// ExeAttestations memoizes ExeAttestsAF per (pid, StartID) for the scope of
// ONE sweep: a vanished-session reap classifies the same claimant about nine
// times per candidate (the ancestry walk and the candidate loop both meet
// it), and each attempt used to pay the full kernel read, buildinfo parse
// and StartID revalidation — with the full stall budget when the read
// wedges (#7494 item 2).
//
// A cached verdict is honest only for the image it was read from: an exec
// keeps pid and StartID but swaps the executable, so before returning a
// cached POSITIVE the memo re-reads the exe's (dev,ino) — one stat, not a
// kernel read — and re-attests when it no longer matches (#7496). A cached
// false needs no revalidation: the fail-closed answer is safe on any image.
// Scoping the memo to the sweep means a wedged-mount false is retried next
// sweep rather than cached forever. A nil *ExeAttestations attests directly,
// so callers without a sweep scope pay nothing extra.
type ExeAttestations struct {
	mu   sync.Mutex
	seen map[attestKey]attestEntry
}

type attestEntry struct {
	verdict bool
	exe     exeID
}

// NewExeAttestations returns the per-sweep attestation memo.
func NewExeAttestations() *ExeAttestations {
	return &ExeAttestations{seen: map[attestKey]attestEntry{}}
}

// AttestsAF is ExeAttestsAF deduplicated per (pid, StartID) for this scope,
// with a cached positive revalidated against the current executable image.
func (a *ExeAttestations) AttestsAF(p Process) bool {
	if a == nil {
		return ExeAttestsAF(p)
	}
	k := attestKey{pid: p.PID, startID: p.StartID}
	a.mu.Lock()
	e, ok := a.seen[k]
	a.mu.Unlock()
	if ok {
		if !e.verdict {
			return false
		}
		if id, known := exeIdentityFn(p.PID); known && id == e.exe {
			return true
		}
		// The image changed under the same instance, or its identity
		// cannot be confirmed: re-attest rather than ride the old
		// executable's verdict onto a different binary (#7496).
	}
	v, id := exeAttestsAFShared(p)
	a.mu.Lock()
	a.seen[k] = attestEntry{verdict: v, exe: id}
	a.mu.Unlock()
	return v
}

// exeIdentity is the memo's cheap revalidation read: the (dev,ino) the
// kernel currently reports as pid's executable — one stat against the
// /proc/<pid>/exe link on Linux, the PROC_PIDPATHINFO path stat'd on
// darwin. An exec swaps the image an attestation proved, so a cached
// positive verdict is only honest while the identity still matches
// (#7496). Behind a package var so the memo test can control the image
// sequence without staging an exec.
var exeIdentityFn = exeIdentity

// exeAttestsAF is the attestation itself, run under exeAttestBudget. It
// reports the (dev,ino) of the file it read alongside the verdict so the
// memo can re-validate a cached positive after an exec. exeOpenFn is the
// platform kernel read, behind a package var so the budget test can stall
// it deterministically; exeAttestsAFShared snapshots it before spawning so
// an abandoned read can never see a later write.
var exeOpenFn = exeOpen

func exeAttestsAF(fn func(pid int) (*os.File, bool), p Process) (bool, exeID) {
	f, ok := fn(p.PID)
	if !ok {
		return false, exeID{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false, exeID{}
	}
	var id exeID
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		id = exeID{dev: uint64(uint32(sys.Dev)), ino: sys.Ino}
	}
	bi, err := buildinfo.Read(f)
	if err != nil || bi.Main.Path != afModulePath {
		return false, exeID{}
	}
	// Revalidate the instance AFTER the read: the kernel answered for the
	// pid number, and the verdict is only honest for the instance the
	// snapshot saw — a recycled pid returns false like any other unverified
	// claim.
	cur, err := Lookup(p.PID)
	if err != nil || cur.StartID != p.StartID {
		return false, exeID{}
	}
	return true, id
}
